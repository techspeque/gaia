# phase-1-ws-1.4 — Sessions over the host: run, ls, stop, logs

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.4

## Goal

Make sessions usable from the CLI through the host: the host's session API
(start on a named node, list, get, stop, read scrollback from an offset),
routed to the node over the tailnet or in-process in local-only mode, and
`gaia run --node N --detach -- argv…`, `gaia ls [-o json]`,
`gaia stop SESSION` and `gaia logs SESSION [--follow]`.

## Architectural context

- OVERVIEW §2 (host holds the registry; CLI talks only to its local daemon;
  only daemons join the tailnet), §2.3 (dependency rule: `host` and `node`
  import leaves only; `host` never imports `node`), §3.3 (listeners only on
  the tailnet, 127.0.0.1 or the user-only socket; a session survives its
  client), §6 (CLI commands, host API under `/api/v1`, `-o json` on lists).
- ADR-0004 (machine-to-machine over the tailnet; CLI never starts tsnet;
  tailnet calls authorised by peer identity), ADR-0008 (supervisor per
  session; reports and re-registration from 1.3).
- Launcher contract v1 exit codes: 91 node_unavailable, 95 session_lost,
  96 version_mismatch, 97 start_failed, 98 usage, 99 internal.
- Consumes: `core.Session`, `ListSessionsResponse`, `StartSessionRequest`
  (+`Validate`, `ValidArgv`), `StopSessionResponse`, `SessionReport` (1.1,
  1.3); `store` session CRUD (1.1); `node.Manager` (`OpenManager`, `Start`,
  `Stop`, `Get`, `SessionDir`, `Run`, `Reporter`) (1.3);
  `supervise.Client.Status/Stream`, `ReadState`, scrollback file layout
  (1.2, 1.3).
- Consumers: 1.5 (attach builds on the routing and the node tailnet API),
  1.6 (`--repo`/`--branch`, checkouts as work dir), 1.7 (`--wait`).

Design decisions:

- **Host → node transport.** Each node daemon serves a small node API on
  its own tailnet device (TLS on port 443, the same self-signed scheme as
  the host), separate from its local socket:
  `POST /api/v1/node/sessions` (start), `POST …/node/sessions/{id}/stop`,
  `GET …/node/sessions/{id}/logs?offset=N&follow=1`. A request is admitted
  only when WhoIs names the caller and its tailnet address is the address
  of the `--host` peer (a `node.PeerCheck` supplied by `cmd/gaia`, so
  `internal/node` stays tsnet-free). The host dials a node by its stored
  tailnet name through `node.HostTransport` (built in `cmd/gaia`).
- **Routing seam.** `host.NodeSessions` (Start, Stop, Logs with core types)
  and `host.NodeRouter` (`Route(core.Node) (NodeSessions, error)`), set as
  `host.Options.Nodes`. Implementations live in `internal/node`:
  `node.LocalSessions` (in-process, over a `*node.Manager`) and
  `node.SessionClient` (HTTP client of the node API). `cmd/gaia` composes a
  router: the in-process node (identity `local`) → `LocalSessions`; any
  other node → `SessionClient` over the tailnet when the host has one;
  otherwise unavailable. In local-only mode no tailnet code runs.
- **Wire types** (new `internal/core/nodeapi.go`): `NodeStartRequest{id,
  mode, argv}`, `NodeSession{id, state, started_at, ended_at, exit_code}`,
  `NodeSessionResponse`, paths, `SessionReportsPath` (`/api/v1/session-reports`),
  `ReportSessionRequest{node_id, report}`, the error detail key `reason`
  whose value is a contract exit-code name, and `ExitCodeByName`.
  `core.ErrNotDelivered`: the request certainly did not reach the node.
- **Start** (`POST /api/v1/sessions`): validate (`StartSessionRequest.Validate`;
  empty argv refused); `repo`/`branch` set → `invalid_argument` (checkouts
  are 1.6); unknown node → `not_found`, offline node → `unavailable`, both
  with reason `node_unavailable`, and no row is created. Then the host
  creates the row (`starting`, new id), then asks the node:
  - node started it → apply its state to the row, answer the session;
  - node refused after creating its record (state `failed` in the error's
    `session_state` detail) → row `failed`, reason `start_failed`;
  - node refused without a record, or the request was not delivered
    (`ErrNotDelivered`, e.g. dial failure) → row `failed` (terminal, so it
    never shows as one that may still run; rows are never deleted, 1.1's
    `sessions_no_delete` trigger), reason `start_failed` resp.
    `node_unavailable`;
  - any other transport failure (outcome unknown) → row `lost`, reason
    `session_lost`; a later report from the node corrects it.
  The work dir is the node user's home directory and the environment is the
  node daemon's (no credentials in this phase, plan §Context).
