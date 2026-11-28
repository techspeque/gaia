# phase-1-ws-1.3 — Node session manager

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.3

## Goal

Give the node a session manager that starts each session's supervisor as a
detached process, keeps everything a session needs on disk, rediscovers
supervisors after a daemon restart (ADR-0008 rule 4), classifies outcomes
(`running`, `exited`, `stopped`, `failed`, `lost`), reports every state
change to the host with replay after an outage, stops sessions on request,
and bounds scrollback per session and in total.

## Architectural context

- OVERVIEW §2.3 (`node` may import leaves; `supervise` is a leaf importing
  `core` only), §3.3 (a session survives the loss of the node daemon; no
  secret on disk or in a log; listeners only on user-only sockets), §7
  (supervisor tests with real processes, kill the daemon mid-session and
  reattach), §8 (node down → sessions continue and reattach).
- ADR-0008 rules 1–4. Launcher contract v1: agent killed by signal N is
  reported as exit code `128 + N`; stop = SIGTERM, SIGKILL after 10 s.
- Consumes from 1.1: `core.SessionID`, `core.SessionState` and its
  transition table, `core.SupervisorState`, `core.Error`. From 1.2:
  `supervise.Spec`, `ReadState`, `Client` (`Status`, `Stop`, `Stream`), the
  ready line, `StateFile`/`ScrollbackFile`/`SocketFile`.
- Consumers: 1.4 (wires the manager into `gaia node` and `gaia host --node`,
  implements the host side of `Reporter`, routes start/stop/list), 1.5
  (attach through `supervise.Client` on `Manager.SessionDir`), 1.7.

Design decisions:

- **Package** `internal/node`, file `session.go` (+ `session_unix.go` /
  `session_other.go` for process attributes and signal numbers). Wiring into
  the daemons is 1.4's: there is no host session endpoint to report to yet.
  `OpenManager` is what "on node start" calls.
