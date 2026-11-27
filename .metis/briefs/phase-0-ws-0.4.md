# phase-0-ws-0.4 — Host daemon, local socket and logging

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.4

## Goal

`gaia host [--state DIR] [--node]` runs the host daemon: it opens the store,
serves `/healthz`, `/api/v1/version` and `/api/v1/nodes` on a 0600 unix
socket in a user-only runtime directory (and on no TCP listener), logs
structured JSON to stderr through a redacting logger, optionally registers
its own machine as a node in-process, and shuts down cleanly on SIGTERM and
SIGINT; the daemon roles refuse to start on Windows.

## Architectural context

- OVERVIEW §2 (CLI talks only to the local daemon over a user-only socket),
  §2.3 (host and node depend on leaves; leaves import only `core`), §3.3
  (every listener binds to the tailnet, 127.0.0.1 or a user-only socket),
  §5 boundary 4, §8 (structured JSON to stderr, redaction at the logger).
- ADR-0004 rules 1, 3, 4, 8: local socket 0600, no other listener, local-only
  is the default. ADR-0002: one binary, CGO off, six targets compile.
- ADR-0006: the store from 0.3 (`store.Open`, node CRUD).
- Consumes from `internal/core`: `NodeID`, `NewNodeID`, `NodeState`,
  `core.Node`, `ListNodesResponse`, `RegisterNodeRequest`,
  `HeartbeatRequest`, `*core.Error`, `ErrorResponse`, `Secret`, `APIVersion`.
- Consumers: 0.5 (CLI dials the socket with `sock.Path`/`sock.Dial`, reads
  `/api/v1/version`), 0.6 (serves the same handler on tsnet), 0.7 (the
  `node.Registrar` shape goes over HTTP; heartbeat-derived state).

Design decisions:

- **New leaf `internal/sock`** (imports `core` only, so the CLI in 0.5 can
  use it without importing `host`): resolves the runtime directory, binds,
  dials.
  - Runtime dir: `$XDG_RUNTIME_DIR/gaia` when `XDG_RUNTIME_DIR` is set,
    else `os.TempDir()/gaia-<uid>`. Socket `<dir>/gaia.sock`, lock
    `<dir>/gaia.lock`.
  - The directory is created at 0700; an existing one is accepted only if
    `Lstat` shows a real directory (not a symlink), owned by the current
    uid, with `mode & 0077 == 0`. Otherwise refuse with
    `core.CodeInvalidArgument` (details `path`, `mode`).
  - `Listen` takes an exclusive non-blocking `flock` on the lock file
    (0600) held for the listener's life; a held lock is `core.CodeConflict`
    ("another daemon is running"). With the lock held, an existing path is
    removed only if it is a socket and dialling it is refused (no daemon
    answers); a socket that answers is `CodeConflict`, a non-socket file is
    `CodeInvalidArgument`. Then `net.Listen("unix")`, `chmod 0600`, and an
    `Lstat` check that the mode is exactly 0600, else refuse.
  - Socket paths longer than the platform's `sun_path` limit (104 on
    darwin, 108 on linux, minus the NUL) are refused with a clear message
    naming `XDG_RUNTIME_DIR`.
  - Accepted connections are checked by peer credentials
    (`SO_PEERCRED` on linux, `LOCAL_PEERCRED` on darwin, through
    `golang.org/x/sys/unix`); a peer whose uid is not the daemon's is
    closed before any byte is read. Belt and braces on top of 0600/0700.
  - `Close` closes the listener, removes the socket and releases the lock.
  - Windows and other non-linux/darwin unix: `Listen` returns
    `core.CodeUnavailable`; files compile on all six targets.
- **New leaf `internal/logging`**: `logging.New(w io.Writer, r *Redactor)
  *slog.Logger` writes JSON (slog `JSONHandler`). `Redactor` is a
  concurrency-safe set of secret values; `Register(core.Secret)` adds one
  (empty ignored). The handler replaces every occurrence of a registered
  value with `core.Redacted` in the message and in every string-valued
  attribute (including groups, `WithAttrs`, `WithGroup`, errors and
  `fmt.Stringer`/any values, by formatting them to strings first), longest
  value first. Not in `core` because `core` may not import `log/slog` or
  `io` (core noio test).
