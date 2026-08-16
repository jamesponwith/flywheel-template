// Agent-effectiveness metrics: how the work got done, not just what shipped.
//
// DORA measures the software. Nothing here measures the thing that is actually
// novel — that an agent wrote most of it. These four are derivable from the
// GitHub API alone, so they backfill across a repo's whole history rather than
// starting to accumulate today:
//
//   - first-pass gate rate: PRs whose first CI run was green
//   - rework:               commits pushed after the first review, median
//   - review round-trips:   review submissions per PR, median
//   - autonomous share:     PRs opened from bead/* branches, and how many
//     merged without a human pushing to them afterwards
//
// Cost per PR is deliberately absent: it needs the agent audit log, not the
// API, and a number this repo cannot verify has no business on a public
// dashboard.
package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

type pull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Head      struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type checkRuns struct {
	Runs []struct {
		Conclusion string    `json:"conclusion"`
		StartedAt  time.Time `json:"started_at"`
	} `json:"check_runs"`
}

type prCommit struct {
	Commit struct {
		Author struct {
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

type prReview struct {
	SubmittedAt time.Time `json:"submitted_at"`
}

// agentBranch reports whether a PR came from the autonomous loop. The
// flywheel-next skill always branches as bead/<id>, which makes the convention
// the measurement.
func agentBranch(ref string) bool { return strings.HasPrefix(ref, "bead/") }

// collectAgent walks recent PRs. It is deliberately tolerant: a repo with no
// merged PRs, no reviews, or no check runs yields zero-sample metrics rather
// than an error, because a fresh template clone must still produce a valid
// dora.json.
func collectAgent(api, repo string, limit int) (map[string]any, error) {
	var pulls []pull
	if err := gh(api, fmt.Sprintf("/repos/%s/pulls?state=closed&per_page=%d", repo, limit), &pulls); err != nil {
		return nil, err
	}

	var (
		firstPassGreen, firstPassTotal int
		autonomous, autonomousClean    int
		reworkCounts, tripCounts       []int
	)

	for _, p := range pulls {
		if p.MergedAt == nil {
			continue // unmerged PRs say nothing about whether the gate passed
		}

		// First-pass gate: was the earliest check run on the head SHA green?
		var cr checkRuns
		if err := gh(api, fmt.Sprintf("/repos/%s/commits/%s/check-runs", repo, p.Head.SHA), &cr); err == nil && len(cr.Runs) > 0 {
			slices.SortFunc(cr.Runs, func(a, b struct {
				Conclusion string    `json:"conclusion"`
				StartedAt  time.Time `json:"started_at"`
			}) int {
				return a.StartedAt.Compare(b.StartedAt)
			})
			firstPassTotal++
			green := true
			for _, run := range cr.Runs {
				if run.Conclusion != "success" && run.Conclusion != "neutral" && run.Conclusion != "skipped" {
					green = false
					break
				}
			}
			if green {
				firstPassGreen++
			}
		}

		var reviews []prReview
		_ = gh(api, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, p.Number), &reviews)
		var commits []prCommit
		_ = gh(api, fmt.Sprintf("/repos/%s/pulls/%d/commits?per_page=100", repo, p.Number), &commits)
		tripCounts = append(tripCounts, len(reviews))

		// Rework: commits landing after the PR was opened. Anchoring to the PR
		// rather than to a review is what makes this work when review happens
		// on the laptop.
		after := 0
		for _, c := range commits {
			if c.Commit.Author.Date.After(p.CreatedAt) {
				after++
			}
		}
		reworkCounts = append(reworkCounts, after)

		if agentBranch(p.Head.Ref) {
			autonomous++
			// "Clean" = merged with nothing pushed after it was opened, i.e.
			// nobody had to fix it. This is the number that decides whether the
			// fleet's caps can rise (ADR 0006).
			if after == 0 {
				autonomousClean++
			}
		}
	}
	rate := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}

	return map[string]any{
		"first_pass_gate": map[string]any{
			"green": firstPassGreen, "total": firstPassTotal, "rate": rate(firstPassGreen, firstPassTotal),
		},
		"rework_commits_after_open": map[string]any{
			"median": medianInt(reworkCounts), "samples": len(reworkCounts),
		},
		"review_round_trips": map[string]any{
			"median": medianInt(tripCounts), "samples": len(tripCounts),
		},
		"autonomous_prs": map[string]any{
			"opened": autonomous, "merged_without_human_edits": autonomousClean,
			"clean_rate": rate(autonomousClean, autonomous),
		},
	}, nil
}

func medianInt(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	slices.Sort(xs)
	return float64(xs[len(xs)/2])
}
