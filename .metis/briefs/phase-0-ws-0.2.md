# phase-0-ws-0.2 — Core types and contract constants

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.2

## Goal

Give every other package one shared, I/O-free `internal/core` holding typed
ids, node and session states with their legal transitions, the reserved exit
codes of `docs/contracts/launcher-v1.md`, the typed `{code, message, details}`
error, the API version and `/api/v1/nodes` wire types, and a redaction type
that never reveals a secret.

## Architectural context

- OVERVIEW §2.2 (Node, Session entities), §2.3 (`internal/core`: ids,
  states, reserved exit codes, error shape, redaction; no I/O), §6 (host API
  under `/api/v1`; launcher contract exit codes 90–99).
- `docs/contracts/launcher-v1.md` "Exit codes" table is the source of truth
  for the exit-code constants; a test in `internal/core` parses it.
- `deps_test.go` (slice 0.1) already enforces that `core` imports no package
  of this module.
- Consumers: 0.3 (store: NodeID, Node fields), 0.4/0.5/0.7 (API types, typed
  errors, exit codes, API version), Phase 1 (SessionID, session states).

Design decisions:

- Ids: `NodeID` and `SessionID`, string form `<prefix>_<26 chars>` with
  prefixes `node` and `sess`; the body is 16 bytes in lowercase Crockford
  base32. Construction takes caller-supplied 16 bytes (`NewNodeID(b [16]byte)`),
  so `core` reads no entropy source itself (no I/O). `Parse*` rejects a wrong
  or missing prefix, wrong length, and characters outside the alphabet.
  Both implement `encoding.TextMarshaler`/`TextUnmarshaler`, so JSON
  round-trips through the string form.
- Node states: `online`, `offline`; legal: online→offline, offline→online.
- Session states: `starting`, `running`, `exited`, `stopped`, `failed`,
  `lost`. Legal transitions:
  starting→running|stopped|failed|lost;
  running→exited|stopped|failed|lost;
  lost→running (supervisor rediscovered, ADR-0008 rule 4)|exited|stopped|failed.
  `exited`, `stopped`, `failed` are terminal. A state to itself is not a
  transition and is rejected. Anything not listed is rejected.
- Exit codes: typed `ExitCode` constants 90–99 named as in the contract
  table, with `Name()` returning the contract name (e.g. `no_daemon`).
- Error: `*core.Error{Code ErrorCode, Message string, Details map[string]string}`
  implementing `error`, JSON `{"code","message","details"}` with `details`
  always present (`{}` when empty). Stable codes: `invalid_argument`,
  `not_found`, `conflict`, `unauthorized`, `forbidden`, `unavailable`,
  `version_mismatch`, `internal`. `errors.Is` matches on code.
- API: `APIVersion = 1`, `APIPrefix = "/api/v1"`. Types: `Node`,
  `ListNodesResponse{nodes}`, `RegisterNodeRequest`, `RegisterNodeResponse`,
  `HeartbeatRequest`, all with snake_case JSON tags.
- Redaction: `core.Secret` wraps a string; `String`, `GoString`, `Format`
  (every verb and flag), `MarshalJSON`, `MarshalText` all yield `[REDACTED]`;
  the value is reachable only through an explicit `Reveal()` method.
  No unmarshal methods: decoding into a Secret is out of scope for Phase 0.

## Declared file scope

- **owned_paths:** internal/core/
- **read_only_paths:** docs/contracts/launcher-v1.md, OVERVIEW.md, deps_test.go, .metis/adr/

## Definition of Done

- Ids round-trip through string and JSON form; parsing rejects wrong prefix,
  other id's prefix, bad length and bad characters.
- Every (from, to) pair of node and session states not in the legal table is
  rejected by `CanTransition`, checked exhaustively by a test; every legal
  pair is accepted; unknown state values are rejected.
- A test parses the "Exit codes" table of `docs/contracts/launcher-v1.md`
  and fails if any row's code or name differs from the constants, or if a
  constant is missing from the table or vice versa.
- `core.Error` marshals to exactly `{code, message, details}` and
  unmarshals back; `errors.Is` matches by code.
- Formatting a `Secret` with `%v %+v %#v %s %q %x %X %d` and widths/flags,
  `fmt.Sprint`, JSON (bare and nested in a struct, map, slice) and text
  marshalling never contain the value.
- `internal/core` non-test files import only the standard library and no
  I/O packages (`os`, `io`, `net`, `syscall`, `crypto/rand` …), checked by a
  test; `make verify` green.

## Test plan

- `id_test.go`: round-trip, JSON round-trip, rejection table.
- `state_test.go`: exhaustive transition matrix for both state kinds;
  terminal states; JSON of states as strings.
- `exitcode_test.go`: parses the contract table (test-only file read) and
  compares both directions.
- `error_test.go`: JSON shape, round-trip, `errors.Is`/`errors.As`.
- `secret_test.go`: every verb/flag, JSON, text, nested containers.
- `noio_test.go`: parses non-test files in `internal/core` and fails on any
  module import or a denied stdlib I/O package.

## Out-of-scope touches

None.

## Report

Filled at implementation. Facts and evidence only: what changed, commands run and their results,
decisions taken with the reason, findings resolved. No narrative; short.
