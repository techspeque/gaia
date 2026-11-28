# phase-1-ws-1.7 — The launcher contract: `gaia run --wait`

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.7

## Goal

`gaia run … --wait -- argv…` behaves as `docs/contracts/launcher-v1.md`
states: the session runs in the node's checkout of the caller's repository
and branch, its output streams to the caller's stdout, the agent's commits
are pushed before `gaia run` returns, and the exit code is the agent's
(128+N for a signal) or a reserved 90–99 code for each Gaia failure; an
interrupt stops the remote session and exits 130.

## Architectural context

- OVERVIEW §3.3 invariants: `--wait` behaves like a local agent process;
  Gaia never resets, rebases, stashes or force-pushes; a session survives the
  loss of its client; nothing a session needs is only in daemon memory.
  §6 launcher contract. ADR-0009 rules 1–5, 7; ADR-0010 rules 1–6; ADR-0008.
- Consumes `internal/repo` (1.6): `Workspace.Acquire`, `Checkout.Prepare`,
  `Checkout.Finish`, `*repo.SyncError` (93 before / 94 after). This slice
  wires it into the node's session start and end, as the 1.6 brief deferred.
- Consumes the 1.4 session API (`StartSession`, `GetSession`, logs, stop),
  `core.ExitCode` table and the `DetailReason` mapping in `cli.classify`.

Design decisions:

- **Sync outcome is a session field.** `core.SyncState`: `""` (no repo),
  `pending` (the after-session push has not run), `done` (pushed, or nothing
  to push), `failed` (the push failed; commits are in the node's checkout →
  94). It is on `core.Session` (`sync`), `core.NodeSession`,
  `core.SessionReport` (omitempty), the node's session record and a new
  `sessions.sync` column (migration 0004). Legal moves: `""`→any,
  `pending`→`done|failed`; a settled value never changes (a replayed report
  is a no-op). `gaia ls -o json` gains `sync`; the fixtures are updated.
- **Node start with a repo** (`NodeStartRequest.Repo/Branch`,
  `StartSpec.Repo/Branch`, `ManagerConfig.Workspace`): the manager acquires
  the checkout (queued, ctx-bounded), runs `Prepare`, checks ctx is still
  live, then writes the record (`sync: pending`, repo, branch) and spawns
  the supervisor with the checkout as working directory. The checkout stays
  held while the session is starting or running. A Prepare failure records
  nothing and is a `*core.Error` with reason `sync_before_failed`
  (code conflict, detail `sync_reason`), or reason `usage` for an invalid
  URL or branch. A supervisor that refuses the session releases the
  checkout with `sync ""`.
- **Node end:** when a session with `sync: pending` reaches a terminal
  state the manager runs `Finish` in the background (bounded, 10 min),
  records `done` or `failed`, queues a report of the same state with the
  new sync, persists, then releases the checkout. A session that becomes
  `lost` releases the checkout unpushed (the next Prepare pushes leftover
  commits or reports a dirty tree). After a daemon restart the manager
  re-acquires the checkouts of live sessions before serving, and finishes
  terminal sessions still `pending`. Repo-less sessions are unchanged.
- **Host:** accepts `repo`/`branch` (both or neither, `StartSessionRequest.
  Validate`), stores them, forwards them, and applies `sync` from start
  answers and reports. A node refusal that carries a known reason keeps it
  (93, 98) instead of becoming 97. `--profile` names nothing in this phase,
  so any profile is refused with reason `profile_rejected` (92) and no
  session row. A start with a repo is bounded by 1 h node-side request
  timeout (`SessionClient`), not the 30 s default, as it includes clone and
  queue time.
- **CLI `gaia run`:** `--wait`, `--detach`, `--interactive` are exclusive;
  `--detach-on-interrupt` needs `--wait`; `--repo`/`--branch` are
  accepted with every mode. With `--wait`, a missing `--repo` is the cwd's
  `origin` URL and a missing `--branch` its current branch (not in a
  checkout, no origin, detached HEAD → 98). When the branch is taken from
  the cwd and the cwd's origin is the repo, the caller must not be ahead:
  no `refs/remotes/origin/B` → 93 "no upstream; push it", `origin/B..HEAD`
  non-empty → 93 "ahead; push first". Read-only git (`remote get-url`,
  `symbolic-ref`, `rev-parse`, `rev-list`); the caller's checkout is never
  changed. Without `--wait`, repo and branch come only from flags.
