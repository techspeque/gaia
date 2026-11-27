---
type: overview
project: gaia
status: living
last_updated: 2026-11-27
---

# Gaia — Full Specification

> Open-source, self-hosted: one binary that runs coding-agent sessions on any of your machines, for hours, with every credential in one place, and lets you watch them from anywhere.

---

## 1. Purpose

Agent sessions now run for hours. They outlive a laptop lid, a train ride and
a terminal tab. Today that means SSH, tmux, and logging every agent CLI in to
every provider on every machine, then repeating it when a token rotates.
Nothing shows which machine is running what, or how close a subscription is to
its limit.

Gaia has two goals:

1. **Easy access to, and setup of, your own machines.** One command adds a
   machine. A **host** holds credentials and the session registry; a **node**
   on each machine runs sessions that survive disconnects and gets
   credentials from the host at session start. Machines find each other over
   a tailnet with no ports, DNS or certificates to manage (ADR-0004).
2. **Safe access to running sessions from anywhere.** Watch, stop and, with a
   fresh sign-in check, type into any session from any device: over the
   tailnet by default, or from any browser through an opt-in public link
   (ADR-0011).

Gaia is agent-agnostic (Claude Code, opencode, Codex) and is Metis's remote
launcher. Every way Metis starts an agent goes through Gaia to any selected
tool on any machine: `metis auto`, `metis launch`, or a person starting a
slice session by hand (ADR-0009). Gaia shows slice progress across machines.
Gaia never needs Metis, and Metis never needs Gaia (ADR-0001).

Ship is the third tool in the family: it runs what sessions build, as
containers on a machine the user owns. Ship is built before Gaia and stands
alone. Where a node also has Ship installed, Gaia shows that machine's
deployments beside its sessions, read through `ship … -o json` only. Gaia
never needs Ship, and Ship never needs Gaia (ADR-0013).

Gaia is an open-source project alongside Metis, MIT licensed, self-hosted,
with no hosted service and no telemetry (ADR-0012). Each installation belongs
to one user or one team and runs on machines they control.

Users: individual developers with several machines, first. Teams, including
inside enterprises, second; they shape the seams (ADR-0007, ADR-0011) but not
Phase 0–2 scope.

---

## 2. Architecture

```
   you (browser / CLI)        metis auto · metis launch
          │                   (agents.<slug>.launch = gaia run …)
          └──────────┬───────────────┘
                     v
┌───────────────────────────────────────────────┐
│ gaia host                                     │
│  registry · credential store · API proxy · UI │
│  SQLite                                       │
└───────────────┬───────────────────────────────┘
                │ tailnet (tsnet, no public ports)
      ┌─────────┴──────────┐
      v                    v
┌──────────────┐    ┌──────────────┐
│ gaia node A  │    │ gaia node B  │
│  supervisor  │    │  supervisor  │
│  127.0.0.1   │    │  127.0.0.1   │
│  forwarder   │    │  forwarder   │
└──────┬───────┘    └──────┬───────┘
       v                   v
  claude · opencode · codex, headless or interactive
  (PTY, env-injected credentials, node's own git checkout)
```

One binary, two roles: `gaia host` and `gaia node`. One machine may run both,
and then needs no tailnet (local-only mode). The `gaia` CLI, and Metis through
it, talk only to the daemon on the same machine over a user-only local socket;
only daemons join the tailnet (ADR-0004).

### 2.1 System Boundaries

Inside: session lifecycle, PTYs and scrollback, machine registry, credential
storage and distribution, the API-key pass-through proxy, usage accounting,
the web UI and CLI.

Outside, consumed as-is:

