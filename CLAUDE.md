# CLAUDE.md

Go project built inside the personal agentic flywheel: Intent → Build → Validate → Release →
Operate → Learn.

## Before writing code (Intent)

- Read SPEC.md and the open `bd` issue for the task. No bead, no build — create one first (`bd q "..."`).
- Any feature bigger than one session gets a SPEC.md section before code.
- Decisions that would take >5 minutes to re-derive get an ADR: `docs/adr/`, ~20 lines, copy `template.md`.

## Conventions (Build)

- Ponytail active: the laziest solution that works. stdlib first.
- No new dependency without an ADR justifying it.
- Test-first; table-driven tests (see `main_test.go` for the shape).
- Deliberate shortcuts get a `// ponytail:` comment naming the ceiling and the upgrade path.
- Pre-commit (lefthook) runs gofmt, go vet, `go test -short` — keep the whole hook under 10s.

## Commit protocol

- Small commits, imperative subject, reference the bead ID: `bd-12: add retry backoff`.
- Never bypass a failing pre-commit hook. If a gate gets skipped twice, delete it or automate it.
- Pre-push runs a local AI review (ponytail-review) — advisory findings, read them before opening the PR.
- PRs merge only on a green Validate pipeline; the human is the final approver.

## Running in production (Operate)

- `docs/slo.yml` is the contract: probe url, latency budget, and how long a
  breach must persist before it is an incident. Point `url` at the real
  deployment before enabling `.github/workflows/operate.yml`.
- Keep `healthz.go` when you replace `main.go`, and wire `Healthz` into your
  server. The version it reports is how Learn attributes an incident to a release.
- Incidents are filed and closed by `tools/watch`, never by hand. If you find
  yourself opening an `incident` issue manually, the prober is misconfigured —
  fix that instead, or the DORA numbers go back to measuring your memory.