- **Wait loop:** after the start returns, stream logs with follow from the
  offset received, resuming after a cut, so each byte is written once;
  when the session is terminal drain the rest, then poll until `sync` is
  not `pending`. Exit: exited → agent code (94 if sync failed); stopped not
  by us → its code (94 if sync failed, 99 if none); failed → 99; lost for
  longer than `lostGrace` (10 s) → 95; no progress (daemon or node
  unreachable) for `unreachableAfter` (2 min) → 95. Both are `Env` fields
  for tests. Gaia's own messages go to stderr prefixed `gaia: `.
- **Interrupts** (SIGINT/SIGTERM; `Env.Interrupts`, nil installs
  `signal.Notify`): during the wait → `StopSession` (SIGTERM, SIGKILL after
  10 s) then exit 130; with `--detach-on-interrupt` exit 130 at once and
  print how to find the session. During the start request the start is
  not cancelled (the session id is not known yet): it is awaited and then
  stopped, so no agent is left behind; a second interrupt exits 130 at
  once. With `--detach-on-interrupt` an interrupt during start exits 130
  at once.
- **Daemon wiring:** `openSessions` opens `repo.Open(<state>/workspace)`.

## Declared file scope

- **owned_paths:** cmd/gaia/, internal/core/, internal/host/, internal/node/, internal/store/, testdata/contract/gaia/api-v1/
- **read_only_paths:** internal/repo/, internal/supervise/, testdata/fakeagent/, docs/contracts/launcher-v1.md, OVERVIEW.md, .metis/adr/, deps_test.go

Scope note: the plan suggests `cmd/gaia`, `internal/host`, `internal/core`.
`internal/node` is needed to run Prepare/Finish around the session (deferred
to this slice by 1.6), `internal/store` for the sync column, and the
`gaia ls -o json` fixtures change shape with `sync`.

## Definition of Done

- For agent exit codes 0, 1, 2, 3 and 130, `gaia run --wait` exits with the
  same code; an agent killed by SIGKILL gives 137.
- Each reserved code is produced by a test that creates its condition:
  90 no daemon, 91 unknown node, 92 any profile, 93 caller ahead / no
  upstream / dirty node checkout, 94 push rejected after the session,
  95 session lost, 96 incompatible daemon, 97 missing program, 98 bad
  flags, 99 Gaia internal failure.
- Output arrives on stdout while the agent is still running.
- Argv with spaces, quotes, newlines, an empty string and a leading `-`
  reaches the agent unchanged.
- The agent runs at the remote branch tip in the node's checkout; its
  commits are on the remote before `gaia run --wait` returns.
- SIGINT to `gaia run --wait` exits 130 and leaves no agent process; with
  `--detach-on-interrupt` it exits 130 at once, the session finishes and
  its commits are pushed.
- Killing the node daemon mid-session keeps the checkout held across the
  restart and still pushes the session's commits.
- Start-up overhead to the agent's first byte with a warm checkout is under
  one second (local daemon, local bare remote).
- `make verify` green.

## Test plan

- `cmd/gaia/wait_test.go` (e2e, `gaia host --node`, fake agent, local bare
  remote, isolated git config): `TestWaitContract` table — agent codes 0,
  1, 2, 3, 130, 137; 91, 92, 93 (caller ahead, no upstream, dirty node
  checkout), 94 (pre-receive hook), 95 (supervisor SIGKILLed), 97, 98, 90
  (no daemon); `TestWaitStreamsAndPushes` (output timing, cwd is the
  checkout at the remote tip, commit on the remote at return, warm start
  < 1 s); `TestWaitArgv`; `TestWaitInterrupt` (130, no agent process left,
  session stopped); `TestWaitDetachOnInterrupt` (130 at once, session
  finishes, commit pushed).
- `cmd/gaia/cli/wait_test.go` (fake daemon): flag rules, repo/branch
  defaults and caller checks against temp git repos, 96, 99, 95 by
  `lostGrace` and by unreachable daemon, sync `pending`→`failed` gives 94,
  interrupt during start awaits then stops, second interrupt exits at once.
- `internal/node`: start with a repo runs in the checkout at the remote
  tip; Prepare failure → reason 93, nothing recorded; finish pushes and
  reports `sync: done`; rejected push → `failed`; a second start queues
  until the first ends and is pushed; restart re-holds a live session's
  checkout and finishes a pending one.
- `internal/host`: repo/branch forwarded and stored; node reason kept;
  profile refused with 92 and no row; sync applied from reports, settled
  value not overwritten.
- `internal/store`: migration 0004, sync transitions, column list.
- `internal/core`: `SyncState` validity, request validation, report shape.

## Out-of-scope touches

None.

## Report

Filled at implementation.
