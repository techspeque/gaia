---
type: adr
id: 0003
title: Subscriptions are injected into official clients; API keys are proxied
status: accepted
date: 2026-11-20
phase: 2
---

# ADR-0003: Subscriptions are injected into official clients; API keys are proxied

> Phase: 2 | Status: accepted | Date: 2026-11-20
> Decision drivers: let each user manage their subscriptions and keys from one place, across many machines, within provider terms.

## Context

Two kinds of credential exist:

- **Subscriptions** (Claude Pro/Max, ChatGPT plans). The owner may use them on
  any number of their own machines through the provider's official client.
  Anthropic limits subscription credentials to Claude Code and claude.ai;
  other tools and relays are not permitted. `claude setup-token` issues a
  long-lived token for headless use, read from `CLAUDE_CODE_OAUTH_TOKEN`.
- **API keys** (Anthropic API, OpenAI API, Azure AI Foundry). Usable by any
  client, safe to proxy.

Provider terms move. Re-check before each release.

## Decision

Gaia distributes subscription credentials and proxies API keys, and never
mixes the two.

### Rules this decision implies:

1. A subscription credential is bound to a provider client allowlist (e.g. `claude` for Anthropic). Gaia injects it only when the session's argv[0] is on that allowlist. When Metis launches through `gaia run` (ADR-0009), Gaia starts the client itself, so the check applies directly. A session running `metis auto` on a node gets no subscription credentials; its agent sessions should go through `gaia run` instead.
2. Subscription credentials reach the node in memory at session start and enter only that session's environment. Never written to node disk.
3. Gaia never sits in the request path of subscription traffic. No proxy, relay or replay.
4. API keys never leave the host process. Sessions get a base URL on the node forwarder and a session token.
5. Session tokens are scoped to one session and its profile's credentials, and expire when the session ends.
6. Credential secrets never appear in logs, scrollback, the UI or API responses.
7. A subscription credential belongs to one user. In a multi-user installation, Gaia injects it only into sessions that user starts; it is never shared across users. API keys may be shared by profile, as the provider allows.

## Consequences

### Positive
- One place to add, rotate and revoke every credential for every machine.
- Subscription use stays inside the official client.
- API-key usage is metered per session for free.

### Negative
- A session can read its own injected subscription token. Accepted; mitigated by per-session scope, rotation and revocation.
- Subscription usage is not visible on the wire; Gaia estimates it from session runtime and client output.
- Host outage stops API-key sessions mid-flight.

### Neutral
- All machines share one subscription's limits; Gaia must account for that (OVERVIEW §8).

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Proxy subscription traffic, inject token at the proxy | Token never on the node | Relays a consumer credential through a third-party component | Terms risk; account risk |
| Copy `~/.claude` credentials to each node | Simple | Secrets at rest everywhere, no central rotation | Defeats the purpose |
| API keys on nodes via env | No proxy | Keys at rest in session env, no metering | Proxy is cheap |

## References

- ADR-0005, ADR-0007
