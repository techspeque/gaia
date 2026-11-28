# phase-1-ws-1.6 — Node repository checkouts

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-28
- **Plan:** .metis/plans/phase-1-plan.md §1.6

## Goal

Give the node one git checkout per remote URL in its workspace, synced only
through the remote: before a session, push any earlier unpushed work, fetch
and fast-forward the requested branch; after a session, push it if ahead,
fast-forward only. Failures are typed (exit 93 before, 94 after) and leave
the checkout as it was. Runs on one checkout are queued (ADR-0010).

## Architectural context

- OVERVIEW §2.3: `internal/repo` is a leaf; it imports `core` only.
  §3.3: Gaia never resets, rebases, stashes or force-pushes; killing Gaia
  leaves a repo a person can continue by hand; no secret on disk on a node.
  §7: repo sync tests against local bare remotes.
- ADR-0010 rules 2, 3, 5, 6. Launcher contract v1 exit codes 93
  (`sync_before_failed`) and 94 (`sync_after_failed`),
  `core.ExitSyncBeforeFailed` / `core.ExitSyncAfterFailed`.
- Consumer: 1.7 wires this into the node's session start and end. No
  wiring in this slice. Rule 1 (caller ahead of the remote) is checked on
  the caller's side in 1.7; the node can only guarantee it starts at the
  remote tip, which this slice tests.

Design decisions:

- **API.** `repo.Open(root) (*Workspace, error)` makes `root` (0700).
  `Workspace.Acquire(ctx, url) (*Checkout, error)` validates the URL and
  blocks until no other holder has that checkout (FIFO queue per checkout,
  ctx cancels the wait). `Checkout.Prepare(ctx, branch) (Prepared, error)`
  runs the before-session sync and returns the head commit.
  `Checkout.Finish(ctx, branch) (Finished, error)` runs the after-session
  push; it needs no state from `Prepare`, so a restarted daemon can call it.
  `Checkout.Dir()`, `Checkout.Release()`.
- **Directory name.** `DirName(url)`: the URL with any scheme and user
  stripped, every byte outside `[A-Za-z0-9._-]` mapped to `_`, truncated to
  48 bytes, prefixed with `r` if it does not start with an alphanumeric,
  then `-` and 16 hex digits of SHA-256 of the exact URL. It is a single
  path component and never `.` or `..`. The joined path is checked to be a
  direct child of the root, and an existing entry must be a real directory
  (no symlink).
- **URL validation** (`core.CodeInvalidArgument`): empty, a leading `-`,
  control characters, a `<transport>::` remote-helper form (`ext::` runs
  commands), or a password in the URL (it would be written to
  `.git/config` on the node) are rejected. Accepted: `scheme://…`,
  scp-like `user@host:path`, absolute local paths.
- **Branch validation** (`core.CodeInvalidArgument`): non-empty, no leading
  `-`, and `git check-ref-format --branch` accepts it.
- **Clone** on first use into a fresh `.clone-*` temp directory under the
  root, then rename into place, so a kill mid-clone never leaves a
  half-made checkout under the real name. Names never start with `.`.
- **Existing checkout** must have `.git` as a directory and
  `remote.origin.url` equal to the requested URL (else `wrong_remote`).
  Every git call passes `--git-dir` and `--work-tree` so git never walks
  up into a parent repository.
- **Prepare order:** dirty check (`status --porcelain -z
  --untracked-files=all`, before anything else, so a dirty checkout is not
  touched at all) → `fetch origin` → classify local `refs/heads/B` against
  `refs/remotes/origin/B`:
  neither → `branch_not_found`; remote only → `switch --track`;
  local only or local ahead → push it first (earlier session's exit-94
  work), rejected → 93 `push_rejected`; remote ahead or equal → switch to
  B, `merge --ff-only`; diverged → 93 `diverged`. Only the requested branch
  is considered.
- **Finish:** local B missing → 94 `branch_not_found`; ahead of
  `origin/B` (or no tracking ref) → `push --porcelain origin
  refs/heads/B:refs/heads/B`; a `!` line → 94 `push_rejected`, other
  failure → 94 `push_failed`. The checkout is never modified on failure.
  Uncommitted changes the agent left are not pushed; the next Prepare
  reports them as dirty.
