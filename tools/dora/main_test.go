package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollect(t *testing.T) {
	// two releases a week apart; v2's earliest commit 48h before publish;
	// two incidents (one closed after 6h, one open) and one PR mislabeled
	// incident that must be excluded.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/deployments": // none recorded, as GitHub answers
			io.WriteString(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/releases"):
			io.WriteString(w, `[
			  {"tag_name":"v0.2.0","published_at":"2026-01-08T12:00:00Z"},
			  {"tag_name":"v0.1.0","published_at":"2026-01-01T12:00:00Z"}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/v0.1.0...v0.2.0"):
			io.WriteString(w, `{"commits":[
			  {"commit":{"author":{"date":"2026-01-06T12:00:00Z"}}},
			  {"commit":{"author":{"date":"2026-01-07T12:00:00Z"}}}]}`)
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/issues"):
			io.WriteString(w, `[
			  {"created_at":"2026-01-02T00:00:00Z","closed_at":"2026-01-02T06:00:00Z"},
			  {"created_at":"2026-01-03T00:00:00Z","closed_at":null},
			  {"created_at":"2026-01-04T00:00:00Z","closed_at":"2026-01-04T01:00:00Z","pull_request":{}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	m, err := collect(fake.URL, "o/r", now)
	if err != nil {
		t.Fatal(err)
	}

	df := m["deploy_frequency"].(map[string]any)
	if df["source"] != "releases" {
		t.Errorf("source = %v, want releases: with no deployments recorded, releases stand in", df["source"])
	}
	if df["releases_total"].(int) != 2 || df["releases_last_30d"].(int) != 2 {
		t.Errorf("deploy_frequency = %+v, want 2 total, 2 in 30d", df)
	}
	if pw := df["per_week"].(float64); pw < 0.99 || pw > 1.01 {
		t.Errorf("per_week = %v, want ~1 (two releases, 7 days apart)", pw)
	}
	lt := m["lead_time_hours"].(map[string]any)
	if lt["samples"].(int) != 1 || lt["median"].(float64) != 48 {
		t.Errorf("lead_time = %+v, want median 48h from 1 sample", lt)
	}
	cf := m["change_failure_rate"].(map[string]any)
	if cf["incidents"].(int) != 2 || cf["rate"].(float64) != 1.0 {
		t.Errorf("cfr = %+v, want 2 incidents (PR excluded), rate 1.0", cf)
	}
	mt := m["mttr_hours"].(map[string]any)
	if mt["samples"].(int) != 1 || mt["median"].(float64) != 6 {
		t.Errorf("mttr = %+v, want median 6h from the 1 closed incident", mt)
	}
}

// With production deployments recorded, deploy metrics come from them, not
// from releases: a release whose deploy failed isn't a deploy, and a failed
// deploy is a change failure.
func TestCollectFromDeployments(t *testing.T) {
	// v0.1.0's deploy failed; v0.1.1 deployed 30m after its earliest commit; v0.2.0
	// deployed a week later, 24h after its earliest commit. One incident, closed.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/repos/o/r/releases"):
			io.WriteString(w, `[{"tag_name":"v0.2.0","published_at":"2026-01-08T11:00:00Z"},
			  {"tag_name":"v0.1.1","published_at":"2026-01-01T11:30:00Z"},
			  {"tag_name":"v0.1.0","published_at":"2026-01-01T10:00:00Z"}]`)
		case p == "/repos/o/r/deployments":
			if r.URL.Query().Get("environment") != "production" {
				t.Errorf("deployments queried for %q, want production", r.URL.Query().Get("environment"))
			}
			io.WriteString(w, `[{"id":3,"sha":"c3","created_at":"2026-01-08T11:05:00Z"},
			  {"id":2,"sha":"c2","created_at":"2026-01-01T11:35:00Z"},
			  {"id":1,"sha":"c1","created_at":"2026-01-01T10:05:00Z"},
			  {"id":9,"sha":"c9","created_at":"2026-01-09T00:00:00Z"}]`)
		case p == "/repos/o/r/deployments/3/statuses":
			io.WriteString(w, `[{"state":"success","created_at":"2026-01-08T12:00:00Z"},{"state":"in_progress","created_at":"2026-01-08T11:06:00Z"}]`)
		case p == "/repos/o/r/deployments/2/statuses":
			io.WriteString(w, `[{"state":"inactive","created_at":"2026-01-08T12:00:00Z"},{"state":"success","created_at":"2026-01-01T12:00:00Z"}]`)
		case p == "/repos/o/r/deployments/1/statuses":
			io.WriteString(w, `[{"state":"failure","created_at":"2026-01-01T10:10:00Z"}]`)
		case p == "/repos/o/r/deployments/9/statuses": // still running: not counted yet
			io.WriteString(w, `[{"state":"in_progress","created_at":"2026-01-09T00:01:00Z"}]`)
		case strings.HasPrefix(p, "/repos/o/r/compare/c2...c3"):
			io.WriteString(w, `{"commits":[{"commit":{"author":{"date":"2026-01-07T12:00:00Z"}}}]}`)
		case strings.HasPrefix(p, "/repos/o/r/issues"):
			io.WriteString(w, `[{"created_at":"2026-01-05T00:00:00Z","closed_at":"2026-01-05T02:00:00Z"}]`)
		default:
			t.Errorf("unexpected GET %s", p)
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	m, err := collect(fake.URL, "o/r", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	df := m["deploy_frequency"].(map[string]any)
	if df["source"] != "deployments" || df["total"].(int) != 2 || df["last_30d"].(int) != 2 {
		t.Errorf("deploy_frequency = %+v, want 2 successful deployments (the failed and running ones aren't)", df)
	}
	if pw := df["per_week"].(float64); pw < 0.99 || pw > 1.01 {
		t.Errorf("per_week = %v, want ~1 (two deploys, 7 days apart)", pw)
	}
	if df["releases_total"] != df["total"] {
		t.Errorf("releases_total = %v, want it to mirror total for older dashboards", df["releases_total"])
	}
	lt := m["lead_time_hours"].(map[string]any)
	if lt["samples"].(int) != 1 || lt["median"].(float64) != 24 {
		t.Errorf("lead_time = %+v, want 24h: the earliest commit since the last good deploy, to going live", lt)
	}
	cf := m["change_failure_rate"].(map[string]any)
	if cf["incidents"].(int) != 1 || cf["failed_deploys"].(int) != 1 || cf["attempts"].(int) != 3 {
		t.Errorf("cfr = %+v, want 1 incident and 1 failed deploy over 3 finished attempts", cf)
	}
	if r := cf["rate"].(float64); r < 0.66 || r > 0.67 {
		t.Errorf("cfr rate = %v, want 2/3", r)
	}
}

// A deployment whose job failed and was then re-run to success went live: its
// newest status wins over the older failure.
func TestDeployRetriedToSuccess(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/deployments":
			io.WriteString(w, `[{"id":1,"sha":"c1","created_at":"2026-01-01T10:00:00Z"}]`)
		case "/repos/o/r/deployments/1/statuses": // newest first
			io.WriteString(w, `[{"state":"success","created_at":"2026-01-01T11:00:00Z"},
			  {"state":"failure","created_at":"2026-01-01T10:10:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	ds, err := deploys(fake.URL, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || !ds[0].OK || !ds[0].At.Equal(time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("deploys = %+v, want one live at 11:00 (the re-run's success)", ds)
	}
}
