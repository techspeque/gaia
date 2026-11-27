# Agent Contract — gaia

This repository is managed by Metis. ALL autonomous work follows the
protocol below. These rules are non-negotiable.

## Mandatory

Run `metis kickoff` from step 1 at the start of every session.
Do NOT skip this. Do NOT start work without following the protocol.

## Hard Rules

1. ONE slice at a time — `metis next` decides which, not you
2. Brief BEFORE code — commit scope contract before implementation
3. Scope is a contract — only touch files declared in your brief
4. Cross-vendor review — you cannot review your own work
5. `metis commit` for all commits — enforces format, strips attribution
6. STOP on environment failure — do not modify code to fix a broken sandbox
7. Dirty tree with in-scope files — resume the interrupted session (read brief, check git log, continue)
8. Dirty tree with out-of-scope files — STOP and report to human
9. Reality beats documents — if code contradicts plan, fix the document
10. No planning in execution — do not re-scope or invent additional work
11. Report mismatches — if you're the wrong agent for this slice, STOP
12. Trust the tools — do not walk YAML, compare slugs, or evaluate booleans manually
13. Exact values come from `-o json` — every read command supports it; never parse human-readable output

---

# gaia — Agent Contract

This contract governs all autonomous work in this repository.

## Project Overview

Source: `OVERVIEW.md`

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

One binary, two roles: `gaia host` and `gaia node`. One machine may run both
(`gaia host --node`), and then needs no tailnet (local-only mode). The `gaia` CLI, and Metis through
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
- Every listener binds to the tailnet, to 127.0.0.1 or to a user-only local socket. Never 0.0.0.0. The one exception is the embedded tailnet client's WireGuard UDP socket, which is the tailnet transport itself and accepts only authenticated WireGuard. The only public exposure is the opt-in Funnel link, and it serves only the UI and API.
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
registration and heartbeat, `gaia service install` for Linux and macOS.

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
Installers, Windows service registration, user and operator docs, a threat model
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
gaia host [--state DIR] [--node]
gaia node --host NAME
gaia service install|uninstall
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
- **First run:** `gaia host --node` alone is local-only; `gaia host --tailnet` joins a tailnet; `gaia node --join <key>` adds a machine. Public link stays off until `gaia host --public` and at least one passkey or OIDC user exists.
- **State:** host state dir holds SQLite, tsnet state and the built-in store; back it up as one directory.
- **Logging:** structured JSON to stderr with secret redaction at the logger.
- **Upgrades:** host and node versions negotiated on registration; host serves N and N-1 nodes.
- **Limits:** usage per credential, rolled up across nodes, with warn and pause thresholds set per credential.
- **Failure modes:** host down → running sessions continue, new sessions cannot start, API-key sessions lose the proxy; node down → its sessions continue under their supervisors and reattach when it returns.

## Session Start Protocol

Every autonomous session begins by running `metis kickoff` from step 1.
No pasted prompt is needed. The CLI provides all context.

## Branch & Commit Rules

- All work lands on the `feature/phase-0-foundation` branch. Never commit to `main`.
- Use `metis commit` for all commits — it enforces format and strips attribution.
- Commit format: `{prefix}({slice_id}): {message}`
- Allowed prefixes: feat, fix, refactor, docs, test, chore
- Every commit subject contains the slice ID.
- No AI attribution in commits (Co-Authored-By, Generated with, model names).

## Definition of Done

A slice is done only when ALL hold:
1. Implementation matches the brief in .metis/briefs/<slice-id>.md
2. Tests proportional to the testing rules exist and pass
3. `metis verify` is green, confirmed independently by the Reviewer
4. The Reviewer walked the checklist with no blocking findings
5. Ledger and brief are committed; commit subjects carry the slice ID

## Roles

- **Coder** — implements one slice within its declared file scope; owns its tests
- **Reviewer** — reviews one slice against the checklist; re-runs verification independently; owns the sign-off
- **Human** — owns planning, scope conflicts, escalations, and release merges

Reviews are cross-vendor by default.

## Hot-Path Zones

(None configured)

## Scope Discipline

- Before any code, commit a brief declaring file scope
- Implement only within declared files
- Genuinely-required out-of-scope fixes go in the brief's "Out-of-scope touches" section
- If the slice needs a non-goal item, or scope differs materially from the plan, stop and report

## Model Routing

- Review: codex/gpt-6-astra

## Testing Rules

(None configured)

## Non-Goals

(None configured)

## Conventions

(None configured)

## Accuracy Rules

(None configured)

## Review Checklist

Walk in order; one-line verdict per item citing file:line evidence (a brief's `## Review` section replaces this list for its slice):
1. Behavioral correctness
2. Security and authorization correctness
3. Scope discipline (diff vs. the committed brief)
4. Test sufficiency
5. Architectural fit — no duplicated/hallucinated interfaces; matches ADRs
6. Maintainability

Record every issue in one review — one `metis block` per blocking finding. A finding blocks only when it is one of:
- a deviation from the brief
- a missing or failing test the brief lists
- a broken accuracy rule
- a demonstrated defect
Anything else is an advisory (`metis findings record`).

From review_cycles 1 on, a review is narrowed: confirm or re-raise the open findings; a new blocking finding only at P1; other new observations are advisories.

## Feedback Loop

- Every blocking review finding is logged via `metis block`
- Findings tracked in .metis/findings.yaml
- Recurring failures graduate into new accuracy rules (`metis rule promote`)
- review_cycles per slice provides routing evidence
- Phase gates validate composed system behavior

## Tooling Map

| Command | Purpose |
|---|---|
| `metis next` | Find active slice, role, required model |
| `metis kickoff` | Session protocol steps |
| `metis instructions --for <id>` | Risk-scaled contract for a slice |
| `metis brief <id> --write` | Generate brief template |
| `metis verify --pre` | Pre-flight verification |
| `metis verify --post` | Post-implementation verification |
| `metis verify --env` | Environment soundness check only |
| `metis interfaces` | Regenerate API summary |
| `metis commit -m "..."` | Commit with enforced format |
| `metis commit --brief` | Commit the brief |
| `metis commit --flip coded` | Flip coded and commit |
| `metis commit --flip reviewed --agent <slug>` | Flip reviewed and commit (identity required) |
| `metis block <id>` | Block a slice during review; again for each further blocking finding |
| `metis wait <id> --reason ... [--for 90m]` | Park a slice on something outside the repository (a lab run, an approval); `--clear` returns it |
| `metis archive` | Move done slices to archive |
| `metis check` | Validate config + ledger |
| `metis status` | One-line progress summary |
| `metis log <id> --validate` | Audit slice commits: format + scope vs brief (scope N/A for gates — inspect manually) |
| `metis findings record <id>` | Record advisory (non-blocking) observation |
| `metis findings resolve <f-id> --commit <rev> --test <name>` | Close a finding with the fix's evidence (or `--ref <file:line>`; `--duplicate-of <f-id>` for a duplicate) |
| `metis findings waive <f-id> --reason ... --by <who>` | Accept a finding as it stands, on record (a person's decision; counted apart from fixes) |
| `metis config get <key>` | Read one config value |

Every read command accepts `-o json`. When you need an exact value
(slice ID, agent slug, status), read the JSON field — never parse the
human-readable text.
