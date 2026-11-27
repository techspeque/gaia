---
type: plan
phase: 1
title: Sessions
overview_ref: OVERVIEW.md §4 Phase 1
status: draft
created: 2026-11-27
---

# Phase 1 — Sessions

> Derived from: OVERVIEW.md §4 Phase 1
> Produces: slices phase-1-ws-1.1 … 1.8 and phase-1-gate in .metis/slices.yaml
> ADRs: 0008, 0009, 0010 (all in .metis/adr/)
> Contract: docs/contracts/launcher-v1.md

## Context

Phase 1 makes Gaia useful. After it, `gaia run` starts an agent CLI on any
registered node, in that node's checkout of the caller's repository, and the
session outlives the caller's terminal and a restart of the node daemon.
`gaia run --wait` behaves like a local agent process, so `metis launch` and
`metis auto` drive agents on other machines by changing one line in
`project.yaml`. There are no credentials yet: a session inherits the node
user's environment, so agent CLIs must already be signed in on each node.

Decisions made in this plan:

- **One session per checkout, queued** (ADR-0010 rule 6). A second run on
  the same node and repository waits for the first. Worktrees are not built.
- **Headless sessions use pipes; interactive sessions use a PTY.** `--wait`
  and `--detach` start the agent with no terminal, as Metis does locally.
  `--interactive` allocates a PTY. Both write scrollback.
- **`--profile` is optional in this phase** and names nothing yet. Phase 2
  makes it carry credentials.
- **`--interactive --slice` is Phase 3**, with the Metis adapter. This phase
  ships `--interactive -- argv…` only.
- **Linux and macOS only.** ConPTY and named pipes stay unplanned (Phase 0
  plan).

## Dependencies

- Requires: Phase 0 complete
- External: for the gate, two machines on a tailnet with `git`, an agent CLI signed in on each, and push access to a test repository from both
- Metis: `metis launch` and `metis auto` need to push before and fetch after a remote session (`metis/plans/auto-hardening.md`, item 3). Until that ships, workstream 1.8 uses a wrapper script in `testdata/`.
- Local: gate machines are described in `.local/hosts.md`, never committed

---

## Workstream 1.1: Session model, store and API types

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions

Tasks:
- Add the migration for the session table of OVERVIEW §2.2, with the profile column nullable
- Implement create, get, list, update-state and set-exit on session, rejecting illegal state transitions in the store
- Define request and response types for `/api/v1/sessions` and the session start request (node, repo, branch, mode, argv, profile)
- Define the supervisor's state file: session id, pid, mode, argv, started_at, exit code, ended_at
- Record `gaia ls -o json` fixtures for the empty list and for one session in each state

Suggested packages:
- `internal/core`
- `internal/store`

Acceptance criteria:
- [ ] A session row cannot move from a terminal state to any other state
- [ ] argv round-trips through the store byte for byte, including empty strings, quotes and newlines
- [ ] The fixtures match the encoded types, and a shape change fails the build
- [ ] No session column can hold a credential, and the schema has none that could be mistaken for one

---

## Workstream 1.2: Session supervisor

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.1

Tasks:
- Implement the internal `gaia supervise` command: start the child in its own process group, own its pipes or PTY, write scrollback, serve a 0600 unix socket, and exit when the child exits or on stop (ADR-0008)
- Start the child from argv with no shell, in a given working directory and environment
- Write scrollback to disk through a redactor fed the session's known secret values, before any byte is written (ADR-0008 rule 3)
- Define the socket protocol: stream output from a byte offset, send input, resize, stop, read status
- Persist the state file atomically on start and on exit, with the exit code or the signal
- On stop, send SIGTERM to the process group, then SIGKILL after a grace period
- Add a fake agent under `testdata/` that prints, streams slowly, sleeps, ignores SIGTERM and exits with a given code, on cue from its arguments

Suggested packages:
- `internal/supervise`
- `cmd/gaia`
- `testdata/`

Acceptance criteria:
- [ ] The supervisor keeps running, and keeps recording output, after the process that started it is killed
- [ ] The child's exit code and death by signal are both recorded exactly
- [ ] A known secret printed by the child, including when split across two writes, never reaches the scrollback file
- [ ] A client that reconnects with its last byte offset receives every byte after it and none twice
- [ ] Stop ends a child that ignores SIGTERM within the grace period and leaves no process in its group
- [ ] The socket is mode 0600 and is removed on exit
- [ ] Headless mode gives the child no terminal; interactive mode gives it a PTY whose size follows resize

---

