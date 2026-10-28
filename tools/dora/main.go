// dora: DORA-lite metrics from the GitHub API, no dependencies.
//   - deploy frequency: successful production deployments (total, last 30d,
//     per week); repos that record none fall back to published releases
//   - lead time: earliest commit since the previous deploy → this one going
//     live (or a release being published), median
//   - change-failure rate: changes that failed / finished deploy attempts (or
//     releases). A change failed if its deploy failed, or if an `incident`-labeled
//     issue opened while it was live; several incidents on one deploy count once.
//   - mttr: incident open → close, median
//
// Run weekly by learn.yml, which commits docs/dora.json back to the repo —
// the git history of that file IS the trend line.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"
)

type release struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
}

// A deployment is what GitHub records for a job with `environment: production`;
// its statuses say whether it went live.
type deployment struct {
	ID        int64     `json:"id"`
	Sha       string    `json:"sha"`
	CreatedAt time.Time `json:"created_at"`
}

type deploymentStatus struct {
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// deploy is a finished production deployment: live at At, or failed.
type deploy struct {
	Ref string // the deployed commit
	At  time.Time
	OK  bool
}

type issue struct {
	CreatedAt   time.Time  `json:"created_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	PullRequest *struct{}  `json:"pull_request"` // set when the "issue" is a PR — excluded
}

type compare struct {
	Commits []struct {
		Commit struct {
			Author struct {
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	} `json:"commits"`
}

func gh(api, path string, v any) error {
	req, err := http.NewRequest("GET", api+path, nil)
	if err != nil {
		return err
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func medianHours(ds []time.Duration) float64 {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[len(ds)/2].Hours()
}

// deploys is every finished production deployment, oldest first: live at its
// first success status, or failed. Ones still in progress don't count yet.
func deploys(api, repo string) ([]deploy, error) {
	var ds []deployment
	if err := gh(api, "/repos/"+repo+"/deployments?environment=production&per_page=100", &ds); err != nil {
		return nil, err
	}
	var out []deploy
	for _, d := range ds {
		var statuses []deploymentStatus
		if err := gh(api, fmt.Sprintf("/repos/%s/deployments/%d/statuses?per_page=100", repo, d.ID), &statuses); err != nil {
			return nil, err
		}
		dep := deploy{Ref: d.Sha}
		for _, st := range statuses { // newest first; a success later goes inactive
			switch st.State {
			case "success":
				dep.OK, dep.At = true, st.CreatedAt
			case "failure", "error":
				if !dep.OK {
					dep.At = st.CreatedAt
				}
			}
		}
		if !dep.At.IsZero() {
			out = append(out, dep)
		}
	}
	slices.SortFunc(out, func(a, b deploy) int { return a.At.Compare(b.At) })
	return out, nil
}

func collect(api, repo string, now time.Time) (map[string]any, error) {
	all, err := deploys(api, repo)
	if err != nil {
		return nil, err
	}
	source := "deployments"
	var live []deploy
	failed := 0
	for _, d := range all {
		if d.OK {
			live = append(live, d)
		} else {
			failed++
		}
	}
	if len(all) == 0 { // no deployments recorded: releases stand in for deploys
		source = "releases"
		var releases []release
		if err := gh(api, "/repos/"+repo+"/releases?per_page=20", &releases); err != nil {
			return nil, err
		}
		for _, r := range releases {
			live = append(live, deploy{Ref: r.TagName, At: r.PublishedAt, OK: true})
		}
		slices.SortFunc(live, func(a, b deploy) int { return a.At.Compare(b.At) })
	}

	last30 := 0
	for _, d := range live {
		if now.Sub(d.At) <= 30*24*time.Hour {
			last30++
		}
	}
	perWeek := 0.0
	if len(live) > 1 {
		span := live[len(live)-1].At.Sub(live[0].At)
		perWeek = float64(len(live)-1) / (span.Hours() / (24 * 7))
	}

	// lead time: earliest commit between the previous deploy and this one
	var leads []time.Duration
	for i := 1; i < len(live); i++ {
		var cmp compare
		if err := gh(api, "/repos/"+repo+"/compare/"+live[i-1].Ref+"..."+live[i].Ref, &cmp); err != nil {
			return nil, err
		}
		var earliest time.Time
		for _, c := range cmp.Commits {
			if earliest.IsZero() || c.Commit.Author.Date.Before(earliest) {
				earliest = c.Commit.Author.Date
			}
		}
		if !earliest.IsZero() {
			leads = append(leads, live[i].At.Sub(earliest))
		}
	}

	var incidents []issue
	if err := gh(api, "/repos/"+repo+"/issues?labels=incident&state=all&per_page=100", &incidents); err != nil {
		return nil, err
	}
	incidents = slices.DeleteFunc(incidents, func(i issue) bool { return i.PullRequest != nil })
	var repairs []time.Duration
	broke := map[int]bool{} // index into live of each deploy an incident is blamed on
	for _, i := range incidents {
		if i.ClosedAt != nil {
			repairs = append(repairs, i.ClosedAt.Sub(i.CreatedAt))
		}
		// ponytail: blames whichever deploy was live when the incident opened;
		// match on the incident body's `version:` line if deploys start overlapping.
		k := -1
		for j, d := range live {
			if !d.At.After(i.CreatedAt) {
				k = j
			}
		}
		if k >= 0 { // an incident before the first deploy has no change to blame
			broke[k] = true
		}
	}
	attempts := len(live) + failed
	failedChanges := len(broke) + failed
	cfr := 0.0
	if attempts > 0 {
		cfr = float64(failedChanges) / float64(attempts)
	}

	return map[string]any{
		"repo":         repo,
		"generated_at": now.UTC().Format(time.RFC3339),
		// releases_* and releases mirror the generic keys for dashboards that
		// predate deployments.
		"deploy_frequency": map[string]any{
			"source": source, "total": len(live), "last_30d": last30, "per_week": perWeek,
			"releases_total": len(live), "releases_last_30d": last30,
		},
		"lead_time_hours": map[string]any{"median": medianHours(leads), "samples": len(leads)},
		"change_failure_rate": map[string]any{
			"incidents": len(incidents), "failed_deploys": failed, "failed_changes": failedChanges,
			"attempts": attempts, "rate": cfr,
			"releases": attempts,
		},
		"mttr_hours": map[string]any{"median": medianHours(repairs), "samples": len(repairs)},
	}, nil
}

func main() {
	repo := flag.String("repo", "", "owner/name (required)")
	out := flag.String("out", "docs/dora.json", "output path")
	api := flag.String("api", "https://api.github.com", "GitHub API base")
	prLimit := flag.Int("prs", 50, "how many recent closed PRs to sample for agent metrics")
	flag.Parse()
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "-repo required")
		os.Exit(2)
	}
	m, err := collect(*api, *repo, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Agent metrics are additive: a failure here must not cost the DORA
	// snapshot, which is the one thing this tool has always produced.
	if a, err := collectAgent(*api, *repo, *prLimit); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent metrics unavailable: %v\n", err)
	} else {
		m["agent"] = a
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}
