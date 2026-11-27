# phase-0-ws-0.3 — SQLite store and migrations

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.3

## Goal

Give the host a durable registry: `internal/store` opens a user-only state
directory, holds a pure-Go SQLite database in WAL mode with one writer,
applies numbered, embedded, forward-only migrations at open (backing up an
existing database first, refusing a newer schema), and implements create,
get, list, update-last-seen and remove on the node table.

## Architectural context

- OVERVIEW §2.2 (Node: id, name, tailnet identity, os/arch, gaia version,
  last_seen), §2.3 (`internal/store` is a leaf; depends on `core` only),
  §3.1 (pure-Go SQLite, CGO off), §8 (state dir backed up as one directory).
- ADR-0006 rules 1–3: one state directory; numbered, embedded, forward-only
  migrations run at start; WAL mode, one writer. ADR-0002: CGO off.
- Consumes from `internal/core`: `NodeID`, `ParseNodeID`, `*core.Error` and
  its codes.
- Consumers: 0.4 (`gaia host` opens the store, `--node` registers itself),
  0.7 (registration, heartbeat, `gaia nodes remove`).

Design decisions:

- `store.Open(ctx, dir) (*Store, error)`. The state directory is created at
  mode 0700 if missing. An existing one that is not a directory, or whose
  mode grants any group or other permission (`mode & 0077 != 0`), is refused
  with `core.CodeInvalidArgument` and details `path`, `mode`. The check is
  skipped on Windows, where Unix mode bits do not apply (the daemon roles
  refuse Windows until after Phase 2).
- Database file `<dir>/gaia.db`. Driver `modernc.org/sqlite`. Two pools on
  the same file: a writer with `MaxOpenConns(1)` (the one writer) and a
  reader pool opened `query_only`. Pragmas: `journal_mode(WAL)`,
  `busy_timeout(5000)`, `foreign_keys(1)`; writer transactions are
  `BEGIN IMMEDIATE`.
- Schema version is `PRAGMA user_version`. Migrations are embedded
  `migrations/NNNN_name.sql` files; the runner requires them numbered 1..N
  without gaps, and applies each pending one in its own transaction that
  also sets `user_version`, so a failed migration leaves the previous version.
- Newer schema: before any connection is opened, the version is read from
  the SQLite file header (offset 60, so the open itself changes nothing),
  and again inside the migration transaction. Either above N fails with
  `core.CodeVersionMismatch`, details `db_version`, `binary_version`.
- Backup: when the database file existed before `Open` and at least one
  migration is pending, `VACUUM INTO <dir>/gaia.db.v<from>-<UTC timestamp>.bak`
  runs first (consistent with WAL; file mode 0600). A fresh database is not
  backed up.
- Node row: `id` (text PK, the `node_…` form), `name` (unique),
  `tailnet_identity` (unique), `os`, `arch`, `gaia_version`, `api_version`,
  `created_at`, `last_seen` (integer Unix nanoseconds, UTC). Names are unique
  because the CLI addresses nodes by name (`--node N`, `gaia nodes remove
  NAME`); tailnet identity is unique because re-registration is by identity
  (plan 0.7). `api_version` is stored for version negotiation (plan 0.7).
- `store.Node` holds the stored fields. Online/offline state is derived from
  `last_seen` by the host, so it is not stored and `core.Node` is not reused.
- Methods: `CreateNode`, `GetNode`, `ListNodes` (by name), `UpdateNodeLastSeen`,
  `RemoveNode`, `Close`. Missing row → `core.CodeNotFound`; duplicate id,
  name or tailnet identity → `core.CodeConflict`; zero id or empty name or
  identity → `core.CodeInvalidArgument`. Only these five operations: lookup
  by identity or name and upsert are left to the slices that need them.

## Declared file scope

- **owned_paths:** internal/store/, go.mod, go.sum
- **read_only_paths:** internal/core/, OVERVIEW.md, .metis/adr/, deps_test.go, Makefile

## Definition of Done

- A fresh state directory is created at 0700, migrates to the current
  schema, and reopens cleanly with no further migration and no backup.
- Opening a database whose `user_version` is above the binary's fails with
  the `version_mismatch` typed error, and every file in the state directory
  is byte-identical afterwards with no file added.
- Opening an existing database with a pending migration leaves a backup
  file that is a valid SQLite database at the old version.
- A state directory at mode 0755 (and 0750, 0701) is refused with the typed
  error; a file in place of the directory is refused.
- Node create/get/list/update-last-seen/remove work, with not_found,
  conflict and invalid_argument cases.
- The journal mode is WAL; the writer pool allows one connection.
- `internal/store` imports only `internal/core` from this module;
  `CGO_ENABLED=0 go test ./internal/store/` and `make verify` pass.

## Test plan

- `store_test.go`: fresh open (dir mode 0700, user_version = N, WAL mode),
  reopen without backup; dir mode refusals; file-not-dir refusal; newer
  schema refused with directory snapshot compared before/after; backup
  created on an existing older database (simulated by a database at
  user_version 0 holding a table, and by a runner test with two migrations
  where the second is pending).
- `migrate_test.go`: migration file name parsing and gap/duplicate
  detection on an in-memory `fstest.MapFS`; header version reader on a
  short/non-SQLite file.
- `node_test.go`: CRUD round-trip, ordering, last_seen update, not_found,
  conflicts on id/name/identity, invalid input; data survives reopen.

## Out-of-scope touches

None.

## Report

Filled at implementation.
