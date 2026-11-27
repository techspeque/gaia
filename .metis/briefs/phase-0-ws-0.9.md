# phase-0-ws-0.9 — Service install

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.9

## Goal

`gaia service install (host [--node] [--tailnet …] | node --host NAME …)`
registers the daemon as a user-level service (systemd user unit on Linux,
launchd agent on macOS) and starts it, with no root, no secret in the unit
and idempotently; `gaia service uninstall` stops and removes only a unit
Gaia wrote and leaves the state directory in place; Windows is refused.

## Architectural context

- OVERVIEW §3.1 (user-level service, no root), §3.3 (no secret on disk on a
  node, safe defaults), §6 CLI (`gaia service install|uninstall`), §8
  (install). ADR-0002 (one binary, two roles), ADR-0004 (local socket,
  auth key never an argument).
- Consumes: the daemon flags of `cmd/gaia/daemon.go` (`runHost`) and
  `cmd/gaia/node.go` (`runNode`), `defaultStateDir`, `refuseDaemon`'s
  Windows rule, `internal/net.HasState`, `internal/sock.Dir` (the socket dir
  is `$XDG_RUNTIME_DIR/gaia`, else `$TMPDIR/gaia-UID`).

Design decisions:

- **New leaf `internal/service`** (imports nothing of this module but
  `core` at most): renders and installs/uninstalls the unit. All OS commands
  go through an injected `Runner`, and paths come from an injected
  `Platform` (GOOS, home, config home, uid, user name), so unit tests use a
  fake runner. Only `systemctl --user`, `loginctl show-user` and `launchctl`
  are ever run; never `sudo`, never `loginctl enable-linger`.
- **One service per machine**, since host and node bind the same socket:
  Linux `$XDG_CONFIG_HOME/systemd/user/gaia.service` (default
  `~/.config/…`), macOS `~/Library/LaunchAgents/io.github.techspeque.gaia.plist`
  (label `io.github.techspeque.gaia`, logs to `~/Library/Logs/gaia/gaia.log`).
- **Marker:** every unit carries the line `Written by gaia service install;
  gaia service uninstall removes it. Do not edit.` (a `#` comment, or an XML
  comment in the plist). A path that exists without the marker, or is not a
  regular file, is neither overwritten nor removed (error, exit 1).
- **Idempotent:** install renders the unit; if the existing Gaia unit is
  byte-identical, no file is written and, when the service is already
  enabled and running (`systemctl --user is-enabled/is-active`, `launchctl
  print gui/UID/label`), it prints that nothing changed and runs no
  state-changing command. A differing Gaia unit is rewritten atomically
  (temp + rename, 0644) and the service restarted. Uninstall of an absent
  unit prints nothing changed and exits 0.
- **Linux:** `daemon-reload`, `enable`, `start`/`restart`; `uninstall`:
  `disable --now`, remove, `daemon-reload`. After install, `loginctl
  show-user UID -p Linger --value`; unless `yes`, print the one command
  `loginctl enable-linger USER` and do not run it. Unit: `Type=simple`,
  `Restart=on-failure`, `WantedBy=default.target`.
- **macOS:** `launchctl enable gui/UID/label`, `bootstrap gui/UID PATH`
  (retried briefly after a `bootout`); uninstall: `bootout gui/UID/label`
  when loaded, then remove. Plist: `RunAtLoad`, `KeepAlive` on unsuccessful
  exit.
- **Arguments:** `cmd/gaia/service.go` parses the role arguments with a
  flag set accepting only: host `--state --node --tailnet --control-url
  --allow --allow-file --heartbeat-interval --missed-heartbeats`; node
  `--host (required) --name --state --control-url`. `--join` and
  `--auth-key-file` are refused with a message saying to join once by hand
  first. The same flag rules as the daemons hold (tailnet-only flags need
  `--tailnet`; `--tailnet` needs an allowlist). The unit's argv is the
  absolute `os.Executable()`, the role, and the canonical flags, with
  `--state` always present and absolute (default `defaultStateDir`) and
  `--allow-file` absolute. With `--tailnet` (host) or for a node, install
  refuses unless the state directory already holds tailnet state
  (`net.HasState`), so the unit never needs an auth key.
