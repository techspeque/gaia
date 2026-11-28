# phase-1-ws-1.2 — Session supervisor

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.2

## Goal

Add the internal `gaia supervise` command: a per-session process that starts
the agent from argv with no shell, owns its pipes or PTY, writes redacted
scrollback, persists the state file, serves a 0600 unix socket (stream from
an offset, input, resize, stop, status) and exits when the child exits or on
stop (ADR-0008); plus a scripted fake agent under `testdata/`.

## Architectural context

- OVERVIEW §2.3 (`internal/supervise` is a leaf: imports `core` only), §3.3
  (no secret on disk or in a log; listeners only on a user-only socket; a
  session survives the loss of its client), §7 (supervisor tests: real PTYs
  against a fake agent in `testdata/`).
- ADR-0008 rules 1–3, 5. ADR-0002 (one binary; CGO off). Launcher contract
  v1 ("standard input is not forwarded"; stop = SIGTERM, SIGKILL after 10 s).
- Consumes from 1.1: `core.SupervisorState` (+`Validate`, version 1),
  `core.SessionID`, `core.SessionMode`, `core.ValidArgv`, `core.Error`,
  `core.ExitStartFailed`.
- Consumers: 1.3 (starts supervisors detached, rediscovers them by session
  directory, reads state files), 1.4/1.5 (logs, attach through the client).

Design decisions:

- **Invocation.** `gaia supervise --session-dir DIR`. The spec arrives as one
  JSON object on stdin, never in argv or on disk: `session_id`, `mode`,
  `argv`, `work_dir`, `env` (the complete child environment), `secrets`
  (values to redact), `rows`/`cols` (initial PTY size), `grace` (stop grace,
  default 10 s). The supervisor creates DIR at 0700 (refuses one that exists
  and is not empty), then writes one JSON line to stdout:
  `{"ok":true,"pid":N}` once the child runs, or `{"ok":false,"error":{…}}`
  and exits 97 (`ExitStartFailed`) when it cannot start. It then closes
  stdout/stderr use, ignores SIGHUP and SIGPIPE, and treats SIGTERM/SIGINT
  to itself as a stop request. Detaching (new session) is the caller's job
  (1.3); tests do it themselves.
- **Files in DIR:** `state.json` (0600, `core.SupervisorState`, written by
  temp file + fsync + rename + dir fsync, on start and on exit),
  `scrollback` (0600, append-only, redacted), `sock` (0600 unix socket,
  removed on exit). A path longer than the platform's sun_path limit is
  refused at start.
- **Child.** `exec` of argv[0] resolved against the spec's `PATH`, no shell,
  in `work_dir`, with exactly `env`. Headless: stdin `/dev/null`, stdout and
  stderr one shared pipe (order kept), own process group (`Setpgid`), no
  controlling terminal. Interactive: a PTY (`github.com/creack/pty`, already
  in go.sum, becomes a direct dependency; pure Go on linux/darwin) with
  `Setsid`+`Setctty`, so the child leads its own group.
- **Redaction** (`redact.go`): every output byte passes a `Redactor` before
  the scrollback write. Each secret occurrence becomes `[REDACTED]`
  (`core.Redacted`), leftmost-longest. The redactor holds back only the
  longest buffered suffix that is a proper prefix of some secret, so a
  secret split across any number of writes is still caught and nothing
  else is delayed; the held tail is flushed (redacted) at end of output.
  Empty secrets are ignored. Stream clients read only the scrollback file,
  so they never see unredacted bytes.
- **End.** When the child exits, its process group gets SIGKILL (nothing it
  left behind keeps writing into the session), output is drained to EOF
  (bounded at 2 s), the redactor flushed, and the state file rewritten with
  `exit_code` or `signal` (`unix.SignalName`, e.g. `SIGKILL`) and
  `ended_at`. The socket is then closed and removed; open streams get up to
  5 s to send their last bytes, then the process exits 0.
- **Stop.** SIGTERM to the process group, SIGKILL to the group after the
  grace period if the child is still alive, then the End path. Idempotent.
- **Socket protocol** (`protocol.go`), one request per connection: the
  client writes one JSON line `{"op":…}`; the supervisor answers one JSON
  line `{"ok":true,…}` or `{"ok":false,"error":{core.Error}}`.
  - `status` → `state` (`core.SupervisorState`), `size` (scrollback bytes),
    `stopping`.
  - `stream`, `offset` → header `{"ok":true,"offset":N}` then the raw
    scrollback bytes from N, live, until the session has ended and every
    byte is sent; then the supervisor closes the connection. An offset past
    the end is refused. Bytes are counted in the scrollback file, so a
    client that reconnects with offset + bytes read gets each byte once.
  - `input`, `data` (bytes) → written to the PTY. Refused in headless mode
    (no stdin; contract v1) and after the end.
  - `resize`, `rows`, `cols` → PTY window size (child gets SIGWINCH).
    Refused in headless mode.
  - `stop` → stops the child and answers with `status` once it has ended.
  - A Go client: `Dial(dir)`, `Status`, `Stream(ctx, offset, w)`, `Input`,
    `Resize`, `Stop`.
- **Unsupported platforms** (windows): `gaia supervise` fails with
  `CodeUnavailable`; the binary still builds.
- **Fake agent** `testdata/fakeagent` (package main in this module, built by
  tests by import path): steps from argv, in order — `print:TEXT`,
  `write:TEXT` (no newline, one write), `stream:N:DUR`, `sleep:DUR`,
  `ignore-term`, `tty` (prints `tty=yes|no` for stdin/stdout), `size`,
  `wait-winch`, `echo` (reads one line, prints `got:LINE`), `pwd`,
  `env:NAME`, `spawn` (starts a SIGTERM-ignoring grandchild in its group and
  prints `grandchild=PID`), `kill:SIG`, `exit:N`. `\n` in TEXT is a newline.