- **Reports** (1.3's `Reporter`): `host.Server.ReportSession` applies a
  report to the row: equal state → done (a resent seq is a no-op); a
  terminal state → `SetSessionExit`; else `UpdateSessionState`; a
  forbidden move → `conflict` (a final refusal for the node). Over the
  tailnet, `POST /api/v1/session-reports` (tailnet mux only) is accepted
  only from the peer that is the session's node; the node's local socket
  refuses that path, as it refuses heartbeats. `node.Client` gains
  `ReportSession`. The in-process node reports directly.
- **Stop** (`POST /api/v1/sessions/{id}/stop`): a row already terminal is
  returned as it is; else routed to the node, the node's answer applied,
  the session returned. Node unreachable → `unavailable`, reason
  `node_unavailable`.
- **Logs** (`GET /api/v1/sessions/{id}/logs?offset=N&follow=1`): raw bytes,
  `application/octet-stream`, flushed as produced; an error before the
  first byte is the JSON error. On the node: a running session streams from
  its supervisor (to the current size without follow, to the end with
  follow); an ended session is read from the scrollback files (new
  `supervise.OpenScrollback`, which maps logical offsets over `scrollback`
  and `scrollback.prev` using `scrollback_end`). An offset before the kept
  start is `invalid_argument` (detail `start`); a dropped scrollback is
  `not_found`. `gaia logs --follow` resumes from its byte count whenever a
  stream ends while the session is not terminal, and after it ends reads
  the remainder once without follow, so it prints every byte once and ends
  with the session.
- **CLI** (`cmd/gaia/cli/sessions.go`): `gaia run --node N --detach -- argv…`
  prints the session id (only `--detach` in this slice; `--wait` 1.7,
  `--interactive` 1.5, `--repo/--branch` 1.6; `--profile P` accepted and
  stored); `gaia ls [-o text|json]` (JSON is `ListSessionsResponse`, the
  fixture shape); `gaia stop SESSION`; `gaia logs SESSION [--follow]`.
  Errors map to exit codes by the `reason` detail (91/95/97), version
  mismatch 96, bad usage or empty argv 98, an unknown session 1, else 99.
  Streams use a client with no overall timeout.
- **Daemon wiring:** `gaia node` and `gaia host --node` open a
  `node.Manager` at `<state>/sessions` with `[os.Executable(), "supervise"]`
  as supervisor, run its reporter loop, and (node) serve the node API on
  the tailnet. `gaia host` sets `Options.Nodes`.

## Declared file scope

- **owned_paths:** internal/core/nodeapi.go, internal/core/nodeapi_test.go, internal/host/host.go, internal/host/session.go, internal/host/session_test.go, internal/node/sessionapi.go, internal/node/sessionapi_test.go, internal/node/remote.go, internal/node/remote_test.go, internal/supervise/scrollback.go, internal/supervise/scrollback_test.go, cmd/gaia/main.go, cmd/gaia/daemon.go, cmd/gaia/node.go, cmd/gaia/sessions.go, cmd/gaia/sessions_test.go, cmd/gaia/node_test.go, cmd/gaia/cli/client.go, cmd/gaia/cli/sessions.go, cmd/gaia/cli/sessions_test.go
- **read_only_paths:** OVERVIEW.md, .metis/adr/, .metis/plans/phase-1-plan.md, docs/contracts/launcher-v1.md, internal/core/, internal/net/, internal/node/session.go, internal/node/node.go, internal/supervise/, internal/store/, testdata/, deps_test.go, Makefile

## Definition of Done

- `gaia run --node node-b --detach -- fakeagent …` on the host machine
  starts the agent on node B over a test tailnet; `gaia ls` on the host and
  on node B lists it.
- `gaia logs --follow` prints output as it is produced and returns when the
  session ends, every byte once; `gaia logs` without follow prints what
  exists and returns.
- `gaia stop` ends a running session; `gaia ls` shows `stopped` (exit code
  143), not `failed`.
- A start on an offline or unknown node exits 91 and leaves no session row;
  a start that cannot reach a node the host believed online leaves a
  `failed` row, never a live one.
- A run with empty argv exits 98 and sends nothing.
- The same commands work with `gaia host --node` in local-only mode, with
  the tailnet trap armed (no tailnet code runs).
- Reports from a node update the host's rows; a repeated report is a no-op;
  a report from a peer that is not the session's node is refused.
- `make verify` green; Windows build still compiles.

## Test plan

- `internal/core/nodeapi_test.go`: `NodeStartRequest.Validate`,
  `ExitCodeByName` covers every reserved code.
- `internal/supervise/scrollback_test.go`: `TestOpenScrollback` (one
  segment, two segments after rotation, offset before start refused, at
  end empty).
- `internal/host/session_test.go` with a fake router: start happy path,
  unknown/offline node → no row + reason, not delivered → failed row, node
  refusal with record → failed, ambiguous → lost, empty argv / repo set
  refused, reports (apply, repeat no-op, illegal → conflict, wrong node over
  tailnet → forbidden), stop of terminal row not routed, logs streamed and
  pre-stream error JSON.
- `internal/node/sessionapi_test.go`: node API handler against a real
  `Manager` with the fake agent (start, stop, logs running/ended/follow,
  offset before start), peer check refusal; `SessionClient` against that
  handler including `ErrNotDelivered` on dial failure.
- `internal/node/remote_test.go`: `Client.ReportSession`; local socket
  `Handler` refuses the reports path.
- `cmd/gaia/cli/sessions_test.go` with a fake daemon: flag/usage errors
  (98), empty argv, exit-code mapping by reason, `ls` text and JSON,
  `logs --follow` resume after a cut.
- `cmd/gaia/sessions_test.go`: `TestSessionsLocalOnly` (host `--node`,
  trap armed: run/ls/logs --follow/stop/unknown node 91/empty argv 98) and
  `TestSessionsOverTailnet` (host with in-process node + node-b on a test
  control server: run from the host on node-b, ls on both, logs --follow
  through node-b's forwarder, stop → stopped, node-b down → run exits 91,
  no row).
- `cmd/gaia/node_test.go`: the node-state content check allows the new
  `sessions` directory.

## Out-of-scope touches

None.

## Report

Filled at implementation.
