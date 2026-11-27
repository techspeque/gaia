---
type: adr
id: 0007
title: Credentials come from pluggable secret backends
status: accepted
date: 2026-11-20
phase: 2
---

# ADR-0007: Credentials come from pluggable secret backends

> Phase: 2 | Status: accepted | Date: 2026-11-20
> Decision drivers: personal use wants zero setup; enterprises will not accept Gaia as the system of record for secrets.

## Context

On a personal machine the OS keychain or an encrypted file is enough. In an
enterprise, secrets live in Azure Key Vault or HashiCorp Vault, Foundry may
use Entra ID instead of keys, and model traffic may have to go through an
existing gateway (e.g. Azure API Management).

## Decision

Define one `creds.Backend` interface (get, put, rotate, delete by reference).
The registry stores references; backends hold secrets. Phase 2 ships the
built-in encrypted store and OS keychain; Phase 4 adds Key Vault, Vault and
Entra ID.

### Rules this decision implies:

1. The database stores a backend name and reference, never a secret, except inside the built-in store's own encrypted table.
2. Secrets are fetched per use and held in memory only for the request or session.
3. Backends are compiled in; no plugin loading at runtime.
4. Built-in store key comes from the OS keychain where available, otherwise a passphrase at host start.

## Consequences

### Positive
- Same product personally and in an enterprise; only configuration differs.
- Rotation in the backend is picked up without touching Gaia.

### Negative
- Backend SDKs add dependencies (Phase 4).
- Per-use fetch adds latency; cache with short TTL if needed.

### Neutral
- The upstream gateway setting (ADR-0005 rule 5) is the matching seam on the traffic side.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Built-in store only | Simplest | Enterprise blocker | Rules out the second audience |
| External plugins (gRPC) | Extensible | Runtime deps, attack surface | ADR-0002 |

## References

- ADR-0003, ADR-0005
