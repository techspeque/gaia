---
type: adr
id: 0002
title: One static Go binary with host and node roles
status: accepted
date: 2026-11-20
phase: 0
---

# ADR-0002: One static Go binary with host and node roles

> Phase: 0 | Status: accepted | Date: 2026-11-20
> Decision drivers: install on any machine with no runtime and no sidecars.

## Context

The point of Gaia is low friction on many machines. Every extra runtime
(Python for LiteLLM, Node for a UI server, a separate database) is another
thing to install, patch and explain to an enterprise security review.

## Decision

Ship one Go binary, built with CGO disabled, that runs as `gaia host`,
`gaia node`, or both on one machine. The UI, migrations and templates are
embedded.

### Rules this decision implies:

1. `CGO_ENABLED=0` in every build; CI fails otherwise.
2. No runtime dependency beyond the OS and the agent CLIs the user installs.
3. Every new direct dependency needs a line in the PR saying why.
4. The UI ships as static files via `go:embed`; no server-side JS runtime.
5. Release targets: darwin, linux, windows × amd64, arm64.

## Consequences

### Positive
- `curl | sh` and done, on any machine.
- One version number to reason about.

### Negative
- Pure-Go constraints on SQLite and PTY libraries.
- Host-only code ships to nodes (size, not risk: role is chosen at start).

### Neutral
- Feature work is all Go; the UI is the only other toolchain, at build time.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Separate host and node binaries | Smaller node | Two artifacts, version skew | Little gain |
| Compose stack (host + LiteLLM + Postgres) | Mature gateway | Many runtimes, not portable to laptops | Contradicts the goal |

## References

- ADR-0005, ADR-0006
