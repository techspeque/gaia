# phase-1-ws-1.5 — Attach and interactive sessions

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.5

## Goal

Terminal I/O over WebSocket at `/api/v1/sessions/{id}/tty`, from host to
node to supervisor; `gaia attach SESSION` (raw mode, resize forwarding, a
detach key, the caller's terminal restored on every exit path) and
`gaia run --interactive -- argv…` (start with a PTY, then attach), with
recent scrollback replayed on attach, input accepted from every attached
client (ADR-0008 rule 5), and the WebSocket origin checked.

## Architectural context

- OVERVIEW §2 (CLI talks only to its local daemon; a node's daemon forwards
  `/api/v1` to the host), §2.3 (dependency rule: leaves import only core;
  `cmd/gaia/cli` imports only core, sock and non-module packages), §3.3
  (listeners; a session survives its client), §6 (terminal I/O over
  WebSocket at `/api/v1/sessions/{id}/tty`).
- ADR-0004 (tailnet calls authorised by peer identity; the CLI never starts
  tsnet), ADR-0008 rule 5 (many clients attach; input from any is accepted).
- Launcher contract v1 exit codes: 91 node_unavailable, 95 session_lost,
  98 usage, 99 internal.
- Consumes: 1.2's `supervise.Client.Input/Resize/Status`, `ReadState`,
  `OpenScrollback`; 1.3's `node.Manager.Get/SessionDir`; 1.4's
  `host.NodeSessions`/`NodeRouter`, `node.LocalSessions` (and its
  `SessionLogs` follow loop), `node.SessionClient`, `node.SessionHandler`,
  `node.Forwarder`, `cli.Client`.
- Consumers: Phase 3 UI (live terminal over the same WebSocket, same-origin);
  1.7 (`--wait` shares the run flags).

Design decisions:

- **Library.** `github.com/coder/websocket` (ISC, stdlib-only, already in
  the module graph through tailscale.com) for WebSocket, and
  `golang.org/x/term` (BSD, already in the graph) for raw mode and size.
  Both move from indirect to direct in `go.mod`. No compression.
- **Wire protocol** (new `internal/core/tty.go`, contract for CLI and UI):
  `SessionTTYPath(id)` on the host, `NodeSessionTTYPath(id)` on the node
  API. Binary frames carry raw bytes: output server→client, input
  client→server. Text frames carry one JSON `TTYMessage{type, …}`:
  `hello` (server, first message: `mode`, `offset` of the first output
  byte), `resize` (client: `rows`, `cols`), `notice` (server: `message`,
  e.g. input refused on a headless session). The server closes with
  status 1000 (normal) once the session has ended and every output byte
  was sent; any other close or a cut connection is a drop. An error before
  the upgrade is the usual JSON error with its HTTP status.
- **Origin.** `core.CheckOrigin(origin, host)`: no `Origin` header (the
  CLI, the host's own node API calls) or an `Origin` whose host equals the
  request's `Host` (same-origin, the future UI) is admitted; anything else,
  including an unparsable or `null` origin, is refused 403 `forbidden`
  before any routing, with a warn log line. Checked by the host's tty
  handler and by the node API's; coder/websocket's own same-origin check
  stays on as a second guard.
- **New leaf `internal/tty`** (imports core + coder/websocket only):
  `Terminal` interface (`Mode`, `Offset`, `io.Reader` output ending in
  `io.EOF` when the session ended, `Input`, `Resize`, `Close`);
  `Serve(w, r, Terminal, log)` (accept, hello, pump output, pass input and
  resizes; input to a headless terminal is dropped with one `notice`);
  `Dial(ctx, *http.Client, url) (Terminal, error)` (the host's client of a
  node's tty; a refusal before the upgrade is the node's `*core.Error`).
  The CLI cannot import it (graph rule), so it speaks the core protocol
  with coder/websocket directly.
- **Node.** `LocalSessions.AttachSession(ctx, id) (tty.Terminal, error)`:
  mode from the manager; output is `SessionLogs(follow)` from the replay
  offset, through a pipe, so running and ended sessions and cut supervisor
  streams are handled as for `gaia logs --follow`. Replay offset:
  `max(start, size − ReplayBytes)` with `ReplayBytes = 128 KiB`, size from
  the supervisor's status while running, from the state file's
  `scrollback_end` once ended. Input and resize go to the supervisor (one
  call each; the supervisor refuses them for headless sessions). The node
  API serves `GET …/node/sessions/{id}/tty` (peer check, then origin check,
  then `tty.Serve`). `SessionClient.AttachSession` dials it with
  `tty.Dial`.
- **Host.** `NodeSessions` gains `AttachSession`. `GET
  /api/v1/sessions/{id}/tty` on the socket and tailnet muxes: origin check,
  session row, route to its node (unreachable → 503 reason
  `node_unavailable`), attach, `tty.Serve`. The host never interprets the
  bytes. The request log wrapper lets the connection be hijacked.
- **Forwarder.** A node's local socket forwards the upgrade to the host
  unchanged (`httputil.ReverseProxy` handles 101); the recovering transport
  keeps a 101 response's body writable.
- **CLI.** `gaia attach SESSION`: connect, dial the tty over the local
  socket, read `hello`. Interactive: if stdin is a terminal, raw mode for
  the attachment, the size sent at once and on every SIGWINCH; stdin bytes
  are sent as input until the detach key **Ctrl-]** (0x1d), whose
  preceding bytes are sent and which itself is not. Headless: no raw mode,
  nothing sent; stderr says it is watch-only; Ctrl-] or Ctrl-C detaches.
  The terminal is restored by a deferred restore on every return, also on
  SIGINT/SIGTERM/SIGHUP to the CLI (which end the attachment). Exit codes:
  detach → 0; the session ended → the agent's exit code once the host
  shows it terminal (1 if it has none, e.g. failed; 0 with a note if the
  host has not caught up within 5 s); connection dropped → 95
  session_lost with "it may still be running: gaia attach ID"; refusals
  before attaching map as for the other commands (unknown session → 1).
  `gaia run --interactive -- argv…` starts the session with mode
  `interactive` and attaches; `--interactive` and `--detach` are mutually
  exclusive and exactly one is required; `--slice` stays Phase 3.
  `cli.Env` gains `Stdin` and `Term` (the caller's terminal, nil when stdin
  is not one), so tests drive attach without a real terminal.