## Workstream 1.3: Node session manager

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.2

Tasks:
- Start a supervisor as a detached process, in a new session, that survives the node daemon's exit
- On node start, discover supervisors from the state directory, reconnect to the live ones and re-register their sessions with the host (ADR-0008 rule 4)
- Mark a session whose supervisor is gone and whose state file has no exit as `lost`
- Report state changes and exit codes to the host; replay them when the host was unreachable
- Stop a session on request from the host
- Bound scrollback per session and in total, dropping the oldest closed sessions first

Suggested packages:
- `internal/node`
- `internal/supervise`

Acceptance criteria:
- [ ] Killing the node daemon with SIGKILL mid-session leaves the session running; a new daemon lists it as `running` and then records its real exit code
- [ ] A supervisor killed with SIGKILL yields a `lost` session, never `exited`
- [ ] A session that ends while the host is down is reported with the right exit code when the host returns
- [ ] Nothing a session needs is held only in the daemon's memory, checked by restarting the daemon at each step of a session's life in a test
- [ ] Scrollback on disk never exceeds the configured bound

---

## Workstream 1.4: Sessions over the host: run, ls, stop, logs

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.3

Tasks:
- Implement the host's session API: start on a named node, list, get, stop, and read scrollback from an offset
- Route each request to the node over the tailnet, or in-process in local-only mode
- Implement `gaia run --node N --detach -- argv…`, printing the session id
- Implement `gaia ls`, `gaia stop SESSION` and `gaia logs SESSION [--follow]`, with `-o json` on `ls`
- Fail a start on an unknown or offline node with the documented exit code, without creating a session row that can never run
- Refuse a run whose argv is empty

Suggested packages:
- `internal/host`
- `internal/node`
- `cmd/gaia`

Acceptance criteria:
- [ ] `gaia run --detach` on machine A starts the fake agent on node B, and `gaia ls` on either machine shows it
- [ ] `gaia logs --follow` prints output as it is produced and ends with the session
- [ ] `gaia stop` ends the session and `gaia ls` shows `stopped`, not `failed`
- [ ] A start on an offline node exits 91 and leaves no session behind
- [ ] The same commands work in local-only mode with no tailnet code running

---

## Workstream 1.5: Attach and interactive sessions

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.4

Tasks:
- Implement terminal I/O over WebSocket at `/api/v1/sessions/{id}/tty`, from host to node to supervisor
- Implement `gaia attach SESSION`: raw terminal mode, resize forwarding, a detach key sequence, and restore of the caller's terminal on every exit path
- Implement `gaia run --interactive -- argv…`: start with a PTY and attach
- Replay recent scrollback on attach so the screen is not empty
- Accept input from every attached client (ADR-0008 rule 5)
- Check the WebSocket origin and refuse anything but the CLI and same-origin requests

Suggested packages:
- `internal/host`
- `internal/node`
- `cmd/gaia`

Acceptance criteria:
- [ ] Detaching leaves the session running; attaching from a second machine shows the same screen
- [ ] Two attached clients both see output, and input from either reaches the agent
- [ ] The caller's terminal is restored after detach, after the session ends and after the connection drops
- [ ] Attaching to a headless session shows its output and accepts no input
- [ ] A WebSocket upgrade with a foreign origin is refused

---

## Workstream 1.6: Node repository checkouts

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.1

Tasks:
- Keep one checkout per remote URL under the node's workspace directory, cloned on first use, with a directory name derived from the URL that cannot escape the workspace
- Before a session: fetch, then fast-forward the requested branch; create the local branch when it exists only on the remote
- Fail with a typed error on a dirty checkout or a non-fast-forward, changing nothing (ADR-0010 rule 2)
- After a session: push the branch if it is ahead, fast-forward only; on rejection leave the checkout as it is and report it (ADR-0010 rule 3)
- Run git with structured argv and the node user's own credentials; never pass a credential, and never run `reset`, `rebase`, `stash`, `clean` or a forced push
- Queue a second session for the same checkout behind the first
- Test against local bare remotes: caller ahead, dirty checkout, diverged branch, rejected push, new branch, concurrent runs

Suggested packages:
- `internal/repo`

Acceptance criteria:
- [ ] A session starts at exactly the remote branch's tip
- [ ] Commits made by the session are on the remote before the run is reported finished
- [ ] A dirty checkout, a diverged branch and a rejected push each produce their typed error and leave the checkout byte for byte as it was
- [ ] A test fails if the package ever invokes a forbidden git subcommand or flag
- [ ] A URL crafted to escape the workspace directory is rejected
- [ ] Two runs on one checkout never overlap

