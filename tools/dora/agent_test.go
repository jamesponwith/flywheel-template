package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentBranch(t *testing.T) {
	for _, tt := range []struct {
		ref  string
		want bool
	}{
		{"bead/fw-oef.8", true},
		{"bead/abc-1", true},
		{"feature/thing", false},
		{"main", false},
		{"beads-rename", false}, // prefix must be the directory, not a substring
	} {
		if got := agentBranch(tt.ref); got != tt.want {
			t.Errorf("agentBranch(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestMedianInt(t *testing.T) {
	if got := medianInt(nil); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	if got := medianInt([]int{5, 1, 3}); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
}

// agentAPI serves a small but complete fixture: four merged PRs — two from
// bead/ branches (one clean, one reworked), two human, one with a red first
// check run.
func agentAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/pulls"):
			io.WriteString(w, `[
              {"number":1,"state":"closed","created_at":"2026-01-01T10:00:00Z","merged_at":"2026-01-02T00:00:00Z","head":{"ref":"bead/x-1","sha":"aaa"}},
              {"number":2,"state":"closed","created_at":"2026-01-01T10:00:00Z","merged_at":"2026-01-03T00:00:00Z","head":{"ref":"bead/x-2","sha":"bbb"}},
              {"number":3,"state":"closed","created_at":"2026-01-01T10:00:00Z","merged_at":"2026-01-04T00:00:00Z","head":{"ref":"feature/y","sha":"ccc"}},
              {"number":4,"state":"closed","created_at":"2026-01-01T10:00:00Z","merged_at":null,"head":{"ref":"feature/z","sha":"ddd"}}]`)
		case strings.Contains(p, "/commits/aaa/check-runs"):
			io.WriteString(w, `{"check_runs":[{"conclusion":"success","started_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.Contains(p, "/commits/bbb/check-runs"):
			io.WriteString(w, `{"check_runs":[{"conclusion":"failure","started_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.Contains(p, "/commits/ccc/check-runs"):
			io.WriteString(w, `{"check_runs":[{"conclusion":"success","started_at":"2026-01-01T00:00:00Z"},
                                             {"conclusion":"skipped","started_at":"2026-01-01T00:01:00Z"}]}`)
		case strings.HasSuffix(p, "/pulls/1/reviews"):
			io.WriteString(w, `[{"submitted_at":"2026-01-01T12:00:00Z"}]`)
		case strings.HasSuffix(p, "/pulls/1/commits"):
			// no commits after the review -> clean
			io.WriteString(w, `[{"commit":{"author":{"date":"2026-01-01T06:00:00Z"}}}]`)
		case strings.HasSuffix(p, "/pulls/2/reviews"):
			io.WriteString(w, `[{"submitted_at":"2026-01-01T12:00:00Z"},{"submitted_at":"2026-01-01T18:00:00Z"}]`)
		case strings.HasSuffix(p, "/pulls/2/commits"):
			// two commits after the first review -> reworked
			io.WriteString(w, `[{"commit":{"author":{"date":"2026-01-01T06:00:00Z"}}},
                                {"commit":{"author":{"date":"2026-01-01T13:00:00Z"}}},
                                {"commit":{"author":{"date":"2026-01-01T14:00:00Z"}}}]`)
		case strings.HasSuffix(p, "/pulls/3/reviews"):
			io.WriteString(w, `[{"submitted_at":"2026-01-01T12:00:00Z"}]`)
		case strings.HasSuffix(p, "/pulls/3/commits"):
			io.WriteString(w, `[{"commit":{"author":{"date":"2026-01-01T06:00:00Z"}}}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
}

func TestCollectAgent(t *testing.T) {
	srv := agentAPI(t)
	defer srv.Close()

	got, err := collectAgent(srv.URL, "o/r", 50)
	if err != nil {
		t.Fatal(err)
	}

	fp := got["final_gate_pass"].(map[string]any)
	if fp["total"] != 3 {
		t.Errorf("gate total = %v, want 3 (the unmerged PR is excluded)", fp["total"])
	}
	if fp["green"] != 2 {
		t.Errorf("gate green = %v, want 2 — skipped and neutral count as green", fp["green"])
	}

	auto := got["autonomous_prs"].(map[string]any)
	if auto["opened"] != 2 {
		t.Errorf("autonomous opened = %v, want 2 (bead/ branches only)", auto["opened"])
	}
	if auto["merged_without_further_commits"] != 1 {
		t.Errorf("clean = %v, want 1 — PR 2 got two commits after it opened", auto["merged_without_further_commits"])
	}

	rw := got["rework_commits_after_open"].(map[string]any)
	if rw["samples"] != 3 {
		t.Errorf("rework samples = %v, want 3 (one per merged PR)", rw["samples"])
	}
}

func TestCollectAgentSurvivesAnEmptyRepo(t *testing.T) {
	// A fresh template clone has no PRs at all and must still emit a valid
	// snapshot rather than failing the weekly Learn run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	got, err := collectAgent(srv.URL, "o/r", 50)
	if err != nil {
		t.Fatal(err)
	}
	fp := got["final_gate_pass"].(map[string]any)
	if fp["rate"] != 0.0 {
		t.Errorf("rate = %v, want 0 for an empty repo (not NaN)", fp["rate"])
	}
	if fmt.Sprint(fp["rate"]) == "NaN" {
		t.Error("division by zero produced NaN, which does not survive JSON encoding")
	}
}

// A metric that cannot mean anything in this workflow must say so in the data,
// not only in a comment. review_round_trips reading "median 0 across 34
// samples" was the worst artifact this project produced: it looked like
// excellence and measured nothing, because review runs locally and GitHub
// review events never exist here.
func TestUnmeasurableMetricsDeclareThemselves(t *testing.T) {
	srv := agentAPI(t)
	defer srv.Close()
	got, err := collectAgent(srv.URL, "o/r", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"review_round_trips", "rework_commits_after_open"} {
		m, ok := got[key].(map[string]any)
		if !ok {
			t.Fatalf("%s missing", key)
		}
		if m["measurable"] != false {
			t.Errorf("%s claims to be measurable; it is structurally pinned in this workflow", key)
		}
		if why, _ := m["why"].(string); why == "" {
			t.Errorf("%s is unmeasurable but does not say why", key)
		}
	}
	if _, ok := got["first_pass_gate"]; ok {
		t.Error("first_pass_gate is back: /check-runs returns filter=latest and cannot see a first attempt")
	}
}