## Declared file scope

- **owned_paths:** go.mod, go.sum, internal/core/tty.go, internal/core/tty_test.go, internal/tty/, internal/host/host.go, internal/host/session.go, internal/host/session_test.go, internal/host/tty_test.go, internal/node/sessionapi.go, internal/node/sessionapi_test.go, internal/node/tty.go, internal/node/tty_test.go, internal/node/remote.go, internal/node/remote_test.go, cmd/gaia/main.go, cmd/gaia/node.go, cmd/gaia/daemon.go, cmd/gaia/attach_test.go, cmd/gaia/cli/attach.go, cmd/gaia/cli/attach_test.go, cmd/gaia/cli/term_unix.go, cmd/gaia/cli/term_other.go, cmd/gaia/cli/nodes.go, cmd/gaia/cli/sessions.go, cmd/gaia/cli/sessions_test.go
- **read_only_paths:** OVERVIEW.md, .metis/adr/, .metis/plans/phase-1-plan.md, docs/contracts/launcher-v1.md, internal/core/, internal/net/, internal/node/session.go, internal/supervise/, internal/store/, internal/sock/, testdata/, deps_test.go, cmd/gaia/cli/graph_test.go, cmd/gaia/sessions.go, cmd/gaia/sessions_test.go, Makefile

## Definition of Done

- `gaia run --interactive -- fakeagent …` starts the agent with a PTY and
  attaches; Ctrl-] detaches with exit 0 and the session keeps running.
- Attaching from a second machine (node-b on a test tailnet) to a session
  shows the replayed output; with two clients attached both see new
  output, and input from either reaches the agent.
- The caller's terminal (a real PTY in the test) has its original termios
  after detach, after the session ends and after the daemon is killed
  mid-attachment (exit 95).
- Attaching to a headless session shows its output, sends nothing, and the
  server drops any input frame with a notice.
- A WebSocket upgrade with a foreign `Origin` is refused 403 on the host
  socket, the host tailnet and the node API; same-origin and no-origin are
  admitted.
- Attach to an unknown session exits 1; to a session whose node is down,
  91.
- `make verify` green; Windows build still compiles; the CLI import-graph
  test still passes.

## Test plan

- `internal/core/tty_test.go`: `CheckOrigin` table (absent, same host,
  case, foreign, other port, `null`, garbage); `TTYMessage` JSON shape.
- `internal/tty/tty_test.go`: `Serve` + `Dial` over `httptest` with a fake
  terminal: hello, output in order, input and resize passed, headless input
  dropped with a notice, normal close at EOF, pre-upgrade error decoded as
  `*core.Error`.
- `internal/node/tty_test.go`: `LocalSessions.AttachSession` against a real
  `Manager` and the fake agent: interactive echo and resize, replay of
  output written before attach (and only the last `ReplayBytes`), ended
  session replays and ends, headless input refused; node API tty route
  refuses a foreign origin and a non-host peer; `SessionClient.AttachSession`
  through the node API.
- `internal/node/remote_test.go`: the forwarder passes a WebSocket upgrade
  through, also while the link is not ready.
- `internal/host/tty_test.go` with a fake router: host tty route bridges to
  the node terminal, foreign origin 403 (socket and tailnet), unknown
  session 404, node unreachable 503 `node_unavailable`, through the
  request-log wrapper.
- `cmd/gaia/cli/attach_test.go` with a fake daemon and fake terminal: raw
  mode entered and restored on detach, session end and drop; detach key
  splitting; resize on start and on signal; headless sends nothing and no
  raw mode; exit codes (0, agent code, 95, 1); `run --interactive` flag
  rules (98).
- `cmd/gaia/attach_test.go`: `TestAttachLocalOnly` (host `--node`, tailnet
  trap armed: run --interactive under a real PTY, echo, detach restores
  termios, re-attach replays, two clients, session end restores termios and
  returns the agent's code, headless attach, foreign-origin upgrade on the
  socket refused, daemon killed mid-attach → 95 and termios restored) and
  `TestAttachOverTailnet` (interactive session on node-b attached from the
  host machine and from node-b at once, input from both, replay).

## Out-of-scope touches

None.

## Report

Filled at implementation.
