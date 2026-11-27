---
type: adr
id: 0011
title: Remote access is tailnet-first, with an opt-in public link that is watch-only by default
status: accepted
date: 2026-11-20
phase: 4
---

# ADR-0011: Remote access is tailnet-first, with an opt-in public link that is watch-only by default

> Phase: 4 | Status: accepted | Date: 2026-11-20
> Decision drivers: goal 2, safe access to running sessions from anywhere on the internet.

## Context

Users want to check a session from a phone or someone else's laptop. A device
with the Tailscale app can already reach the host over the tailnet. A device
without it, such as a locked-down work laptop, needs a public address.
Tailscale Funnel exposes a tailnet service on a public HTTPS URL, and `tsnet`
supports it directly.

Typing into a session means sending input to an agent that can run commands
on that machine. Remote input is remote code execution.

## Decision

Serve the UI and API over the tailnet by default. Offer a public link through
Funnel as an opt-in, with Gaia's own sign-in. Watching is the default on the
public link; acting is gated.

| Action | Tailnet | Public link |
|---|---|---|
| View machines, sessions, output, usage | allowed | signed in |
| Stop a session | allowed | signed in |
| Type into a session | allowed | fresh sign-in check, per session, time-limited |
| Start a session | allowed | fresh sign-in check |
| Manage credentials, nodes, settings | allowed | never |

### Rules this decision implies:

1. The public link is off by default. `gaia host --public` refuses to start until at least one passkey or OIDC user exists.
2. Public sign-in: passkeys (WebAuthn) built in; OIDC for teams. No passwords.
3. A fresh sign-in check means a passkey or OIDC re-authentication within the last few minutes, bound to one session.
4. The public link serves only the UI and `/api/v1`. The API-key proxy, node endpoints and supervisor sockets are never reachable through it.
5. Credential management, node join and settings are tailnet-only, whatever the user's role.
6. Every public sign-in, input grant and session start goes to the audit log.
7. Browser sessions use secure, HttpOnly, SameSite=strict cookies; state-changing requests need a CSRF token; WebSocket upgrades check origin.
8. Scrollback is redacted before it is served, as before it is written (ADR-0008).

## Consequences

### Positive
- Phones and locked-down laptops can watch sessions with nothing installed.
- A stolen browser session can watch but not act without the user's passkey.
- Teams get per-user identity and an audit trail.

### Negative
- Gaia now owns web authentication code, which is security-critical.
- A public endpoint can be found and probed; rate limits and lockouts are required.
- Funnel ties the public link to Tailscale; Headscale users need their own reverse proxy (documented, not built).

### Neutral
- Tailnet access keeps tailnet identity as sign-in; no second login on trusted devices.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Tailnet only, forever | Smallest attack surface | No access from devices without Tailscale | Fails goal 2 |
| Public link with full control | Simplest | Stolen cookie means remote code execution | Unsafe |
| Hosted relay run by the project | Easiest for users | A service, accounts and trust in us | Non-goal: self-hosted only |

## References

- ADR-0004, ADR-0008, ADR-0012