- **Typed error:** `*repo.SyncError{Stage, Reason, Branch, Detail}` with
  `ExitCode()` (before → 93, after → 94), `errors.Is` by reason. Detail is
  git's stderr, capped at 1 KiB.
- **Git invocation:** one `run` function builds every argv; no shell. A
  guard there refuses the subcommands `reset`, `rebase`, `stash`, `clean`
  and any `-f`, `--force*`, `--mirror`, `--delete`, `-d`, `-D`, `--hard`
  argument or `+`-prefixed refspec, before git runs. Environment: the node
  user's own (SSH agent, credential helper), with `GIT_TERMINAL_PROMPT=0`,
  `LC_ALL=C`, and repository-locating variables (`GIT_DIR`,
  `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`,
  `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_COMMON_DIR`, `GIT_NAMESPACE`)
  removed. No credential is ever passed.
- **Queue** is in-process (one node daemon per workspace). Holding a
  checkout across a daemon restart is 1.7's concern.

## Declared file scope

- **owned_paths:** internal/repo/
- **read_only_paths:** internal/core/, OVERVIEW.md, .metis/adr/0010-repo-checkouts-on-nodes.md, docs/contracts/launcher-v1.md, deps_test.go

## Definition of Done

- A session starts at exactly the remote branch's tip (Prepare's head
  equals the remote ref), including when the caller has unpushed commits
  and when the branch exists only on the remote.
- Commits made in the checkout are on the remote after Finish returns nil.
- A dirty checkout (93 `dirty`), a diverged branch (93 `diverged`) and a
  rejected push (94 `push_rejected`) each return their typed error and leave
  the work tree files, HEAD and every local branch ref as they were.
- Work left unpushed by an earlier rejected push is pushed by the next
  Prepare; if that push is rejected, Prepare fails with 93.
- A test fails if the package invokes a forbidden git subcommand or flag:
  the guard refuses them, and a recorder over the whole test run asserts
  no forbidden argv reached git.
- URLs crafted to escape the workspace, and unsafe URLs, are rejected.
- Two Acquires on one checkout never overlap; a queued Acquire honours ctx.
- `make verify` green; `internal/repo` imports only `core` and the
  standard library.

## Test plan

`internal/repo/*_test.go`, real `git` against local bare remotes in
`t.TempDir()`, with isolated git config (`GIT_CONFIG_GLOBAL`,
`GIT_CONFIG_NOSYSTEM`) and fixed author identity:

- `TestDirName*`: table of URLs incl. `../..`, `/`, `..`, `%2e%2e`,
  `foo/../../x`, very long and unicode URLs → single safe component,
  distinct URLs distinct names, deterministic.
- `TestValidateURL`: rejected forms (`-u…`, `ext::sh -c …`, `fd::`,
  password in URL, newline, empty) and accepted forms.
- `TestAcquireRejectsSymlinkCheckout`: a symlink at the checkout name is
  refused.
- `TestPrepareClonesAndStartsAtRemoteTip`, `…CallerAhead`, `…NewBranch`
  (remote only), `…FastForwards`, `…BranchNotFound`, `…InvalidBranch`.
- `TestPrepareDirty` (modified and untracked file), `TestPrepareDiverged`:
  typed error, snapshot of work tree bytes + HEAD + local refs unchanged.
- `TestFinishPushes`, `TestFinishNothingToPush`,
  `TestFinishRejected` (remote moved on): 94, snapshot unchanged.
- `TestPreparePushesEarlierUnpushedWork` and `…RejectedIs93` (pre-receive
  hook toggled by a marker file).
- `TestWrongRemote`: existing checkout with another origin is refused.
- `TestGuardRefusesForbidden`: each forbidden argv is refused before git
  runs; `TestMain` recorder fails the run if any forbidden argv was seen.
- `TestAcquireQueues`: concurrent holders never overlap; ctx cancel while
  queued returns the ctx error and does not take the checkout.

## Out-of-scope touches

None.

## Report

Filled at implementation.
