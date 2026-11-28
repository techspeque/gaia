# phase-1-ws-1.1 — Session model, store and API types

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.1

## Goal

Give sessions a durable host record (migration 0003, store CRUD that refuses
illegal state transitions), the `/api/v1/sessions` wire types and start
request, the supervisor's state-file type, and recorded `gaia ls -o json`
fixtures that the build checks against the types.

## Architectural context

- OVERVIEW §2.2 (Session: id, node, profile, repo remote, branch, slice
  (optional), mode, argv, state, started_at, ended_at, exit code), §2.3
  (`core` no I/O; `store` imports `core` only), §3.3 (no secret on disk or
  in an API response), §6 (`/api/v1`, `gaia ls`, every list supports
  `-o json`).
- ADR-0006 (SQLite host state), ADR-0008 (supervisor and its state).
- Consumes from Phase 0: `core.SessionID`, `core.SessionState` and its
  transition table (`Terminal`, `Transition`), `core.Error` and codes,
  `core.APIPrefix`, `core.ValidNodeName`, `store.Store` (`w`/`r` split,
  `_txlock=immediate` writer), the migration runner.
- Consumers: 1.2 (state file), 1.3 (node manager), 1.4 (host routes and
  `gaia ls`, which encodes `core.ListSessionsResponse` like `gaia nodes`).

Design decisions:

- **Migration `0003_sessions.sql`**, `STRICT` table `sessions`:
  `id TEXT PK`, `node_id TEXT NOT NULL`, `profile TEXT NULL` (nullable; no
  profiles table yet, Phase 2), `repo_remote`, `branch`, `slice`
  (`TEXT NOT NULL DEFAULT ''`, empty means none), `mode` (CHECK headless |
  interactive), `argv TEXT NOT NULL` (a JSON array of strings, CHECK
  `json_type(argv) = 'array'`), `state` (CHECK one of the six states),
  `started_at INTEGER NOT NULL`, `ended_at INTEGER NULL`,
  `exit_code INTEGER NULL`; Unix nanoseconds UTC like `nodes`. CHECKs: a
  terminal state has `ended_at`, a non-terminal one has neither `ended_at`
  nor `exit_code`; `exited` has an `exit_code`. Index on `node_id`.
  - No foreign key to `nodes`: `gaia nodes remove` must keep working and a
    session's history outlives its node's registration.
  - A trigger aborts any UPDATE of a row whose state is terminal and any
    DELETE of a row: the terminal-state rule holds even against raw SQL.
  - No column for env, tokens or keys. The column set is fixed by a test.
- **argv:** non-empty, `argv[0]` non-empty, each element valid UTF-8 with
  no NUL (exec cannot carry NUL; JSON would alter invalid UTF-8). Every
  accepted argv round-trips byte for byte. Checked by
  `core.ValidArgv`, used by the store and by `StartSessionRequest.Validate`.
- **Store (`internal/store/session.go`):** `Session{ID, NodeID, Profile,
  RepoRemote, Branch, Slice, Mode, Argv, State, StartedAt, EndedAt
  (zero = none), ExitCode *int}`.
  - `CreateSession` (state must be `starting`, no end or exit code;
    duplicate id → conflict).
  - `GetSession` (not found → `CodeNotFound`).
  - `ListSessions` (ordered by `started_at`, then id; empty non-nil).
  - `UpdateSessionState(ctx, id, to)`: non-terminal targets only.
  - `SetSessionExit(ctx, id, SessionExit{State, ExitCode *int, EndedAt})`:
    terminal targets only, sets ended_at and exit code in the same write.
  - Both read the current state and update with `WHERE state = <read>` in
    one immediate transaction; an illegal move is the `core.Transition`
    conflict error. Profile `""` is stored as NULL.
- **Core (`internal/core/session.go`):** `SessionMode` (`headless`,
  `interactive`, `Valid`); `SessionsPath`, `SessionPath(id)`; wire type
  `Session{id, node_id, node_name, profile, repo, branch, slice, mode,
  argv, state, started_at, ended_at (null until ended), exit_code (null
  when none)}`; `ListSessionsResponse{sessions}`; `StartSessionRequest{node,
  repo, branch, mode, argv, profile}` with `Validate`;
  `StartSessionResponse{session}`; `StopSessionResponse{session}`. `profile`
  is `""` on the wire when none. `node_name` is the node's name at read time,
  filled by the host (1.4).