- **Layout** under the manager root `DIR` (0700): one directory per session
  `DIR/<session id>/` (0700) holding `session.json` (the manager's record,
  0600, written temp + fsync + rename + dir fsync), `supervisor.log` (the
  supervisor's stderr, 0600; it never logs secrets) and `sv/` (the
  supervisor's session directory of 1.2). Nothing a session needs lives
  only in memory: the record holds state, exit code, signal, ended_at,
  whether a stop was requested, the supervisor pid, and the queue of
  reports the host has not acknowledged.
- **Start** (`Manager.Start(ctx, StartSpec)`; the id comes from the caller,
  the host): check the id is new, make room in the scrollback budget, write
  the record (`starting`), spawn `Supervisor argv + --session-dir DIR/<id>/sv`
  with `Setsid` (new session and process group, so it outlives the daemon
  and a signal to the daemon's group), exactly `SupervisorEnv` as its
  environment, the spec as JSON on stdin, stdout a pipe for the ready line
  (30 s bound), stderr the log file. The daemon reaps it in a goroutine.
  Ready ok → `running`; refusal → `failed` with the supervisor's
  `core.Error` returned. The spec (env, secrets) is never written to disk.
- **Reconcile** (one function, used on open, by the watcher, after stop):
  supervisor `state.json` ended → terminal; else `Status` over the socket
  → `running`; socket unreachable → re-read `state.json` (it may have just
  ended) → ended ? terminal : `lost`. No `state.json` yet: wait up to 10 s
  from the record's creation (a supervisor may still be starting) → then
  `failed`. A `lost` session goes back to `running` if its supervisor
  answers again (on the next open).
- **Classification of an end:** stop requested → `stopped`, else
  `exited`. Exit code: the agent's code, or `128 + N` for death by signal N
  (contract v1). `lost` and `failed` carry no exit code. Moves follow
  `core.SessionState.CanTransition`; a move it forbids is ignored.
- **Watcher:** one goroutine per live session streams from the current end
  to `io.Discard` and reconciles when the stream returns, so an end or a
  supervisor's death is seen at once.
- **Stop** (`Manager.Stop(ctx, id)`): reconcile; if already terminal,
  return it; else persist `stop_requested`, `Client.Stop`, reconcile. A
  daemon that dies after persisting the request still classifies the end as
  `stopped`.
- **Reporting:** `Reporter` interface (`ReportSession(ctx,
  core.SessionReport) error`); new wire type `core.SessionReport{session_id,
  seq, state, exit_code, ended_at}` (`internal/core/report.go`, with
  `Validate`). Every state change appends a report (seq increasing per
  session) to the record's queue before anything else; `Manager.Run` sends
  queued reports in order per session, removes each once acknowledged, and
  retries with backoff (0.5 s → 30 s) while the host is unreachable. A
  `core.Error` refusal of `invalid_argument`, `not_found` or `conflict` is
  final: logged and dropped. A crash after acknowledgement and before the
  record is rewritten resends a report: the host must treat a repeated seq
  as done. On open, every non-terminal session queues a report of its
  current state (re-registration). A nil Reporter keeps reports queued.
- **Scrollback bounds** (`MaxScrollback` per session, default 64 MiB;
  `MaxScrollbackTotal`, default 1 GiB; total ≥ per session):
  - Per session, in the supervisor: new `Spec.MaxScrollback`. The
    scrollback rotates in two segments of `max/2` bytes, `scrollback`
    (newest) and `scrollback.prev`, so disk use never exceeds the bound and
    the newest output is kept. Offsets stay logical (bytes since the
    session started). `Status` gains `start` (first kept offset); a
    `stream` from an offset before it is refused (`invalid_argument`,
    detail `start`); a stream overtaken by rotation is closed, so the
    client sees a cut and resumes. `core.SupervisorState` gains
    `scrollback_end` (logical length, written on exit) so readers of an
    ended session map offsets and `Client.Stream` checks completeness.
    `max_scrollback` 0 keeps 1.2's unbounded behaviour.
  - In total, in the manager: before a start, closed sessions' scrollback
    plus `MaxScrollback` for every live session and the new one must fit
    `MaxScrollbackTotal`; the oldest closed sessions (by `ended_at`; `lost`
    counts as closed) have their scrollback deleted first. If it still does
    not fit, the start is refused (`unavailable`). A dropped session whose
    reports are all acknowledged is removed entirely. The same pruning runs
    on open and when a session ends.
- **Reads:** `List()` and `Get(id)` return `SessionInfo{ID, Mode, Argv
  (as redacted in state.json), State, PID, SupervisorPID, StartedAt,
  EndedAt, ExitCode, Signal}` from the records; `SessionDir(id)` is the
  supervisor directory for clients.

## Declared file scope

- **owned_paths:** internal/node/session.go, internal/node/session_unix.go, internal/node/session_other.go, internal/node/session_test.go, internal/core/report.go, internal/core/report_test.go, internal/core/supervisor.go, internal/core/supervisor_test.go, internal/supervise/
- **read_only_paths:** OVERVIEW.md, .metis/adr/, .metis/plans/phase-1-plan.md, docs/contracts/launcher-v1.md, internal/core/, internal/store/, internal/node/node.go, internal/node/remote.go, cmd/gaia/, testdata/fakeagent/, deps_test.go, Makefile

## Definition of Done

- A daemon process running the manager is SIGKILLed mid-session; the
  session keeps running; a new daemon lists it `running` and then records
  the agent's real exit code (`exited`, code N).
- A supervisor SIGKILLed mid-session yields `lost`, never `exited`.
- A session that ends while the reporter fails is reported with the right
  state and exit code, in order, once the reporter succeeds — also across
  a daemon restart.
- The daemon is SIGKILLed at each step of a session's life (record written,
  supervisor spawned, ready read, stop recorded, end recorded, report
  acknowledged) and a new daemon reaches the right final state and reports.
- Scrollback on disk of one session never exceeds `MaxScrollback` while the
  agent streams more than that; the newest output is kept; streams from a
  dropped offset are refused and a stream from `start` works. The sum over
  all sessions never exceeds `MaxScrollbackTotal`; the oldest closed
  session is dropped first; a start that cannot fit is refused.
- Stop of a running session yields `stopped` with the exit code
  (`143` for SIGTERM); stop of an ended session returns it unchanged.
- Death of the agent by signal N is reported as exit code `128 + N`.
- Record, log and directory modes are 0600/0700; no env value or secret
  from the spec appears under the manager root.
- 1.2's supervise tests still pass; `make verify` green; windows build
  still compiles.

## Test plan

- `internal/node/session_test.go` (linux/darwin): the test binary is also
  the supervisor and a "daemon" process (TestMain role switch); a daemon
  runs a Manager driven by JSON lines on stdin, reports to a file-backed
  fake host that fails while a "down" file exists, and can SIGKILL itself
  at a named step (test hook). Tests: `TestDaemonKilledSessionSurvives`,
  `TestSupervisorKilledIsLost`, `TestReportsReplayAfterHostOutage`,
  `TestRestartAtEveryStep`, `TestStopRecordsStopped`,
  `TestSignalExitCode`, `TestStartFailureIsFailed`,
  `TestTotalScrollbackBound`, `TestFileModesNoSecrets`,
  `TestReporterRefusalDropped`.
- `internal/supervise/supervise_test.go`: `TestScrollbackRotationBound`
  (sizes sampled while streaming ≤ bound; newest bytes kept; stream from
  `start` reassembles the kept tail; earlier offset refused;
  `scrollback_end` recorded), plus 1.2's tests unchanged.
- `internal/core/report_test.go`: `SessionReport.Validate` table;
  `internal/core/supervisor_test.go`: `scrollback_end` validation.

## Out-of-scope touches

None.

## Report

- Added: `internal/node/session.go` (`Manager`, `OpenManager`, `Start`,
  `Stop`, `List`, `Get`, `SessionDir`, `Run`, `Close`, `Reporter`,
  `StartSpec`, `SessionInfo`), `session_unix.go`, `session_other.go`,
  `session_test.go`; `internal/core/report.go` (+test). Edited:
  `internal/core/supervisor.go` (+test; `scrollback_end`),
  `internal/supervise/` (`spec.go` `MaxScrollback`; `supervisor.go`
  two-segment rotation, `readAt`, `Status.Start`, `scrollback_end` on exit;
  `protocol.go` `Status.Start`, `streamEnd` checks `scrollback_end`;
  `state.go` `ScrollbackPrevFile`; `doc.go`), new
  `internal/supervise/rotate_test.go` (the rotation test lives there, not
  in `supervise_test.go` as the test plan said; same package).
- Tests: all listed in the test plan exist and pass, plus
  `TestOpenManagerRefusesBadConfig`, `TestSessionReportShape`,
  `TestSpecMaxScrollbackValidated`. `go test -race -count=3
  ./internal/node/ ./internal/supervise/`: green. `make fmt-check vet
  staticcheck`: clean. `GOOS=windows go vet` and `GOOS=linux go vet` on the
  touched packages: clean. Linux tests not run locally (CI runs them).
- Decisions:
  - The spec is written to the supervisor's stdin in full (pipe, 30 s
    deadline) before the "spawned" step; a daemon that dies earlier leaves
    a supervisor that refuses and no agent, so the session ends `failed`.
    Found by `TestRestartAtEveryStep/spawned`.
  - A new daemon completes a stop the previous one recorded but may not
    have sent; otherwise a later natural exit would be classified
    `stopped` without a stop.
  - An end with neither exit code nor known signal is `failed` (an
    `exited` session needs an exit code).
  - The orphaned agent of a SIGKILLed supervisor is left running; the
    plan asks only for `lost`.
- Not wired into `gaia node` / `gaia host --node`: there is no host session
  endpoint yet (1.4).
- Follow-up for 1.4 or later (outside this scope): the systemd user unit
  has the default `KillMode=control-group`, so `systemctl --user
  stop/restart` would kill supervisors with the daemon; it needs
  `KillMode=process` (launchd likewise `AbandonProcessGroup`). Supervisor
  socket paths under the default macOS state dir are ~94 bytes; a very
  long home path can exceed the 103-byte limit, which the supervisor
  refuses with a clear error.
