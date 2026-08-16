package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		check   func(*testing.T, config)
	}{
		{
			name: "full contract with comments",
			in: `# leading comment
url: https://x.invalid/healthz
breach_after: 3m
recover_after: 5m
latency_budget: 2s   # trailing comment
availability_target: 0.99
`,
			check: func(t *testing.T, c config) {
				if c.URL != "https://x.invalid/healthz" {
					t.Errorf("url = %q", c.URL)
				}
				if c.BreachAfter != 3*time.Minute {
					t.Errorf("breach_after = %v", c.BreachAfter)
				}
				if c.LatencyBudget != 2*time.Second {
					t.Errorf("latency_budget = %v (trailing comment not stripped?)", c.LatencyBudget)
				}
				if c.AvailabilityTarget != 0.99 {
					t.Errorf("availability_target = %v", c.AvailabilityTarget)
				}
			},
		},
		{name: "url required", in: "breach_after: 1m", wantErr: true},
		{name: "unknown key is an error, not silence", in: "url: http://x\nnope: 1", wantErr: true},
		{name: "bad duration", in: "url: http://x\nbreach_after: soon", wantErr: true},
		{name: "not key: value", in: "url: http://x\ngarbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseConfig([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		delay    time.Duration
		budget   time.Duration
		wantKind string
	}{
		{name: "healthy", status: 200, body: `{"status":"ok","version":"v0.1.0"}`, wantKind: ""},
		{name: "non-200", status: 503, body: ``, wantKind: "unhealthy"},
		{name: "status not ok", status: 200, body: `{"status":"degraded"}`, wantKind: "unhealthy"},
		{name: "not json", status: 200, body: `<html>`, wantKind: "unhealthy"},
		{name: "over latency budget", status: 200, body: `{"status":"ok"}`, delay: 50 * time.Millisecond, budget: time.Millisecond, wantKind: "slow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			// A fake clock advances by tt.delay between the two now() calls in
			// probe, so latency is asserted deterministically.
			calls := 0
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time {
				calls++
				if calls == 1 {
					return base
				}
				return base.Add(tt.delay)
			}
			got := probe(config{URL: srv.URL, LatencyBudget: tt.budget}, now)
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q (%s), want %q", got.Kind, got.Detail, tt.wantKind)
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // closed: connection refused
	if got := probe(config{URL: srv.URL}, time.Now); got.Kind != "unreachable" {
		t.Errorf("kind = %q, want unreachable", got.Kind)
	}
}

// fakeGitHub records what the tool did to the issue tracker.
type fakeGitHub struct {
	open     []ghIssue
	created  []map[string]any
	comments int
	closed   []int
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues"):
			_ = json.NewEncoder(w).Encode(f.open)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			f.comments++
			io.WriteString(w, `{}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.created = append(f.created, in)
			io.WriteString(w, `{"number":42}`)
		case r.Method == "PATCH":
			f.closed = append(f.closed, 1)
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// healthzServer serves a sequence of responses, one per probe, so a test can
// describe "broken, then still broken" or "broken, then fine".
func healthzServer(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := bodies[min(n, len(bodies)-1)]
		n++
		if b == "" {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, b)
	}))
}

const okBody = `{"status":"ok","version":"v0.1.0"}`

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		bodies    []string // one per probe
		open      []ghIssue
		want      string
		wantFiled int
		wantClose int
	}{
		{
			name: "healthy with no open incident does nothing", bodies: []string{okBody},
			want: "healthy",
		},
		{
			name: "sustained breach files exactly one incident", bodies: []string{"", ""},
			want: "filed incident #42", wantFiled: 1,
		},
		{
			name: "blip that recovers within breach_after files nothing", bodies: []string{"", okBody},
			want: "blip",
		},
		{
			name:   "second run during the same outage does not duplicate",
			bodies: []string{"", ""},
			open:   []ghIssue{{Number: 7, Body: fingerprintMarker + " %URL%|unhealthy"}},
			want:   "ongoing: incident #7",
		},
		{
			name:   "recovery that holds closes the incident",
			bodies: []string{okBody, okBody},
			open:   []ghIssue{{Number: 7, Body: fingerprintMarker + " %URL%|unhealthy"}},
			want:   "recovered: closed incident #7", wantClose: 1,
		},
		{
			name:   "recovery that does not hold leaves it open",
			bodies: []string{okBody, ""},
			open:   []ghIssue{{Number: 7, Body: fingerprintMarker + " %URL%|unhealthy"}},
			want:   "flapping",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := healthzServer(t, tt.bodies...)
			defer target.Close()

			gh := &fakeGitHub{}
			for _, o := range tt.open {
				o.Body = strings.ReplaceAll(o.Body, "%URL%", target.URL)
				gh.open = append(gh.open, o)
			}
			api := gh.server(t)
			defer api.Close()

			cfg := config{URL: target.URL, BreachAfter: time.Hour, RecoverAfter: time.Hour}
			slept := 0
			got, err := run(client{api.URL, "o/r", ""}, cfg, time.Now, func(time.Duration) { slept++ })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("run() = %q, want prefix %q", got, tt.want)
			}
			if len(gh.created) != tt.wantFiled {
				t.Errorf("filed %d incidents, want %d", len(gh.created), tt.wantFiled)
			}
			if len(gh.closed) != tt.wantClose {
				t.Errorf("closed %d incidents, want %d", len(gh.closed), tt.wantClose)
			}
		})
	}
}

func TestFiledIncidentIsLabelledAndFingerprinted(t *testing.T) {
	target := healthzServer(t, "", "")
	defer target.Close()
	gh := &fakeGitHub{}
	api := gh.server(t)
	defer api.Close()

	_, err := run(client{api.URL, "o/r", ""}, config{URL: target.URL}, time.Now, func(time.Duration) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.created) != 1 {
		t.Fatalf("filed %d incidents, want 1", len(gh.created))
	}
	in := gh.created[0]
	labels, _ := in["labels"].([]any)
	if len(labels) != 1 || labels[0] != "incident" {
		t.Errorf("labels = %v, want [incident] — tools/dora reads this label", labels)
	}
	body, _ := in["body"].(string)
	if !strings.Contains(body, fingerprintMarker+" "+target.URL+"|unhealthy") {
		t.Errorf("body missing fingerprint, dedup will break:\n%s", body)
	}
}