| External | How Gaia uses it |
|---|---|
| Agent CLIs (claude, opencode, codex) | Launched as child processes in a PTY. Never modified, never wrapped beyond env and argv. |
| Provider APIs (Anthropic, OpenAI, Azure AI Foundry) | API-key traffic passes through the host proxy unchanged. Subscription traffic goes client → provider directly (ADR-0003). |
| Tailscale / Headscale | Control server for the embedded tailnet, between machines only; Funnel for the opt-in public link (ADR-0004, ADR-0011). |
| Identity providers (OIDC), passkeys | Sign-in for the public link and for teams (ADR-0011). |
| Metis | Calls `gaia run` as an agent launcher (ADR-0009). Gaia reads it through `metis … -o json` only (ADR-0001). |
| Ship | Optional, per node. The node runs `ship … -o json` against the local Ship daemon to list projects and deployments. Gaia never deploys, and never grants or mediates a session's access to Ship (ADR-0013). |
| Git remotes | The only sync channel between the caller and node checkouts (ADR-0010). |
| Secret backends (keychain, Key Vault, Vault) | Source of credentials behind one interface (ADR-0007). |

### 2.2 Data Model

Host, SQLite (ADR-0006):

| Entity | Key fields |
|---|---|
| Node | id, name, tailnet identity, os/arch, gaia version, last_seen |
| Credential | id, kind (`subscription` \| `api_key`), provider, backend ref, label, rotated_at. Never the secret itself unless the backend is the built-in encrypted store. |
| Profile | id, name, credential ids, agent surface, default argv, env template |
| Session | id, node, profile, repo remote, branch, slice (optional), mode (`headless` \| `interactive`), argv, state, started_at, ended_at, exit code |
| SessionToken | session id, hash, scope (which API-key credentials), expires_at |
| UsageEvent | session id, credential id, provider, model, tokens in/out, ts |

Node, on disk: git checkouts, session scrollback logs and supervisor state. No credentials.

### 2.3 Component Structure

```
cmd/gaia            entry point, role dispatch
internal/core       ids, states, reserved exit codes, error shape, redaction; no I/O
internal/host       registry, API, UI serving
internal/node       node agent, supervisor client, forwarder
internal/repo       node checkouts: fetch, fast-forward, push
internal/supervise  per-session supervisor process, PTY, scrollback
internal/creds      credential model, backends interface, built-in store
internal/proxy      pass-through reverse proxy, token check, usage capture
internal/net        tsnet wiring
internal/store      SQLite access
internal/metis      metis CLI adapter (exec + JSON decode, nothing else)
internal/ship       ship CLI adapter (exec + JSON decode, nothing else)
web/                UI, built to static files, embedded with go:embed
```

Dependency rule: `host` and `node` depend on the leaves; leaves depend on
`core` and never import each other except `proxy → creds`. `metis` is imported only by `host`; `ship`
only by `node`.

---

## 3. Constraints

### 3.1 Technology

- **Language:** Go
- **Build system:** `go build`, CGO off, static binaries for darwin/linux/windows × amd64/arm64
- **Storage:** SQLite via a pure-Go driver (host); flat files (node)
- **Runtime:** a user-level service (launchd, systemd user unit, Windows service); no root
- **Networking:** embedded `tsnet`
- **UI:** static build embedded in the binary; no Node.js runtime

### 3.2 Non-Goals (do NOT build)

- Translating between provider API formats. Pass-through only (ADR-0005).
- Proxying, replaying or relaying subscription (OAuth) traffic (ADR-0003).
- Giving subscription credentials to any client other than the provider's own CLI (ADR-0003).
- Patching, forking or wrapping agent CLIs beyond argv and environment.
- Any public internet listener other than the opt-in public link for the UI and API (ADR-0011).
- A hosted Gaia service, accounts on our side, or telemetry.
- Running sessions for people outside the installation's own users.
- Planning, slicing, review or ledger logic. That is Metis.
- Importing Metis Go packages.
- Building, deploying, routing or hosting applications. That is Ship.
- Storing Ship's secrets, or handing Gaia credentials to Ship.
- Multi-tenant SaaS hosting.
- Sandboxing agents. Gaia runs them as the user would.

### 3.3 Invariants (must ALWAYS hold)

