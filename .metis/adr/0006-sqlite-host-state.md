---
type: adr
id: 0006
title: Host state in pure-Go SQLite; nodes hold no durable state but scrollback
status: accepted
date: 2026-11-20
phase: 0
---

# ADR-0006: Host state in pure-Go SQLite; nodes hold no durable state but scrollback

> Phase: 0 | Status: accepted | Date: 2026-11-20
> Decision drivers: durable registry without a database server, CGO off.

## Context

The host needs a registry of nodes, profiles, credential metadata, sessions
and usage. It runs on one machine. ADR-0002 rules out CGO and a separate
database.

## Decision

The host stores state in SQLite through a pure-Go driver
(`modernc.org/sqlite`), with embedded, forward-only migrations. Nodes keep
only scrollback and supervisor state on disk.

### Rules this decision implies:

1. One state directory per host: database, tsnet state, built-in credential store.
2. Migrations are numbered, embedded, forward-only and run at start.
3. WAL mode; one writer.
4. Nodes can be wiped and re-registered without losing anything but scrollback.

## Consequences

### Positive
- Backup is copying one directory.
- No server to run.

### Negative
- One host; no high availability.
- Pure-Go SQLite is slower than the C library; fine at this scale.

### Neutral
- Moving to Postgres later means a store interface, which `internal/store` provides anyway.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Postgres / MySQL | HA, team scale | Separate server | ADR-0002 |
| bbolt / flat files | Simple | No queries for usage roll-ups | Usage reporting needs SQL |

## References

- ADR-0002