- **Environment:** the unit carries only the socket-dir variables as the
  installing shell sees them (`XDG_RUNTIME_DIR` if set, else unset on Linux;
  `TMPDIR` on macOS), so the CLI in that shell finds the daemon's socket. No
  other variable, and never `GAIA_AUTHKEY`/`TS_AUTHKEY`.
- **Windows:** refused with a clear message, exit 1.

## Declared file scope

- **owned_paths:** internal/service/, cmd/gaia/service.go, cmd/gaia/service_test.go, cmd/gaia/service_e2e_test.go, cmd/gaia/main.go, .github/workflows/ci.yml, README.md
- **read_only_paths:** cmd/gaia/, internal/, deps_test.go, OVERVIEW.md, .metis/adr/, .metis/plans/

## Definition of Done

- `gaia service install host|node …` writes the unit, enables and starts it;
  `gaia service uninstall` stops and removes it and leaves the state
  directory untouched (e2e test in CI on ubuntu and macos runners, `gaia
  nodes` answers while installed).
- A second install prints that nothing changed and writes no file (mtime
  and content unchanged, no state-changing command run).
- A unit at the path without the marker is neither overwritten nor removed.
- Only `systemctl --user`, `loginctl show-user` and `launchctl` are run;
  no `sudo` anywhere in the code path; Linux with lingering off prints the
  `loginctl enable-linger USER` command and does not run it.
- The rendered unit contains no credential: no auth key from the
  environment, `--join`/`--auth-key-file` refused.
- Windows refused with a clear message.
- `make verify` green.

## Test plan

- `internal/service` unit tests with a fake runner and temp home, for linux
  and darwin: render (argv quoting, marker, env, no key), fresh install
  (file + commands), second install unchanged (no write, mtime same, no
  state-changing command, "nothing changed"), changed unit (rewrite +
  restart), foreign unit refused on install and uninstall (file untouched),
  symlink refused, uninstall (commands, file gone) and uninstall absent,
  linger off/on, command failure surfaced, source contains no `sudo`.
- `cmd/gaia/service_test.go`: argument parsing (accepted flags, `--join` and
  `--auth-key-file` refused, tailnet rules, `--host` required, absolute
  `--state`/`--allow-file`, default state), tailnet-state precondition,
  Windows refusal, and an install with `GAIA_AUTHKEY`/`TS_AUTHKEY` set whose
  unit holds neither the key nor the names.
- `cmd/gaia/service_e2e_test.go`, run only with `GAIA_SERVICE_E2E=1` (CI job
  `service` on ubuntu-latest and macos-latest): builds the binary, installs
  `host --node`, waits for `gaia nodes` to list the machine online,
  reinstalls (nothing changed), uninstalls, checks the daemon is gone and
  the state directory is intact.

## Out-of-scope touches

None.

## Report

- **Built:** `internal/service` (`service.go`: `Platform`, `Spec`,
  `Install`, `Uninstall`, `UnitPath`, `Render`, `Marker`, `ErrNotOurs`;
  `render.go`: systemd unit and launchd plist); `cmd/gaia/service.go`
  (`gaia service install|uninstall`, argument parsing, tailnet-state
  precondition, socket-dir env, messages); `main.go` dispatch and usage;
  README "Running as a service"; CI job `service` (ubuntu-latest,
  macos-latest) running `TestServiceE2E` with `GAIA_SERVICE_E2E=1`.
- **Run locally (darwin/arm64):** `make verify` green;
  `go test ./internal/service` and `-run Service ./cmd/gaia` pass;
  `GOOS=windows go build ./cmd/gaia` and `GOOS=linux go vet` OK.
- **Not run locally:** `TestServiceE2E` (installs a real launchd agent;
  the command was not permitted in this session). It runs first in CI job
  `service`. On Linux the job runs `sudo loginctl enable-linger "$USER"` in
  a workflow step to give the runner a systemd user manager; that is runner
  setup, not Gaia's code path, which runs only `systemctl --user`,
  `loginctl show-user` and `launchctl` (checked by `TestOnlyUserCommands`).
- **Decisions:** flags are written as `--flag=value`; heartbeat flags only
  when given; the plist's `KeepAlive` restarts on unsuccessful exit only,
  so a clean SIGTERM stop stays stopped; `bootstrap` after `bootout` is
  retried up to 10× at 300 ms because launchd tears down asynchronously.
