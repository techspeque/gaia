---
type: adr
id: 0008
title: Each session runs under its own supervisor process
status: accepted
date: 2026-11-20
phase: 1
---

# ADR-0008: Each session runs under its own supervisor process

> Phase: 1 | Status: accepted | Date: 2026-11-20
> Decision drivers: sessions run for hours and must survive client disconnects and node-daemon restarts and upgrades.

## Context

If the node daemon owns each PTY directly, restarting or upgrading the daemon
kills every running session. tmux solves this but is an external dependency
and absent on Windows.

## Decision

`gaia node` starts each session as a detached `gaia supervise` process (same
binary) that owns the PTY, the child process and the scrollback file. The
daemon talks to supervisors over a per-session local socket and reconnects to
them after a restart.

### Rules this decision implies:

1. A supervisor outlives the node daemon; it exits only when its child exits or on stop.
2. Supervisor socket is user-only (0600 unix socket; named pipe with user ACL on Windows).
3. Scrollback is written to disk with secret redaction applied before write.
4. On start, the node daemon discovers live supervisors from its state directory and re-registers their sessions.
5. Many clients may attach to one session; input from any of them is accepted.

## Consequences

### Positive
- Daemon upgrades without killing work.
- No tmux; same behaviour on Windows (ConPTY).

### Negative
- Process-management code per OS.
- Redaction of scrollback is best-effort for secrets Gaia doesn't know about.

### Neutral
- A machine reboot still ends sessions; `metis auto` resumes from the ledger, plain sessions do not.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| tmux / zellij | Proven | External dep, no Windows | ADR-0002 |
| PTYs owned by the daemon | Simplest | Restart kills sessions | Fails the core requirement |

## References

- ADR-0002
