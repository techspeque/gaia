# phase-0-ws-0.5 — CLI client and version negotiation

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.5

## Goal

The `gaia` CLI talks to the daemon on its own machine over the local socket
only: `gaia nodes [-o text|json]` lists the registry, the API version is
checked on every connect and a mismatch fails with the command that fixes
it, a missing daemon fails fast with one clear message and a reserved exit
code, and the `gaia nodes -o json` shape is locked by a recorded fixture.

## Architectural context

- OVERVIEW §2 (CLI talks only to the local daemon over a user-only socket;
  only daemons join the tailnet), §6 (every list command supports
  `-o json`), §8 (versions negotiated). ADR-0004 rule 2 (the CLI never
  starts `tsnet`), rule 3 (user-only socket).
- `docs/contracts/launcher-v1.md` exit codes: 90 `no_daemon`, 96
  `version_mismatch`, 98 `usage`, 99 `internal`.
- Consumes from 0.4: `sock.Dir`, `sock.Path`, `sock.Dial`; the daemon's
  `GET /api/v1/version` (`core.VersionResponse`) and `GET /api/v1/nodes`
  (`core.ListNodesResponse`); `core.ErrorResponse` on non-2xx.
- Consumers: every later CLI command (`gaia ls`, `run`, …) uses the same
  client; Phase 3 and Metis read `gaia nodes -o json`.

Design decisions:

- **New package `cmd/gaia/cli`** holds the client and the client commands.
  It imports only `internal/core`, `internal/sock` and the standard library,
  so the import-graph test can prove that nothing a CLI command reaches
  links `tsnet` even after 0.6 makes `internal/host` / `internal/net`
  import it. `cmd/gaia` only dispatches `nodes` to it. Under `cmd/` it is
  outside the leaf rule of `deps_test.go`, which is unchanged.
- **Client** (`cli.Client{Dir string; APIVersion int; Timeout
  time.Duration}`; zero values mean `sock.Dir()`, `core.APIVersion`, 10 s):
  HTTP over `sock.Dial`. `Connect(ctx)` does `GET /api/v1/version` and
  compares: equal → ok; otherwise a `*core.Error` with
  `CodeVersionMismatch`. A 404 on the version path means a daemon that no
  longer speaks this API: treated as a newer daemon. Every command calls
  `Connect` before its own request. Non-2xx bodies decode as
  `core.ErrorResponse`.
- **Mismatch messages** (exit 96), naming both versions:
  - CLI older than the daemon: `gaia: this gaia (<v>, API v<a>) is older
    than the daemon (<dv>, API v<b>): install gaia <dv> on this machine
    and run the command again`.
  - CLI newer than the daemon: `gaia: the daemon (<dv>, API v<b>) is older
    than this gaia (<v>, API v<a>): restart the daemon with this binary
    (stop `gaia host` or `gaia node` and start it again)`.
  The CLI requires an exact API match; "host serves N and N-1" (OVERVIEW
  §8) is about nodes and lands with 0.7.
- **No daemon** (exit 90): runtime directory or socket missing, connection
  refused, or platform without the socket → `gaia: no Gaia daemon is
  running on this machine (<socket path>): start one with `gaia host`
  (add --node on a single machine), or `gaia node --host NAME` to join
  another host`. The dial has a 1 s timeout; the missing/refused cases
  return at once.
- **`internal/sock` client hardening**: `Dial` refuses a runtime directory
  that is not a real directory owned by the user with no group/other bits
  (same check as `Listen`, without creating it), and closes a connection
  whose peer uid is not the user's (the client's half of the peer check).
  Missing directory/socket, refused connection and unsupported platform are
  wrapped with a new sentinel `sock.ErrNoDaemon`. An unsafe directory or a
  foreign peer is a `*core.Error` (`invalid_argument` / `forbidden`) and
  the CLI exits 99 with its message.
- **`gaia nodes [-o text|json]`** (also `--output`): JSON is
  `core.ListNodesResponse` re-encoded with two-space indent and a trailing
  newline, `"nodes": []` when empty. Text is a `tabwriter` table `NAME
  STATE OS/ARCH VERSION LAST SEEN` with last seen relative to now (`12s
  ago`, `5m ago`, `3h ago`, else the RFC 3339 time), or `no nodes
  registered` when empty. Bad flag, unknown `-o` value or extra argument →
  usage on stderr, exit 98. Other failures exit 99. All CLI errors are one
  line prefixed `gaia: ` on stderr.
- **Fixture** `testdata/contract/gaia/api-v1/nodes.json`: a recorded
  `gaia nodes -o json` with one node, using neutral values (no real
  hostname). `testdata/contract/README.md` lists it.