- **`internal/host`**:
  - `host.New(st *store.Store, opts Options) *Server`; `Options{Version
    string; HeartbeatInterval time.Duration; MissedHeartbeats int; Logger
    *slog.Logger; Now func() time.Time}`; defaults 10 s and 3.
  - `Handler()` routes: `GET /healthz` → `{"status":"ok"}` only;
    `GET /api/v1/version` → `core.VersionResponse{api_version,
    gaia_version}`; `GET /api/v1/nodes` → `core.ListNodesResponse` with
    `state` `online` when `now - last_seen <= interval × missed`, else
    `offline`. Wrong method → 405 with `core.ErrorResponse`
    (`invalid_argument`); unknown path → 404 (`not_found`); store errors →
    500 (`internal`, generic message, detail logged). Every request is
    logged (method, path, status, duration).
  - `Register(ctx, core.RegisterNodeRequest) (core.Node, error)`: a known
    tailnet identity updates that row (same id, `created_at` kept), else a
    new `node_…` id from `crypto/rand`. `Heartbeat(ctx,
    core.HeartbeatRequest) (core.Node, error)` updates `last_seen`. These
    are in-process only in this slice; no POST route (0.7 adds it).
  - `Serve(ctx, net.Listener) error`: `http.Server` with
    `ReadHeaderTimeout`; on ctx done, `Shutdown` with a 5 s deadline.
- **`internal/node`**: `node.Registrar` interface (`Register`, `Heartbeat`
  as above, implemented by `*host.Server`); `node.RunLocal(ctx, Registrar,
  core.RegisterNodeRequest, interval, *slog.Logger) error` registers once
  (failure is returned), then heartbeats every interval until ctx is done;
  a failed heartbeat is logged and retried next tick, and a `not_found`
  heartbeat re-registers. `node.LocalIdentity = "local"` is the tailnet
  identity of the in-process node in local-only mode; its name is the
  hostname up to the first dot.
- **`internal/store`**: add `UpdateNode(ctx, Node)` (updates name, os,
  arch, gaia_version, api_version, last_seen by id; not_found / conflict /
  invalid_argument as the other methods). Needed for re-registration to
  update a row in place (plan 0.7 "updates the row"); the 0.3 brief left it
  to the slice that needs it.
- **`internal/core/api.go`**: add `HealthzPath`, `VersionPath`,
  `HealthResponse{Status}`, `VersionResponse{APIVersion, GaiaVersion}` so
  CLI (0.5) and host share them.
- **`cmd/gaia`**: `host` and `node` subcommands. On Windows both print
  `gaia host: the daemon roles do not run on Windows yet` (resp. `node`) and
  exit 1. `gaia node` elsewhere prints that it arrives in a later release
  and exits 1. `gaia host`: flags `--state DIR` (default
  `$XDG_STATE_HOME/gaia`, else `~/.local/state/gaia` on linux,
  `~/Library/Application Support/gaia` on darwin) and `--node`; bad flags
  exit 2. Start order: logger → store → socket → serve → (`--node`) local
  node; any start failure is logged and exits 1. SIGTERM/SIGINT: stop the
  node loop, shut down the server, close the socket (removed), close the
  store, exit 0.
- Direct dependency: `golang.org/x/sys` (already in the module graph via
  `modernc.org/sqlite`) for peer credentials and `flock`; ADR-0002 rule 3.

## Declared file scope

- **owned_paths:** cmd/gaia/, internal/host/, internal/node/, internal/sock/, internal/logging/, internal/core/api.go, internal/store/node.go, internal/store/node_test.go, go.mod, go.sum
- **read_only_paths:** internal/core/, internal/store/, OVERVIEW.md, .metis/adr/, deps_test.go, Makefile, docs/contracts/launcher-v1.md

## Definition of Done

- The socket is mode 0600 inside a 0700 directory owned by the user; a
  directory with group/other bits, a symlinked directory, or one owned by
  another uid is refused; a peer with another uid is disconnected by the
  peer-credential check.
- A fresh `gaia host`, with and without `--node`, has no TCP or UDP socket,
  checked by listing the child process's sockets (`/proc/<pid>` on linux,
  `lsof` on darwin) in a test.
