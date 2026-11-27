---
type: plan
phase: 0
title: Foundation
overview_ref: OVERVIEW.md §4 Phase 0
status: draft
created: 2026-11-27
---

# Phase 0 — Foundation

> Derived from: OVERVIEW.md §4 Phase 0
> Produces: slices phase-0-ws-0.1 … 0.8 and phase-0-gate in .metis/slices.yaml
> ADRs: 0001, 0002, 0004, 0006, 0012 (all in .metis/adr/)

## Context

Phase 0 builds what sessions stand on and runs no session. After it, a
`gaia host` runs on one machine with an empty registry in SQLite, a second
machine joins it with `gaia node --join`, and `gaia nodes -o json` on either
machine lists both with their last heartbeat. With one machine there is no
tailnet at all. No listener exists outside the tailnet interface, 127.0.0.1
and a user-only local socket.

Decisions made in this plan:

- **Local-only mode is one process.** `gaia host` also runs the node role
  for its own machine, in-process, unless started with `--no-node`. `gaia
  node` is only for the other machines.
- **A shared types package.** `internal/core` holds ids, states, exit codes,
  the error shape and the redaction type. Leaves depend on `core` and not on
  each other (OVERVIEW §2.3 is updated with this plan).
- **Windows builds, and does nothing more yet.** All six targets compile in
  CI (ADR-0002 rule 5). The daemon roles refuse to start on Windows with a
  clear message until the named-pipe and ConPTY work is planned, after
  Phase 2.
- **Service registration is by hand for now.** `gaia service install` stays
  in Phase 5. The gate uses a hand-written user unit described in
  `.local/hosts.md`.

## Dependencies

- Requires: none (first phase)
- External: GitHub Actions for CI; a Tailscale account or a Headscale server for the gate only, never for CI
- Local: the gate machines, their names and what an agent may do on them are described in `.local/hosts.md`, which is gitignored and never committed (ADR-0012 rule 7). Creating tailnet auth keys is the Human's.

---

## Workstream 0.1: Module scaffold and verify gate

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation

Tasks:
- Create the Go module `github.com/techspeque/gaia` with `cmd/gaia` and the `internal/` packages in OVERVIEW §2.3, each compiling with a placeholder
- Pin the Go version in `go.mod` and commit `go.sum`
- Add a `Makefile` whose `verify` target runs `gofmt -l`, `go vet ./...`, `staticcheck ./...` and `CGO_ENABLED=0 go test ./...`
- Add a test that fails when a leaf package imports another leaf (the §2.3 dependency rule, with `proxy → creds` as the one exception) or when anything imports `github.com/techspeque/metis`
- Add a `make build-all` target that cross-compiles the six targets in ADR-0002 rule 5 with CGO disabled
- Add a GitHub Actions workflow running `make verify` on linux and macOS and `make build-all` on linux
- Add LICENSE (MIT), a README stating pre-alpha status, SECURITY.md with a private reporting route and CONTRIBUTING.md with the build and test commands
- Implement `gaia --version` printing version, commit and platform

Suggested packages:
- `cmd/gaia`
- `internal/core`
- `internal/host`
- `internal/node`
- `internal/store`
- `internal/net`

Acceptance criteria:
- [ ] `make verify` passes on a clean checkout
- [ ] `gaia --version` prints a version, commit and platform
- [ ] The dependency-rule test fails when a leaf-to-leaf import or a Metis import is added
- [ ] `make build-all` produces six binaries, and the build fails if CGO is enabled
- [ ] CI is green on the integration branch
- [ ] The repository contains no real hostname, account or personal name

---

## Workstream 0.2: Core types and contract constants

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.1

Tasks:
- Define typed, prefixed ids for node and session, with parsing and display
- Define node state (`online`, `offline`) and session state (`starting`, `running`, `exited`, `stopped`, `failed`, `lost`) with their legal transitions
- Define the reserved exit codes for Gaia's own failures exactly as `docs/contracts/launcher-v1.md` lists them, with a test that reads the table from that document
- Define the typed error with stable codes and the `{code, message, details}` shape
- Define the API version constant and the request and response types for `/api/v1/nodes`, shared by CLI, host and node
- Add a redaction type for secret values whose `String`, `GoString`, `Format` and JSON encoding never reveal the value

