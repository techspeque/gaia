---
type: adr
id: 0001
title: Gaia is a separate binary; Metis and Gaia meet only at each other's CLI
status: accepted
date: 2026-11-20
phase: 0
---

# ADR-0001: Gaia is a separate binary; Metis and Gaia meet only at each other's CLI

> Phase: 0 | Status: accepted | Date: 2026-11-20
> Decision drivers: the session host and credential store were first proposed as part of Metis.

## Context

Metis's first design law (`metis/plans/auto.md`) is that Metis never owns the
agent's process, and that auth, sandboxing and tools stay the agent's. Agents
call the `metis` binary in every session. A binary that agents run must not
be able to read stored credentials. Metis is a deterministic per-repo CLI
with five direct dependencies; Gaia is a long-running daemon covering many
machines, with networking, a database and a secret store.

Metis must still be able to send any session, under `metis auto` or not, to
any coding tool on any machine Gaia manages.

## Decision

Build Gaia as its own repository and binary. The two meet in both directions,
and only through each other's CLI:

- **Metis → Gaia:** Metis launches sessions through `gaia run`, set as an
  agent's `launch` argv. Metis needs no Gaia code (ADR-0009).
- **Gaia → Metis:** Gaia reads `metis … -o json` to show slices and to build
  sessions from a slice's handoff.

### Rules this decision implies:

1. Gaia never imports `github.com/techspeque/metis/...`; Metis never imports Gaia.
2. Gaia reads only `metis next`, `metis progress`, `metis status`, `metis show` and `metis handoff`, all with `-o json`.
3. All Metis access in Gaia lives in `internal/metis`.
4. Gaia works with Metis absent; Metis features show as unavailable, not as errors.
5. Gaia never writes to `.metis/`. Ledger changes come only from agents and people running `metis`.
6. The `gaia run` contract Metis depends on (ADR-0009) is versioned and changed only with a major version bump.

## Consequences

### Positive
- Metis keeps its design laws, dependency count and threat model.
- Agents never run a binary that can read credentials.
- Every Metis path (`launch`, `auto`, a person pasting a handoff) can go through Gaia.
- Gaia is useful without Metis.

### Negative
- Two binaries to install and release.
- Two cross-repo contracts: Metis JSON shapes for Gaia, `gaia run` for Metis.

### Neutral
- Release tooling and install-script conventions are copied, not shared.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| `metis host` / `metis node` subcommands | One install | Agent-facing binary holds credentials; breaks Metis law 1; daemon deps in every repo | Security and scope |
| Same Go module, second `cmd/` | Shared `launch` code | Couples release cadence and dependency graph | Coupling outweighs a small amount of shared code |
| Gaia runs only `metis auto` | Simplest seam | Manual and single-slice sessions can't use remote tools | Too narrow |

## References

- `metis/plans/auto.md` §Design laws
- `metis/internal/launch/launch.go` (`agents.<slug>.launch`)
- ADR-0009