---

## Workstream 1.7: The launcher contract: `gaia run --wait`

- **Risk:** high
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.4, 1.6

Tasks:
- Implement `gaia run … --wait -- argv…` exactly as `docs/contracts/launcher-v1.md` states
- Default `--repo` and `--branch` to the caller's `origin` URL and current branch; exit 93 when the caller's branch is ahead of the remote or has no upstream
- Stream the session's output to the caller's stdout as it is produced; write Gaia's own messages to stderr with the `gaia: ` prefix
- Exit with the agent's exit code, `128 + N` for a signal, or the reserved code for each Gaia failure
- Exit 94 when the agent finished but the node could not push
- On SIGINT or SIGTERM stop the remote session and exit 130; with `--detach-on-interrupt` leave it running
- Keep the session running when the caller's connection is lost, and exit 95 on the caller's side only when the outcome cannot be learned
- Add a contract test that runs each row of the exit-code table against the fake agent

Suggested packages:
- `cmd/gaia`
- `internal/host`
- `internal/core`

Acceptance criteria:
- [ ] For agent exit codes 0, 1, 2, 3 and 130, `gaia run --wait` exits with the same code
- [ ] Every reserved code in the contract is produced by a test that creates its condition
- [ ] Output arrives while the agent is still running, not at the end
- [ ] Argv containing spaces, quotes, newlines and a leading `-` reaches the agent unchanged
- [ ] Interrupting `gaia run --wait` leaves no agent process on the node; with `--detach-on-interrupt` the session finishes and its commits are pushed
- [ ] Start-up overhead, from invoking `gaia run` to the agent's first byte, is under one second on a LAN with a warm checkout

---

## Workstream 1.8: Metis drives remote agents

- **Risk:** medium
- **Coder:** claude-code/opus-5-5
- **Reviewer:** codex/gpt-6-astra
- **Stage:** sessions
- **Blocked by:** 1.7

Tasks:
- Add an end-to-end test: a temporary Metis project whose agents' `launch` is `gaia run … --wait`, a node running the fake agent, and a local bare remote
- Run `metis launch` through it and assert output, exit code and the agent's ledger commit arriving in the caller's checkout after a fetch
- Run `metis auto` through it for one slice, coder then reviewer, each on a different node
- Until Metis pushes and fetches around a session itself, provide `testdata/metis-remote.sh`, a wrapper that pushes, runs `gaia run`, then fetches and fast-forwards
- Skip the test, visibly, when `metis` is not on the path; Gaia does not depend on it (ADR-0001 rule 4)
- Document the `project.yaml` lines for a remote agent in the README

Suggested packages:
- `internal/metis`
- `testdata/`

Acceptance criteria:
- [ ] `metis launch` against a remote node exits with the fake agent's code and the ledger flip is in the caller's checkout
- [ ] `metis auto` completes one slice with coder and reviewer on different nodes
- [ ] Interrupting `metis auto` stops the remote session
- [ ] With `metis` absent, the test is reported as skipped and `make verify` passes
- [ ] Gaia wrote nothing under `.metis/` in any checkout

---

## Phase Gate

> This section defines how Phase 1 is validated as a composed system.
> It becomes a `gate` slice automatically (phase-1-gate, risk high,
> blocked by every workstream in the phase).

Composition scenarios to validate:
- [ ] On two real machines, `metis auto` on a real repository runs one slice with the coder on one node and the reviewer on the other, both through `gaia run --wait`, with real agent CLIs
- [ ] Closing the laptop that started a `--detach` session leaves it running; it is visible and followable from a second device
- [ ] Restarting the node daemon mid-session, and upgrading its binary, loses no output and reports the real exit code
- [ ] Killing the host mid-session leaves the session running, and its result is recorded when the host returns
- [ ] `gaia attach` from two machines at once shows one screen
- [ ] A dirty node checkout and a caller ahead of the remote both fail with 93 and change nothing
- [ ] Killing every Gaia process at an arbitrary moment leaves each checkout in a state a person can continue by hand
- [ ] Scrollback, logs and API responses contain none of the test secrets planted in a session's output
- [ ] Every `-o json` output matches its recorded fixture, and the launcher contract document is marked v1

Performance / resource checks:
- [ ] `gaia run --wait` adds under one second to a session's start on a LAN
- [ ] A session producing 10 MB of output streams without loss and without unbounded memory in supervisor, node or host
- [ ] A session runs for eight hours on a gate machine with the caller disconnected throughout
