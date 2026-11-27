---
type: adr
id: 0013
title: Gaia reads Ship through its CLI on each node; Gaia never deploys and never gates Ship
status: accepted
date: 2026-11-24
phase: 4
---

# ADR-0013: Gaia reads Ship through its CLI on each node; Gaia never deploys and never gates Ship

> Phase: 4 | Status: accepted | Date: 2026-11-24
> Decision drivers: Ship, the family's deployment tool, is built before Gaia and must fit it later without either depending on the other.

## Context

Ship turns a working tree into running containers on one Linux machine. It
is a separate Rust binary with its own daemon, state and secret store, and
it is built first because it is needed to host projects now (Ship ADR-0004).
Its daemon listens on a group-only unix socket, 127.0.0.1 and optionally the
machine's tailnet address (Ship ADR-0003), the same shape as Gaia's
listeners.

A machine that runs agent sessions is often the machine that hosts what
they build. Seeing sessions and deployments in one place is useful. Running
deployments from Gaia is not Gaia's job.

A session runs as the node's OS user and is not sandboxed (§3.2). If that
user can reach Ship's socket, so can every session. Gaia has no way to
allow this for one profile and deny it for another.

## Decision

Gaia and Ship stay separate and meet only at the `ship` CLI. A node that
finds `ship` installed runs its read-only JSON commands against the local
Ship daemon and reports the result to the host. Nothing else.

### Rules this decision implies:

1. Gaia never imports Ship code; Ship never imports, calls or detects Gaia.
2. Gaia reads only `ship projects`, `ship status`, `ship deployments` and `ship logs`, all with `-o json`.
3. All Ship access lives in `internal/ship`, imported only by `node`.
4. Gaia works with Ship absent; Ship features show as unavailable, not as errors.
5. Gaia never runs `ship push` or any mutating Ship command, and offers no UI to do so.
6. Gaia never hands a credential to Ship and never reads Ship's secrets. The two stores stay separate.
7. Whether sessions can deploy is decided outside Gaia: by whether the node's OS user is in the machine's `ship` group. The docs say what granting that means.
8. Gaia's tailnet does not carry Ship traffic. Ship is reached over SSH or its own tailnet listener.
9. Ship's JSON shapes are provisional until Ship v1.0; the adapter is tested against recorded fixtures per supported Ship version.

## Consequences

### Positive
- Ship can ship first and Gaia needs no change to it later.
- One view of what is running on each machine: sessions and deployments.
- Gaia's threat model does not grow: it gains a read-only exec, no new credentials.

### Negative
- Two daemons, two secret stores and two machine lists on a machine that has both.
- Deploy access for sessions is all-or-nothing per node.
- Gaia depends on JSON shapes that are not frozen until Ship v1.0.

### Neutral
- An agent in a session may still call `ship push` itself when the OS allows it; Gaia sees the resulting deployment like any other.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Deployment as a node capability | One daemon | Blocks Ship on Gaia; needs root-equivalent Docker access in a no-root daemon | Scope and §3.1 |
| Gaia gates deploys per profile | Finer control | Unenforceable without sandboxing sessions | §3.2 non-goal |
| Gaia forwards Ship traffic over its tailnet | One network | Couples the two; widens the forwarder | Rule 8 keeps the forwarder narrow |
| No integration | Nothing to maintain | Two places to look | Cheap to do through the CLI |

## References

- ADR-0001, ADR-0004, ADR-0009
- Ship ADR-0003, ADR-0004
