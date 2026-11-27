---
type: adr
id: 0005
title: The API proxy passes requests through and never translates formats
status: accepted
date: 2026-11-20
phase: 2
---

# ADR-0005: The API proxy passes requests through and never translates formats

> Phase: 2 | Status: accepted | Date: 2026-11-20
> Decision drivers: keep the gateway small enough to own.

## Context

Gateways such as LiteLLM are large mainly because they translate between
provider formats. Gaia's clients already speak their provider's native API:
Claude Code speaks Anthropic, Codex speaks OpenAI, opencode speaks either.

## Decision

Each proxy route forwards to one provider in that provider's own format. The
proxy changes only the auth header and records usage.

### Rules this decision implies:

1. Routes: `/anthropic`, `/openai`, `/azure/<deployment>`. One upstream per route.
2. Request and response bodies are forwarded byte-for-byte; streaming (SSE) is flushed as it arrives.
3. The proxy strips the session token and sets the provider's auth header from the credential store.
4. Usage is read from response bodies and stream events without altering them.
5. Upstream base URL is configurable per route (Phase 4 gateway setting).

## Consequences

### Positive
- A few hundred lines on `httputil.ReverseProxy`.
- New provider features work on day one.

### Negative
- No cross-provider fallback or model aliasing.
- A client that speaks the wrong format for a route fails; Gaia does not fix it.

### Neutral
- Usage parsing is per provider and must track their response shapes.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Embed translation (LiteLLM-style) | Any client to any model | Large, always behind providers | Not needed by target clients |
| Run LiteLLM alongside | Mature | Python runtime, separate state | ADR-0002 |

## References

- ADR-0003
