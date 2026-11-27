---
type: adr
id: 0004
title: Daemons join a tailnet through embedded tsnet; the CLI talks only to its local daemon
status: accepted
date: 2026-11-20
phase: 0
---

# ADR-0004: Daemons join a tailnet through embedded tsnet; the CLI talks only to its local daemon

> Phase: 0 | Status: accepted | Date: 2026-11-20
> Decision drivers: goal 1, easy access to and setup of a user's own machines, without anyone writing auth, TLS or NAT traversal.

## Context

Hosts and nodes sit behind NAT on laptops, home servers and cloud VMs.
Writing transport auth and TLS is where small tools get breached. Tailscale's
`tsnet` puts a Go program on a tailnet as its own device, with WireGuard
transport, identity, ACLs, MagicDNS names and TLS certificates, and no
system install. Headscale is an open-source control server for the same.

Metis runs `gaia run` once per session. If each CLI call joined the tailnet,
it would take seconds and register a new device every time. A single-machine
user should not need a tailnet at all.

## Decision

Only the long-running daemons (`gaia host`, `gaia node`) join the tailnet,
through embedded `tsnet`. The `gaia` CLI talks only to the daemon on its own
machine, over a user-only local socket. A host and node on one machine run in
local-only mode with no tailnet.

### Rules this decision implies:

1. Machine-to-machine traffic goes over the tailnet. Same-machine traffic goes over a local socket or 127.0.0.1.
2. The CLI never starts `tsnet`. With no local daemon, it fails with a clear message and the command to start one.
3. CLI-to-daemon: 0600 unix socket; named pipe with a user-only ACL on Windows.
4. No listener binds to anything but the tsnet interface, 127.0.0.1 or the local socket. The one exception is the opt-in public link (ADR-0011).
5. Host API calls over the tailnet are authorised by tailnet peer identity (`WhoIs`) plus a host-side allowlist.
6. Control server URL is configurable: Tailscale or Headscale. Docs cover both equally.
7. Node join keys are single-use or tagged, and never committed.
8. Local-only mode is the default for a fresh `gaia host`. Joining a tailnet is one flag.

## Consequences

### Positive
- `gaia node --join <key>` is the whole setup for a new machine: stable name, TLS, NAT traversal.
- One-machine users start with zero accounts; Tailscale arrives with the second machine.
- Fast CLI calls, so `gaia run` as a Metis launcher adds no noticeable start-up time.

### Negative
- Multi-machine use requires a Tailscale account or a Headscale server.
- Larger dependency tree (tsnet pulls in much of Tailscale).
- Every machine that calls `gaia run`, including the one running Metis, needs a running daemon.

### Neutral
- Other processes on a node cannot reach the tailnet through Gaia; the forwarder is the only bridge.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| mTLS over public HTTPS | No third party | Cert lifecycle, NAT, public exposure | Too much security code to own |
| SSH tunnels | Familiar | Key distribution, brittle reconnects | Not a product experience |
| System Tailscale | Smaller binary | Extra install, root on some OSes | Breaks ADR-0002 |
| CLI joins tailnet per call | No daemon on caller | Seconds per call, a device per call | Unusable as a launcher |

## References

- ADR-0002, ADR-0009, ADR-0011