Suggested packages:
- `internal/core`

Acceptance criteria:
- [ ] Ids round-trip through string form and reject a wrong prefix
- [ ] Every illegal state transition is rejected by a test
- [ ] The exit-code constants and the contract document cannot disagree without a test failing
- [ ] Formatting or encoding a redacted value in any way does not reveal it
- [ ] `internal/core` imports no other package of this module and performs no I/O

---

## Workstream 0.3: SQLite store and migrations

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.2

Tasks:
- Add `modernc.org/sqlite` in WAL mode with one writer (ADR-0006 rules 2 and 3)
- Add a runner for numbered, embedded, forward-only migrations that runs at start
- Write the first migration: the node table of OVERVIEW §2.2
- Implement create, get, list, update-last-seen and remove on node
- Refuse to open a database whose schema version is newer than the binary, with a typed error
- Back up the database file before applying a migration to an existing database
- Create the state directory at mode 0700 and refuse one that is readable by group or others

Suggested packages:
- `internal/store`

Acceptance criteria:
- [ ] A fresh state directory migrates to the current schema and reopens cleanly
- [ ] Opening a newer-schema database fails with the typed error and changes nothing
- [ ] A backup file exists after a migration on an existing database
- [ ] A state directory with mode 0755 is refused
- [ ] The store builds and its tests pass with CGO disabled

---

## Workstream 0.4: Host daemon, local socket and logging

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.3

Tasks:
- Implement `gaia host [--state DIR] [--no-node]`: open the store, bind the local socket, serve the API, shut down cleanly on SIGTERM and SIGINT
- Bind a unix socket at mode 0600 in a user-only runtime directory; refuse to start if it cannot be bound as specified, and remove a stale socket only after proving no daemon answers on it (ADR-0004 rule 3)
- Serve `/healthz` with no machine details, `/api/v1/version` and `/api/v1/nodes`
- In local-only mode, register the host's own machine as a node in-process and keep its `last_seen` current
- Emit structured JSON logs to stderr through a logger that redacts registered secret values
- Refuse the daemon roles on Windows with a clear message

Suggested packages:
- `internal/host`
- `internal/node`
- `cmd/gaia`

Acceptance criteria:
- [ ] The socket is mode 0600 and a second user on the machine cannot connect to it
- [ ] A fresh `gaia host` opens no TCP listener at all, checked by listing the process's sockets in a test
- [ ] `/healthz` returns no hostname, version or path
- [ ] `/api/v1/nodes` lists the local machine as `online` within one heartbeat interval
- [ ] A registered secret value written to the logger does not appear in its output
- [ ] SIGTERM removes the socket and exits 0

---

## Workstream 0.5: CLI client and version negotiation

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.4

Tasks:
- Implement the CLI's client for the local socket; the CLI never starts `tsnet` (ADR-0004 rule 2)
- Implement `gaia nodes` with text output and `-o json`
- Negotiate the API version on connect; an incompatible CLI and daemon fail with the command that fixes it
- With no local daemon, fail with one clear message naming `gaia host` and `gaia node`, and a reserved exit code
- Record the `gaia nodes -o json` fixture under `testdata/contract/` with a test that fails when the shape changes

Suggested packages:
- `cmd/gaia`
- `internal/core`

Acceptance criteria:
- [ ] `gaia nodes -o json` against a fresh local-only host matches the recorded fixture apart from ids and timestamps
- [ ] With no daemon running, `gaia nodes` prints the message and exits with the documented code, in under 200 ms
- [ ] A CLI one API version behind the daemon fails with the upgrade instruction
- [ ] No code path reachable from a CLI command imports `tsnet`, checked by a test on the import graph

---

## Workstream 0.6: Tailnet join

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.4

Tasks:
- Implement `gaia host --tailnet [--control-url URL]`: join the tailnet through embedded `tsnet`, keeping tsnet state inside the host state directory
- Serve `/api/v1` over HTTPS on the tsnet listener only
- Authorise each tailnet request by peer identity from `WhoIs` plus a host-side allowlist; a peer not on the allowlist gets `403` and a log line (ADR-0004 rule 5)
- Read the auth key from an environment variable or a file, never from an argument, and never log it
- Keep all `tsnet` use inside `internal/net`
- Test against a local test control server; no Tailscale account in CI

