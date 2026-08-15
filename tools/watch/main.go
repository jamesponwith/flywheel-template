// watch: the Operate stage's prober (ADR 0002).
//
// Reads docs/slo.yml, probes the target's /healthz, and turns sustained
// breaches into `incident`-labelled GitHub issues — the same label tools/dora
// already reads for change-failure rate and MTTR. Recovery closes the issue,
// which is what makes MTTR a measured number rather than a measure of how fast
// someone remembered to click Close.
//
// Dedup is by fingerprint: one open incident per (target, breach kind). A
// two-hour outage is one incident, not one per probe.
//
// Run from Operate's schedule; state lives in the issue tracker, not on disk,
// so it is safe to run from a cron, a workflow, or the host itself.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// fingerprintMarker is embedded in every incident body so a later run can find
// its own open incidents without depending on title formatting.
const fingerprintMarker = "flywheel-watch-fingerprint:"

type config struct {
	URL                string
	BreachAfter        time.Duration
	RecoverAfter       time.Duration
	LatencyBudget      time.Duration
	AvailabilityTarget float64
}

// parseConfig reads the flat `key: value` subset of YAML that slo.yml uses.
//
// ponytail: flat scalars only — no nesting, lists, quotes, or multi-line
// values. That covers every field the Operate contract has. If slo.yml ever
// needs structure, adopt gopkg.in/yaml.v3 behind an ADR rather than growing
// this; the template's whole point is that it has no dependencies.
func parseConfig(b []byte) (config, error) {
	var c config
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return c, fmt.Errorf("slo.yml:%d: not key: value", n+1)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		var err error
		switch key {
		case "url":
			c.URL = val
		case "breach_after":
			c.BreachAfter, err = time.ParseDuration(val)
		case "recover_after":
			c.RecoverAfter, err = time.ParseDuration(val)
		case "latency_budget":
			c.LatencyBudget, err = time.ParseDuration(val)
		case "availability_target":
			c.AvailabilityTarget, err = strconv.ParseFloat(val, 64)
		default:
			return c, fmt.Errorf("slo.yml:%d: unknown key %q", n+1, key)
		}
		if err != nil {
			return c, fmt.Errorf("slo.yml:%d: %s: %w", n+1, key, err)
		}
	}
	if c.URL == "" {
		return c, fmt.Errorf("slo.yml: url is required")
	}
	return c, nil
}

type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// probeResult is one observation. kind is "" when healthy, otherwise the breach
// kind, which is half the incident fingerprint.
type probeResult struct {
	Kind    string
	Detail  string
	Version string
}

