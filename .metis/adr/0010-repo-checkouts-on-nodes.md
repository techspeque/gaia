---
type: adr
id: 0010
title: Nodes keep their own git checkouts and sync through the remote
status: accepted
date: 2026-11-20
phase: 1
---

# ADR-0010: Nodes keep their own git checkouts and sync through the remote

> Phase: 1 | Status: accepted | Date: 2026-11-20
> Decision drivers: a coder on one node and a reviewer on another must see the same ledger and code.

## Context

Metis state is in git: every flip, block and brief is a commit on the
integration branch. When `gaia run` sends a session to another machine, that
machine needs the caller's branch at its latest commit, and the session's
commits must come back before the next session starts anywhere else.

## Decision

Each node keeps one checkout per remote URL in its workspace directory. Before
a session, the node fetches and fast-forwards the requested branch. After the
session exits, the node pushes the branch if it has new commits. The git
remote is the only sync channel.

### Rules this decision implies:

1. The caller's branch must be pushed before `gaia run`; Gaia fails with a sync exit code if the caller is ahead of the remote.
2. Before a session: `fetch`, then fast-forward only. A non-fast-forward or a dirty checkout fails the run; Gaia never resets, rebases or stashes.
3. After a session: push if ahead, fast-forward only. A rejected push leaves the checkout as it is and reports it; Gaia never force-pushes.
4. The caller (Metis or a person) fetches after `gaia run` returns; Gaia does not touch the caller's checkout.
5. Git credentials for fetch and push are node-level (the user's SSH agent or credential helper); Gaia does not distribute them in Phase 1.
6. One session per checkout at a time; a second run on the same repo and node queues or uses a worktree (decided in the Phase 1 plan).

## Consequences

### Positive
- The ledger stays the only state, as Metis requires.
- Killing Gaia leaves a checkout a person can continue by hand.

### Negative
- Every remote session needs a push before and a fetch after. Local `metis auto` driving remote agents has to fetch between sessions; that may need a small Metis change or a wrapper.
- Unpushed local work is invisible to remote sessions, by design.

### Neutral
- Git-host credentials could join the credential store later (ADR-0007).

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Sync working trees directly (rsync, mutagen) | No push needed | Second state channel, conflicts outside git | Breaks "ledger is the only state" |
| Run all sessions on one node | No sync | Defeats multi-machine | Core requirement |
| Gaia rebases or resets to recover | Fewer failed runs | Silent history changes | Unsafe |

## References

- ADR-0009
- `metis/plans/auto.md` design law 2
