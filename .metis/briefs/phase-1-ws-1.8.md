# phase-1-ws-1.8 — Metis drives remote agents

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.8

## Goal

Prove end to end that `metis launch` and `metis auto` drive agents on Gaia
nodes through `gaia run … --wait`, with output, exit codes, interrupts and
the agents' ledger commits arriving back in the caller's checkout, and
document the `project.yaml` lines for a remote agent.

## Architectural context

- ADR-0009 (rules 1–5, 7), ADR-0010, ADR-0001 rules 4 and 5, OVERVIEW §6
  launcher contract, `docs/contracts/launcher-v1.md`.
- Consumes, unchanged: `gaia run --wait` (1.7), `gaia host --node` local-only
  mode, a host plus a second node on a test tailnet
  (`internal/net/nettest`, as `TestSessionsOverTailnet`), the e2e harness in
  `cmd/gaia` (`startRole`, `fakeAgent`, `isolateGit`, `cliSessions`, …).
- Metis side (external, consumed as a CLI): `metis launch` and `metis auto`
  run `agents.<slug>.launch` with no shell. Since Metis v0.0.25 an agent
  with `remote: true` is wrapped in a sync: the caller's branch is pushed
  before the session and fetched, fast-forward only, after it
  (`metis launch --help`; launcher-v1.md "What the caller must do").

Design decisions:

- **No wrapper script.** The plan's `testdata/metis-remote.sh` is
  conditional: "until Metis pushes and fetches around a session itself".
  Metis v0.0.25 does (`remote: true`), so the test uses `remote: true` and
  no wrapper is shipped. A `metis` older than v0.0.25 is reported as a
  visible skip, like an absent one.
- **Where the test lives:** `cmd/gaia/metis_test.go`, not `internal/metis`.
  The test needs the daemon harness and the re-exec of the test binary as
  `gaia` (`GAIA_TEST_RUN_MAIN`), both in `cmd/gaia`'s tests. It is test
  code only: no product code calls `metis`, so ADR-0001 rule 3 is
  unaffected, and `internal/metis` stays the Phase 3 placeholder.
- **The agent:** `testdata/metis-agent.sh ROLE SLICE`, run on the node by
  `gaia run` in the node's checkout. `coder` commits a file through
  `metis commit` and flips the slice coded; `reviewer` flips it reviewed.
  Extra steps (`sleep`, `exit N`) let the test make a session long or
  failing. It uses only `metis` and `git`, as a real agent would.
- **Temporary Metis project:** created by `metis init` in a temp clone of a
  local bare remote, with one slice, a committed brief, `commands.verify`
  set to `true`, and agents whose `launch` is
  `[<gaia>, run, --node, <node>, --wait, --, sh, <agent.sh>, <role>, "{slice}"]`
  and `remote: true`. `<gaia>` is the test binary with
  `GAIA_TEST_RUN_MAIN=1` in the environment metis passes on.
- **Skip rule:** `metis` not on `PATH`, or older than v0.0.25, gives
  `t.Skip` with the reason; `make verify` passes either way (ADR-0001
  rule 4).
- **".metis/ is Metis's":** asserted twice. At run time, every path under
  `.metis/` that differs between the node checkouts and the remote tip is
  metis's own run output (`.metis/runs/`, `.metis/.lock`, both gitignored by
  `metis init`), written by the agent's `metis` calls. Statically, no
  non-test Go source of Gaia names `.metis` as a path.

## Declared file scope

- **owned_paths:** cmd/gaia/metis_test.go, testdata/metis-agent.sh, README.md
- **read_only_paths:** cmd/gaia/ (other files), internal/, testdata/fakeagent/, docs/contracts/launcher-v1.md, OVERVIEW.md, .metis/adr/, .metis/plans/phase-1-plan.md

Scope note: the plan suggests `internal/metis` and `testdata/`; see
"Where the test lives" above.

## Definition of Done

- `metis launch` through `gaia run --wait` on a node exits with the agent's
  code (0, and a non-zero code from a failing agent), its output reaches
  `metis launch`'s run log, and after it returns the coder's `coded` flip is
  in the caller's checkout.
- `metis auto` completes one slice (coder, then reviewer, to reviewed) with
  the coder on one node and the reviewer on another, both through
  `gaia run --wait`; `gaia ls` shows each session on its node.
- Interrupting `metis auto` (SIGINT to its process group) stops the remote
  session: `gaia ls` shows it `stopped` and no agent process remains.
- With `metis` absent from `PATH` the tests report a skip and
  `make verify` passes.
- Gaia wrote nothing under `.metis/` in any checkout, as stated above.
- README documents the `project.yaml` lines for a remote agent.
- `make verify` green.

## Test plan

- `cmd/gaia/metis_test.go`:
  - `TestMetisLaunchRemote` (local-only `gaia host --node`, local bare
    remote): a failing agent → `metis launch` exits its code, no flip; then
    the coder → exit 0, the agent's output in metis's run log, the `coded`
    flip and the agent's commit in the caller's checkout after
    `metis launch` returns; `.metis/` check on the node checkout.
  - `TestMetisAutoRemote` (host with its own node plus `node-b` on a test
    tailnet): coder on the host's node, reviewer on `node-b`;
    `metis auto --slice` exits 0 with the slice reviewed; `gaia ls` shows
    the coder session on one node and the reviewer session on the other;
    `.metis/` check on both node checkouts.
  - `TestMetisAutoInterrupt` (local-only): coder sleeps; SIGINT to
    `metis auto`'s process group; the session ends `stopped` and no agent
    process remains.
  - `TestGaiaNeverNamesMetisDir`: static check over non-test Go sources.
  - Every metis test skips, with its reason, when `metis` is absent or too old.

## Out-of-scope touches

None.

## Report

Filled at implementation.