- No secret is written to disk on a node, and no secret appears in a log, a scrollback file, the UI or an API response.
- API keys never leave the host process; agents receive only a session-scoped token.
- A subscription credential is injected only into a session whose argv[0] is that provider's official CLI.
- `gaia run --wait` behaves like a local agent process: streamed output, the agent's exit code, stop on interrupt.
- Gaia never resets, rebases, stashes or force-pushes a checkout.
- Gaia never chooses the agent, model or slice.
- Every listener binds to the tailnet, to 127.0.0.1 or to a user-only local socket. Never 0.0.0.0. The only public exposure is the opt-in Funnel link, and it serves only the UI and API.
- Over the public link, typing into or starting a session needs a fresh sign-in check, per session.
- Defaults are the safe choice: public link off, remote input off, no credentials until added.
- A session survives the loss of its client, and of the node daemon (ADR-0008).
- Killing Gaia at any point leaves every repo in a state a person can continue by hand.
- The binary builds with CGO disabled.

---

## 4. Phases (High-Level Roadmap)

### Phase 0 — Foundation
Repo, CI, signed release pipeline, open-source scaffolding (LICENSE,
CONTRIBUTING, SECURITY), `gaia host` / `gaia node` skeleton, CLI-to-daemon
local socket, local-only mode, tsnet join, SQLite schema and migrations, node
registration and heartbeat.

### Phase 1 — Sessions
Per-session supervisor, PTY, scrollback, start / list / attach / detach / stop
from the CLI. Sessions survive client and node-daemon restarts. Node git
checkouts. `gaia run --wait` meets the launcher contract, proven by
`metis launch` and `metis auto` driving remote agents.

### Phase 2 — Credentials
Credential model, built-in encrypted store and OS keychain backends,
profiles, subscription token injection, the API-key proxy with session tokens
and usage capture.

### Phase 3 — UI and Metis
Embedded web UI on the tailnet: machines, sessions, live terminal (watch-only
by default), usage against limits.
Metis adapter: interactive slice sessions from `metis handoff`, slice
progress per repo across nodes, notify when a session stops or a slice waits.

### Phase 4 — Remote Access and Teams
Opt-in public link through Funnel, passkey and OIDC sign-in, fresh sign-in
checks for remote input, multiple users per installation, audit log.
Azure Key Vault and HashiCorp Vault backends, Entra ID for Foundry, an
upstream gateway setting.
Ship adapter: deployments per node in the UI, where Ship is installed
(ADR-0013).

### Phase 5 — Release
Installers, service registration, user and operator docs, a threat model
document, an upgrade path, v1.0.

---

## 5. Security Model

Trust boundaries, outermost first:

0. **Public internet (opt-in only).** With the public link on, anyone can reach the host's UI and API over HTTPS. Gaia signs users in itself (passkeys or OIDC). Read-only by default; typing into or starting a session needs a fresh sign-in check, per session. The proxy and node endpoints are never on the public link (ADR-0011).
1. **Tailnet.** Only devices on the tailnet reach host or node. Tailnet ACLs restrict which identities may reach the host API. In local-only mode this boundary does not exist; everything is on one machine.
2. **Host.** Holds every credential. Compromise of the host is compromise of everything; it is the one component to harden.
3. **Node.** Holds nothing at rest. Receives a subscription credential in memory for a session's lifetime and passes it to that session's environment.
4. **Local socket.** The CLI, and Metis through it, reach the daemon over a user-only socket. Any process running as the user can; that is the same trust as the user's shell.
5. **Agent session.** Untrusted. Assume it can read its own environment and anything on the node the user can. It reaches Gaia only through the 127.0.0.1 forwarder, with its session token.

Consequences stated plainly:

- A session can read the subscription token injected into it. That is inherent to how the official CLIs authenticate. Mitigation: per-session injection, rotation from the host, usage visibility, and revocation.
- A session cannot read API keys. It holds a session token valid only for its own lifetime and only for the credentials its profile names.
- Anyone who can type into a session can run commands on that machine through the agent. Remote input is treated as remote code execution and gated as such.
- A session can deploy through Ship exactly when the OS user running the node is in that machine's `ship` group. Gaia does not sandbox sessions and cannot gate this; adding the user to the group lets every agent session on that node run containers there (ADR-0013).
- Sensitive data: credentials (high), scrollback (medium: may contain code and secrets the agent printed), usage (low).
- As an open-source project: vulnerabilities are reported privately under SECURITY.md; releases are signed with checksums and an SBOM (ADR-0012).