Suggested packages:
- `internal/net`
- `internal/host`

Acceptance criteria:
- [ ] With `--tailnet`, the host answers `/api/v1/version` over the tailnet to an allowlisted peer and returns `403` to any other
- [ ] Without `--tailnet`, no tsnet code runs and no network call is made, checked in a test
- [ ] No listener is bound to 0.0.0.0 or to a LAN address in either mode
- [ ] The auth key appears in no log line, process argument or API response
- [ ] The control URL can point at Tailscale or Headscale, and the README covers both

---

## Workstream 0.7: Node role, registration and heartbeat

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.5, 0.6

Tasks:
- Implement `gaia node --host NAME [--join] [--state DIR]`: join the tailnet, register with the host and bind the node's own 0600 local socket
- Register with name, tailnet identity, os/arch and Gaia version; re-registration of a known identity updates the row and creates no duplicate
- Send a heartbeat on an interval; the host marks a node `offline` after a configurable number of missed heartbeats
- Negotiate versions on registration: the host accepts nodes at its own API version and one behind, and refuses others with a typed error
- Forward the CLI's `/api/v1` requests from the node's local socket to the host over the tailnet, so `gaia nodes` works on any machine
- Reconnect with backoff when the host is unreachable; a node never exits because the host is down
- Implement `gaia nodes remove NAME`

Suggested packages:
- `internal/node`
- `internal/host`
- `internal/net`

Acceptance criteria:
- [ ] Two nodes joined through the test control server appear in `gaia nodes -o json` on the host and on each node
- [ ] Stopping a node turns it `offline` after the configured interval; starting it again turns it `online` with the same id
- [ ] A node two API versions behind is refused with the typed error and the upgrade instruction
- [ ] Stopping the host leaves the node process running, and it re-registers when the host returns
- [ ] The node's state directory holds no credential and no tailnet auth key after joining

---

## Workstream 0.8: Release pipeline and install script

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** foundation
- **Blocked by:** 0.5

Tasks:
- Add GoReleaser and a release workflow building the six targets on a `v*` tag, with a dry run on manual dispatch
- Publish checksums, keyless cosign signatures and an SBOM with each release (ADR-0012 rule 3)
- Write `install.sh`, modelled on Metis's and Ship's, that verifies checksum and signature before installing
- Run `govulncheck` and a licence check in CI
- Add a smoke test that the release binary makes no network request on any Phase 0 command in local-only mode
- List in the README exactly the platforms CI builds and tests

Suggested packages:
- `.github/workflows`
- `scripts/`

Acceptance criteria:
- [ ] A dry run produces six archives, a checksum file and an SBOM, and publishes nothing
- [ ] `install.sh` refuses an archive whose checksum or signature does not match
- [ ] The linux binaries are static and run on a machine with no Go toolchain
- [ ] `govulncheck` fails the build on a known vulnerability
- [ ] The binary makes no network request at start-up or on any local-only command

---

## Phase Gate

> This section defines how Phase 0 is validated as a composed system.
> It becomes a `gate` slice automatically (phase-0-gate, risk high,
> blocked by every workstream in the phase).

Composition scenarios to validate:
- [ ] On one machine, install a release build with `install.sh` and run `gaia host`; `gaia nodes -o json` lists that machine `online`, and the process has no TCP listener
- [ ] On two real machines on a tailnet, `gaia host --tailnet` on one and `gaia node --host … --join` on the other; `gaia nodes` on both lists both
- [ ] Suspending or stopping the node machine shows it `offline`; bringing it back shows it `online` without a second join
- [ ] Restarting the host keeps both nodes registered with the same ids
- [ ] Listing listening sockets on both machines shows Gaia only on the tailnet interface, 127.0.0.1 and its local socket
- [ ] A tailnet device that is not on the allowlist is refused by the host API
- [ ] Every `-o json` output matches its recorded fixture
- [ ] Both daemons run under a user unit, with no root, and are up after a reboot

Performance / resource checks:
- [ ] `gaia nodes` completes in under 100 ms on the host and under 500 ms on a node
- [ ] Idle resident memory of host and node is measured on the gate machines and recorded in the README
- [ ] Release binary size is recorded in the README