func probe(c config, now func() time.Time) probeResult {
	start := now()
	resp, err := http.Get(c.URL)
	if err != nil {
		return probeResult{Kind: "unreachable", Detail: err.Error()}
	}
	defer resp.Body.Close()
	elapsed := now().Sub(start)

	if resp.StatusCode != http.StatusOK {
		return probeResult{Kind: "unhealthy", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return probeResult{Kind: "unhealthy", Detail: "healthz is not valid JSON"}
	}
	if h.Status != "ok" {
		return probeResult{Kind: "unhealthy", Detail: "status=" + h.Status, Version: h.Version}
	}
	// Latency is checked last so a slow-but-correct target reports the more
	// specific breach rather than masking a real failure.
	if c.LatencyBudget > 0 && elapsed > c.LatencyBudget {
		return probeResult{Kind: "slow", Detail: fmt.Sprintf("%s > %s", elapsed.Round(time.Millisecond), c.LatencyBudget), Version: h.Version}
	}
	return probeResult{Version: h.Version}
}

type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type client struct {
	api, repo, token string
}

func (c client) do(method, path string, in, out any) error {
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.api+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fingerprint(url, kind string) string { return url + "|" + kind }

// openIncident returns the open incident matching fp, or nil.
func openIncident(c client, fp string) (*ghIssue, error) {
	var issues []ghIssue
	if err := c.do("GET", "/repos/"+c.repo+"/issues?labels=incident&state=open&per_page=100", nil, &issues); err != nil {
		return nil, err
	}
	want := fingerprintMarker + " " + fp
	for i := range issues {
		if strings.Contains(issues[i].Body, want) {
			return &issues[i], nil
		}
	}
	return nil, nil
}

func fileIncident(c client, fp string, r probeResult, first time.Time) (int, error) {
	body := fmt.Sprintf(`The Operate prober saw **%s** continuously since %s.

- target: %s
- detail: %s
- version: %s

Filed automatically by `+"`tools/watch`"+` (ADR 0002). It will close itself when
the target recovers and holds; the open→close span is what MTTR measures.

%s %s`, r.Kind, first.UTC().Format(time.RFC3339), c.repo, r.Detail, versionOr(r.Version), fingerprintMarker, fp)

	var created ghIssue
	err := c.do("POST", "/repos/"+c.repo+"/issues", map[string]any{
		"title":  fmt.Sprintf("incident: %s (%s)", r.Kind, r.Detail),
		"body":   body,
		"labels": []string{"incident"},
	}, &created)
	return created.Number, err
}

func closeIncident(c client, n int, recovered time.Time) error {
	if err := c.do("POST", fmt.Sprintf("/repos/%s/issues/%d/comments", c.repo, n), map[string]any{
		"body": "Recovered and held at " + recovered.UTC().Format(time.RFC3339) + "; closing automatically.",
	}, nil); err != nil {
		return err
	}
	return c.do("PATCH", fmt.Sprintf("/repos/%s/issues/%d", c.repo, n), map[string]any{"state": "closed"}, nil)
}

func versionOr(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// run performs one probe cycle. It is the whole tool: everything else is I/O.
//
// Sustain windows are enforced by re-probing rather than by remembering across
// invocations, so the tool stays stateless and safe to run from anywhere.
func run(c client, cfg config, now func() time.Time, sleep func(time.Duration)) (string, error) {
	r := probe(cfg, now)

	if r.Kind == "" {
		// Healthy. Close any open incident that holds through recover_after.
		inc, err := openIncident(c, fingerprint(cfg.URL, "unreachable"))
		if err != nil {
			return "", err
		}
		for _, kind := range []string{"unhealthy", "slow"} {
			if inc != nil {
				break
			}
			if inc, err = openIncident(c, fingerprint(cfg.URL, kind)); err != nil {
				return "", err
			}
		}
		if inc == nil {
			return "healthy", nil
		}
		sleep(cfg.RecoverAfter)
		if again := probe(cfg, now); again.Kind != "" {
			return "flapping: recovery did not hold, incident #" + strconv.Itoa(inc.Number) + " stays open", nil
		}
		if err := closeIncident(c, inc.Number, now()); err != nil {
			return "", err
		}
		return "recovered: closed incident #" + strconv.Itoa(inc.Number), nil
	}

	// Breaching. Only file if it persists through breach_after.
	first := now()
	sleep(cfg.BreachAfter)
	if again := probe(cfg, now); again.Kind == "" {
		return "blip: " + r.Kind + " did not persist, no incident filed", nil
	}
	fp := fingerprint(cfg.URL, r.Kind)
	inc, err := openIncident(c, fp)
	if err != nil {
		return "", err
	}
	if inc != nil {
		return "ongoing: incident #" + strconv.Itoa(inc.Number) + " already open", nil
	}
	n, err := fileIncident(c, fp, r, first)
	if err != nil {
		return "", err
	}
	return "filed incident #" + strconv.Itoa(n) + " (" + r.Kind + ")", nil
}

func main() {
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name (required)")
	slo := flag.String("slo", "docs/slo.yml", "path to the SLO contract")
	api := flag.String("api", "https://api.github.com", "GitHub API base")
	flag.Parse()
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "-repo required (or set GITHUB_REPOSITORY)")
		os.Exit(2)
	}
	b, err := os.ReadFile(*slo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := parseConfig(b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	msg, err := run(client{*api, *repo, os.Getenv("GITHUB_TOKEN")}, cfg, time.Now, time.Sleep)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(msg)
}