---

## 6. API / Interface Contracts

### CLI (human)

```
gaia host [--state DIR]
gaia node --host NAME
gaia nodes
gaia creds add|list|rotate|remove
gaia profiles add|list|remove
gaia run --node N --profile P [--repo URL] [--branch B]
         (--wait | --interactive [--slice ID] | --detach) [-- argv…]
gaia ls
gaia attach SESSION
gaia stop SESSION
gaia logs SESSION
```

Every list command supports `-o json`.

### Host HTTP API (tailnet only)

JSON over HTTPS on the tailnet, versioned under `/api/v1`. Resources: nodes,
credentials (metadata only), profiles, sessions, usage. Terminal I/O over
WebSocket at `/api/v1/sessions/{id}/tty`.

### Proxy

Node forwarder on `127.0.0.1:<port>` exposes `/anthropic`, `/openai`,
`/azure/<deployment>`. Session env gets the matching base-URL variable and the
session token. Requests and streamed responses are forwarded unchanged apart
from the auth header.

### Launcher contract (provided to Metis, ADR-0009)

`gaia run … --wait -- <agent argv>` streams output, exits with the agent's
exit code, uses a reserved, documented range for Gaia's own failures (90 to
99), and stops the remote session on interrupt. The full contract is
`docs/contracts/launcher-v1.md`. Set as `agents.<slug>.launch` in
`metis.yaml`. Breaking changes need a major version.

### Metis contract (consumed)

`metis next`, `metis progress`, `metis status`, `metis show`, `metis handoff`,
all with `-o json`. Nothing else. Changes to these JSON shapes are breaking
changes for Gaia.

### Ship contract (consumed, ADR-0013)

`ship projects`, `ship status`, `ship deployments` and `ship logs`, all with
`-o json`, run on the node against its local Ship daemon. Nothing else.
Shapes are provisional until Ship v1.0.

---

## 7. Testing Strategy

- **Unit:** credential redaction, token scoping, proxy header rewrite, schema migrations.
- **Supervisor:** real PTYs against a fake agent binary in `testdata/` that prints, streams, sleeps and exits on cue; kill the daemon mid-session and reattach.
- **Proxy:** `httptest` upstreams that assert on headers and stream SSE; no real provider calls in CI.
- **Network:** tsnet tests against a local test control server; no Tailscale account in CI.
- **Metis adapter:** recorded JSON fixtures per supported Metis version.
- **Ship adapter:** recorded JSON fixtures per supported Ship version; Ship absent shows as unavailable, not an error.
- **Launcher contract:** `metis launch` against a node running the fake agent; assert output, exit codes, interrupt and ledger commits arriving back through the remote.
- **Repo sync:** local bare remote; caller ahead, dirty checkout, rejected push.
- **Invariant tests:** grep scrollback, logs and API responses for known test secrets; fail on any hit.

---

## 8. Operational Concerns

- **Install:** `install.sh` / `install.ps1` as Metis does, verifying checksums and signatures; `gaia service install` registers the user service.
- **First run:** `gaia host` alone is local-only; `gaia host --tailnet` joins a tailnet; `gaia node --join <key>` adds a machine. Public link stays off until `gaia host --public` and at least one passkey or OIDC user exists.
- **State:** host state dir holds SQLite, tsnet state and the built-in store; back it up as one directory.
- **Logging:** structured JSON to stderr with secret redaction at the logger.
- **Upgrades:** host and node versions negotiated on registration; host serves N and N-1 nodes.
- **Limits:** usage per credential, rolled up across nodes, with warn and pause thresholds set per credential.
- **Failure modes:** host down → running sessions continue, new sessions cannot start, API-key sessions lose the proxy; node down → its sessions continue under their supervisors and reattach when it returns.