## Declared file scope

- **owned_paths:** internal/supervise/, cmd/gaia/supervise.go, cmd/gaia/supervise_test.go, cmd/gaia/main.go, testdata/fakeagent/, go.mod, go.sum
- **read_only_paths:** OVERVIEW.md, .metis/adr/, .metis/plans/phase-1-plan.md, docs/contracts/launcher-v1.md, internal/core/, internal/sock/, deps_test.go, Makefile

## Definition of Done

- A supervisor whose starter process is SIGKILLed keeps running and keeps
  appending the child's output to the scrollback.
- Exit code N (0, 3, 255) and death by signal (SIGKILL, SIGTERM) are
  recorded exactly in `state.json` and in `status`; `state.json` validates.
- A known secret printed whole, split across two writes with a pause, and
  adjacent to other text never appears in the scrollback file; the text
  around it does; prefix-only fragments that are not secrets are kept.
- A client reading a stream, disconnecting, and reconnecting with its byte
  count receives exactly the scrollback (no gap, no duplicate).
- Stop ends a child that ignores SIGTERM, and its SIGTERM-ignoring
  grandchild, within grace + margin; no process remains in the group.
- The socket is mode 0600, the session dir 0700, scrollback and state 0600;
  the socket is gone after exit.
- Headless child sees no tty on stdin/stdout; interactive child sees a tty
  whose size follows `resize` (reports the new size after SIGWINCH);
  interactive input reaches the child.
- The child runs argv with no shell (a `$HOME`/`;` argument arrives
  literally), in `work_dir`, with exactly `env`; a missing argv[0] answers
  `ok:false` and exit 97 with no state file left.
- No secret or env value appears in `state.json`.
- `make verify` green; `go build` with CGO off for windows still succeeds.

## Test plan

- `internal/supervise/redact_test.go`: table of chunkings (whole, split at
  every byte position, overlapping secrets, prefix-but-not-secret, empty
  secret, flush of held tail); a fuzz-style test splitting random text with
  embedded secrets at random points against a one-shot reference.
- `internal/supervise/supervise_test.go`: re-execs the test binary as the
  supervisor (TestMain switch) and builds the fake agent once. Tests:
  exit codes, signal death, secret split, reconnect offsets, stop of a
  SIGTERM-ignoring child and grandchild with group check (`kill(-pgid, 0)`
  = ESRCH), file and socket modes and socket removal, headless no-tty,
  interactive tty + resize + input, argv literal/work_dir/env, start
  failure, starter killed mid-session, state file contents without secrets,
  protocol refusals (headless input/resize, bad offset, unknown op).
- `cmd/gaia/supervise_test.go`: `gaia supervise` dispatch: bad flags exit
  2 (usage, as `gaia host`/`gaia node`); not listed in the usage text. A
  missing or bad spec (ok:false, exit 97) is tested on `supervise.Main`.

## Out-of-scope touches

None.

## Report

- Added: `internal/supervise/` (`doc.go` replaced the placeholder,
  `spec.go`, `redact.go`, `state.go`, `protocol.go`, `supervisor.go`,
  `proc_unix.go`, `proc_other.go`, `redact_test.go`, `supervise_test.go`),
  `cmd/gaia/supervise.go` (+test), `testdata/fakeagent/main.go`. Edited:
  `cmd/gaia/main.go` (one `supervise` case), `go.mod` (`creack/pty`
  indirect → direct; go.sum unchanged).
- Exported for 1.3: `Main`, `Spec`, `WriteState`, `ReadState`,
  `StateFile`/`ScrollbackFile`/`SocketFile`, `Client` (`Status`, `Stop`,
  `Input`, `Resize`, `Stream`), `ErrStreamCut`, `NewRedactor`.
- Tests: `TestRedactorTable`, `TestRedactorEverySplit` (every pair of cut
  points), `TestRedactorRandom` (8000 chunkings vs `strings.ReplaceAll`),
  `TestExitCodeRecorded` (0/3/255), `TestSignalDeathRecorded`
  (SIGKILL/SIGTERM), `TestSecretNeverReachesScrollback` (split write with a
  200 ms pause; scrollback, stream, state.json, stderr checked),
  `TestStreamReconnectFromOffset` (≥3 cuts, reassembly = scrollback),
  `TestStopKillsTermIgnoringGroup` (agent and grandchild ignore SIGTERM;
  group empty after stop), `TestFileModesAndSocketRemoved`,
  `TestHeadlessHasNoTerminal`, `TestInteractivePTY` (tty, initial size,
  resize → SIGWINCH, input), `TestArgvLiteralWorkDirAndEnv`,
  `TestStartFailureLeavesNothing`, `TestSupervisorOutlivesStarter`
  (starter SIGKILLed, scrollback keeps growing, real exit recorded),
  `TestProtocolRefusals`, `TestSpecFormatHidesSecrets`,
  `TestStateFileRoundTrip`, `TestMainUsageAndBadSpec`,
  `TestSuperviseDispatch`.
- `make verify`: green on darwin/arm64. `go test -race -count=3
  ./internal/supervise/`: green. `GOOS=linux go vet` and `GOOS=windows go
  vet/build`: clean. Linux tests were not run locally (CI runs them).
- Decision: the socket has no peer-uid check of its own (`internal/sock`
  is a sibling leaf and cannot be imported); access is limited by the
  0700 session directory owned by the user.