- `/healthz` returns only `{"status":"ok"}`: no hostname, version or path.
- With `--node`, `/api/v1/nodes` lists the local machine `online` within one
  heartbeat interval; without it the list is empty. A node whose last
  heartbeat is older than interval × missed is `offline`.
- A registered secret value written to the logger, in the message, an
  attribute, a group, an error or a formatted value, does not appear in the
  output.
- SIGTERM (and SIGINT) removes the socket and exits 0.
- A second `gaia host` on the same runtime dir fails while the first runs;
  a stale socket left by a killed daemon is removed and the next start
  succeeds; a non-socket file at the socket path is refused and kept.
- The daemon roles exit 1 with the Windows message on Windows (tested
  through the OS switch with an injected GOOS).
- `make verify` passes; `GOOS=windows`, `linux`, `darwin` × amd64/arm64
  builds compile with CGO off; the dependency-rule test passes with the
  two new leaves.

## Test plan

- `internal/sock`: dir resolution (XDG set/unset); dir refusals (0755,
  symlink, file); fresh dir 0700; socket 0600; second Listen →
  conflict; stale socket removed; live socket → conflict; regular file at
  path → refused and untouched; too-long path refused; Close removes the
  socket; peer-credential check rejects an injected foreign uid and accepts
  the own uid; Dial round-trip.
- `internal/logging`: secret in message, string attr, nested group,
  `WithAttrs`, `WithGroup`, error value, Stringer, `%v` of a struct, and
  overlapping values; output is valid JSON; unregistered values pass
  through; concurrent Register/log under `-race`-safe code.
- `internal/host`: handler tests with `httptest` over a real store:
  healthz body exact, version body, nodes empty, online/offline by
  injected clock, 405/404 error shape, register new / re-register same
  identity keeps id and updates fields, heartbeat not_found.
- `internal/node`: `RunLocal` with a fake registrar: registers then
  heartbeats; re-registers on not_found; returns on ctx cancel; register
  failure returned.
- `internal/store`: `UpdateNode` updates fields, not_found, conflict on
  name, invalid input.
- `cmd/gaia`: the test binary re-executes itself as `gaia host` in a
  subprocess with temp `XDG_RUNTIME_DIR` and `--state`: `/healthz` and
  `/api/v1/nodes` over the socket (with `--node`: local node `online`;
  without: empty); no inet sockets of the child; SIGTERM → exit 0 and
  socket gone; SIGINT likewise; second instance fails; Windows refusal via
  injected GOOS; bad flag exits 2.

## Out-of-scope touches

None.

## Report

- Files: `cmd/gaia/{main,daemon,daemon_test}.go`;
  `internal/host/{host,host_test}.go`, `internal/node/{node,node_test}.go`
  (placeholder `doc.go` removed from both); `internal/sock/{sock,sock_unix,
  sock_other,peercred_linux,peercred_darwin,sock_test}.go`;
  `internal/logging/{logging,logging_test}.go`; `internal/core/api.go`;
  `internal/store/{node,node_test}.go`; `go.mod` (`golang.org/x/sys` made
  direct; `go.sum` unchanged).
- `make verify` green; `go test -race` green on host, node, sock, logging,
  cmd/gaia; `go vet ./...` clean for linux/amd64, darwin/amd64,
  windows/arm64; `make build-all` builds all six targets with CGO off.
- The daemon tests run the test binary as `gaia host` in a child process.
  On darwin the inet-socket check uses `lsof` (ran, not skipped); the
  linux `/proc` path compiles and is exercised in CI on ubuntu.
  `TestSocketListerSeesTCP` proves the checker finds a TCP listener.
- "A second user cannot connect": the socket is 0600 in a 0700
  directory owned by the user, and the peer-credential check closes any
  peer with another uid. CI has no second user, so the check is tested with
  an injected expected uid; a foreign-owned directory refusal is not
  tested (needs root to chown).
- Redaction is done on the encoded JSON line, which also covers attributes
  bound before a secret was registered; the raw and JSON-escaped forms of
  each value are matched.
- Deviation from the design text: `store.UpdateNode` also updates
  `tailnet_identity` (it is the full mutable row); `created_at` is kept.
- `--node` registers by listing nodes and matching the identity (`local`);
  no lookup-by-identity was added to the store.
- Not done here: a lock on the state directory itself. Two daemons on
  the same state dir but different runtime dirs are not prevented.
