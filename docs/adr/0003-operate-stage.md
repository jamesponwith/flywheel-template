# 0003. Operate: probe the deployment, file incidents automatically

Date: 2026-08-15
Status: accepted

## Context

The template shipped Release (goreleaser, deploy.sh) and Learn (tools/dora) with
nothing in between. tools/dora derives change-failure rate and MTTR from
`incident`-labelled issues, which until now a human had to remember to file and
to close — so two of the four published metrics measured recall, not reliability.

## Decision

Add an Operate stage: `docs/slo.yml` declares the contract, `/healthz` serves it
(`healthz.go`, kept when you replace main.go), and `tools/watch` probes on a
schedule (`operate.yml`), filing one `incident` issue per (target, breach kind)
and closing it when the target recovers and holds.

Upstream flywheel decision: agentic-flywheel `docs/adr/0002`.

## Consequences

CFR and MTTR become measured. `/healthz` reporting a version lets Learn
attribute an incident to a release. Sustain windows (`breach_after`,
`recover_after`) mean a blip costs nothing and a flap does not spam the tracker.
Harder: `slo.yml` defaults to `example.invalid`, so a project that enables
operate.yml without pointing it at a real deployment will file nothing useful —
set the url or leave the workflow disabled.