## Declared file scope

- **owned_paths:** cmd/gaia/main.go, cmd/gaia/main_test.go, cmd/gaia/nodes_test.go, cmd/gaia/cli/, internal/sock/sock.go, internal/sock/sock_unix.go, internal/sock/sock_other.go, internal/sock/sock_test.go, testdata/contract/gaia/, testdata/contract/README.md
- **read_only_paths:** internal/core/, internal/host/, internal/node/, cmd/gaia/daemon.go, cmd/gaia/daemon_test.go, deps_test.go, docs/contracts/launcher-v1.md, OVERVIEW.md, .metis/adr/

## Definition of Done

- `gaia nodes -o json` against a fresh `gaia host --node` matches
  `testdata/contract/gaia/api-v1/nodes.json` apart from id, last_seen and
  the machine-specific name/os/arch/gaia_version (which are checked against
  the live machine instead); the fixture test fails when a field is added,
  removed, renamed or changes JSON type.
- With no daemon running, `gaia nodes` prints the one-line message naming
  `gaia host` and `gaia node` and exits 90, in under 200 ms.
- A CLI one API version behind the daemon exits 96 with the install
  instruction; one ahead exits 96 with the restart instruction.
- A test on the import graph (`go list -deps`) fails if `cmd/gaia/cli`
  reaches `tailscale.com/...`, `internal/net`, `internal/host`,
  `internal/node` or `internal/store`; a unit test proves the check flags
  such a dependency.
- `sock.Dial` refuses a group-readable runtime directory and a symlinked
  one, and maps a missing directory, missing socket and refused socket to
  `ErrNoDaemon`.
- `make verify` passes; the six targets build with CGO off.

## Test plan

- `cmd/gaia/cli`: client against an `httptest`-style server on a real
  `sock.Listen` in a temp dir: matching version ok; daemon API+1 → 96 and
  install text; daemon API−1 (client `APIVersion` injected) → 96 and
  restart text; 404 on version → 96; non-2xx error body surfaced; no
  daemon (empty dir, missing dir) → 90 with the message, timed < 200 ms.
  `nodes` output: JSON shape and indent, empty list `[]`, text table and
  relative times with an injected clock, empty text message, bad `-o`,
  bad flag and extra argument → 98. Fixture: re-encoding the decoded
  fixture reproduces it byte for byte, and the key set and JSON types of
  an encoded `core.Node` equal the fixture's. Import graph: `go list -deps`
  check plus the non-vacuous unit test.
- `internal/sock`: `Dial` refusals (0755 dir, symlinked dir), missing dir,
  missing socket, stale socket → `ErrNoDaemon`; round trip still works.
- `cmd/gaia/nodes_test.go`: the test binary runs `gaia host --node` as a
  child (helpers from `daemon_test.go`), then `gaia nodes -o json` and
  `gaia nodes` in-process with that `XDG_RUNTIME_DIR`: fixture match with
  the local node `online`; text output lists it. With no daemon, the real
  binary (child process) exits 90 with the message.

## Out-of-scope touches

None.

## Report

- Files: `cmd/gaia/main.go` (dispatch `nodes`, usage line);
  `cmd/gaia/cli/{client,nodes,client_test,nodes_test,graph_test}.go`;
  `cmd/gaia/nodes_test.go`; `internal/sock/{sock,sock_test}.go`;
  `testdata/contract/gaia/api-v1/nodes.json`;
  `testdata/contract/README.md`. `sock_unix.go`, `sock_other.go` and
  `main_test.go` were declared but needed no change.
- `make verify` green; `go test -race ./cmd/... ./internal/sock/` green;
  `go vet ./...` clean with GOOS=windows and GOOS=linux; `make build-all`
  builds all six targets with CGO off.
- No-daemon timing is asserted (< 200 ms) in-process in
  `cli.TestNoDaemon` for a missing runtime dir and an empty one; the child
  process test `TestNodesNoDaemonBinary` checks exit 90 and the message
  without a timing bound (process start-up would make it flaky in CI).
- The import-graph test is an allowlist: within the module only
  `cmd/gaia/cli`, `internal/core`, `internal/sock`, and nothing from
  `tailscale.com`, for GOOS linux, darwin and windows. It shells out to
  `go list -deps`, so the `go` tool must be on PATH when tests run.
- Non-mismatch daemon errors print as `code: message` (e.g. `internal:
  internal error`); an unsafe runtime directory exits 99, not 90.
- `-h` on `gaia nodes` prints usage and exits 0.
- The live fixture test also checks `tailnet_identity` (`local`),
  `api_version` and `state` (`online`) against the fixture's values.
