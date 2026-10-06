# flywheel-template

Template repo for Go projects built inside a personal agentic flywheel:
**Intent → Build → Validate → Release → Learn**, where AI agents do the work
and every stage feeds the next. One clone onboards a new project (or a new
agent) into the full workflow.

## Use

1. "Use this template" on GitHub, then clone.
2. Edit the module path in `go.mod`; replace `main.go` / `main_test.go`.
3. `bd init`, then `tools/flywheel/setup-beads.sh`, in that order. `bd init` points `core.hooksPath`
   at its own hooks and tracks its JSONL export. The script untracks the exports, pushes the beads
   database to `refs/dolt/data` on origin, and hands `.git/hooks` to lefthook (agentic-flywheel ADRs
   0011 and 0012). Skipping it is how a merged branch's stale export reopened closed beads.
4. Fill in `SPEC.md`. Every feature starts as a `bd` issue.

## What's inside

- `CLAUDE.md` — agent conventions: intent sources, ponytail, test style, commit protocol
- `SPEC.md` + `docs/adr/` — where intent lives (Intent)
- `lefthook.yml` — pre-commit: gofmt, vet, short tests, <10s (Build); pre-push: local AI review (Validate)
- `.github/workflows/pr.yml` — lint + unit gate, no green no merge (Validate)
- `.claude/settings.json` — hooks: `bd prime` on session start, fmt+vet on stop
- `.github/workflows/release.yml` + `.goreleaser.yaml` — semver tag → binaries +
  changelog on GitHub Releases (Release)
- `.github/workflows/learn.yml` + `tools/dora/` — weekly DORA-lite snapshot
  (deploy frequency, lead time, change-failure rate, MTTR) committed to
  `docs/dora.json`, rendered by `docs/dora.html`; label incidents `incident` (Learn)

All five stages of the [flywheel spec](https://github.com/jamesponwith/agentic-flywheel)
ship with the template.