- **State file (`internal/core/supervisor.go`):** `SupervisorState{version
  (=1), session_id, pid, mode, argv, started_at, exit_code (null), signal
  ("" unless killed by a signal; needed by 1.2), ended_at (null)}` with
  `Validate`. Type and encoding only; 1.2 writes it.
- **Fixtures:** `testdata/contract/gaia/api-v1/sessions-empty.json` and
  `sessions.json` (one session per state, neutral ids, names and times), in
  the `gaia nodes -o json` format (two-space indent, trailing newline).
  A core test decodes each with unknown fields refused, re-encodes and
  compares byte for byte, and checks each state appears exactly once. 1.4
  points its `gaia ls` writer test at the same files.

## Declared file scope

- **owned_paths:** internal/core/session.go, internal/core/session_test.go, internal/core/supervisor.go, internal/core/supervisor_test.go, internal/store/migrations/0003_sessions.sql, internal/store/session.go, internal/store/session_test.go, testdata/contract/gaia/api-v1/sessions-empty.json, testdata/contract/gaia/api-v1/sessions.json, testdata/contract/README.md
- **read_only_paths:** OVERVIEW.md, .metis/adr/, internal/core/, internal/store/, cmd/gaia/cli/nodes.go, testdata/contract/gaia/api-v1/nodes.json

## Definition of Done

- A session in `exited`, `stopped` or `failed` cannot move to any state,
  through the store (conflict error) or raw SQL (trigger abort).
- Every illegal transition of the core table is refused by the store; every
  legal one succeeds.
- argv round-trips byte for byte, including empty strings, quotes,
  backslashes, newlines, tabs and non-ASCII; invalid argv is refused.
- The fixtures decode into the types with unknown fields refused and
  re-encode to identical bytes; a field rename, addition or removal fails
  `go test`.
- The sessions table's columns equal a fixed list, none named like a
  credential (token, secret, key, password, credential, auth, env).
- Migrating a v2 database with nodes adds the table and keeps the nodes.
- `make verify` green.

## Test plan

- `internal/store/session_test.go`: CRUD and reopen; list order and empty
  list; not found; duplicate id; invalid create inputs; full transition
  matrix (6×6) through `UpdateSessionState`/`SetSessionExit`; terminal row
  frozen against raw SQL UPDATE and DELETE; argv round-trip table; schema
  column set and credential-name check; v2 → v3 migration keeps nodes.
- `internal/core/session_test.go`: `SessionMode.Valid`, `ValidArgv`,
  `StartSessionRequest.Validate`, paths, fixture round-trip and per-state
  coverage, null `ended_at`/`exit_code` encoding.
- `internal/core/supervisor_test.go`: state file encodes to a fixed JSON
  shape and decodes back; `Validate` cases.

## Out-of-scope touches

None.

## Report

- Added: `internal/core/session.go` (+test), `internal/core/supervisor.go`
  (+test), `internal/store/migrations/0003_sessions.sql`,
  `internal/store/session.go` (+test), `testdata/contract/gaia/api-v1/sessions-empty.json`,
  `sessions.json`. Edited: `testdata/contract/README.md` (two fixture rows).
- Schema version 2 → 3. Terminal rows frozen by trigger
  `sessions_terminal_frozen`; deletes refused by `sessions_no_delete`;
  state/ended_at/exit_code consistency by CHECKs.
- Tests: `TestSessionTransitionMatrix` (36 moves vs `core.CanTransition`),
  `TestTerminalSessionFrozenInSQL`, `TestSessionArgvRoundTrip`,
  `TestSessionColumnsHoldNoCredential`, `TestMigrateV2KeepsNodesAddsSessions`,
  `TestSessionSchemaChecks`, `TestSessionsFixturesRoundTrip`,
  `TestSessionsFixtureCoversEveryState`, `TestSupervisorStateShape`, and the
  validation tests named in the test plan.
- `make verify`: green (fmt, vet, staticcheck, all tests).
- Decision: `UpdateSessionState` takes live targets only and
  `SetSessionExit` terminal ones, so every terminal row gets `ended_at` in
  the same write. `exited` needs an exit code; `stopped` and `failed` may
  have none.
