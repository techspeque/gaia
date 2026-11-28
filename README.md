# gaia

> **Status: pre-alpha.** Nothing here is usable yet. Interfaces, commands and
> on-disk formats will change without notice. Do not run it against real
> credentials.

Gaia is one self-hosted binary that runs coding-agent sessions (Claude Code,
opencode, Codex) on any of your machines, for hours, with every credential in
one place, and lets you watch them from anywhere. A **host** holds credentials
and the session registry; a **node** on each machine runs sessions. Machines
reach each other over a tailnet, with no ports to open.

Open source under the MIT licence, self-hosted only, no telemetry.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/techspeque/gaia/main/scripts/install.sh | sh
```

The script installs `gaia` into `~/.local/bin` for the current user, with no
root. It installs nothing until both checks pass:

- **Signature.** With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
  on `PATH`, it verifies the keyless signature of `checksums.txt`, which must
  come from this repository's release workflow on a `v*` tag. Without cosign
  it warns and checks the checksum only; set `GAIA_REQUIRE_SIGNATURE=1` to
  refuse instead.
- **Checksum.** The archive's sha256 must match its line in `checksums.txt`.

Set `GAIA_VERSION=v0.1.0` for a given release (default: the latest) and
`GAIA_INSTALL_DIR` for another directory. Each release also carries an SPDX
SBOM per archive (`*.sbom.json`) and a signature bundle per file
(`*.sigstore.json`).

## Platforms

Release archives are built for exactly six targets: darwin, linux and
windows, each on amd64 and arm64, all with CGO off. `install.sh` covers
darwin and linux. On Windows the daemon roles refuse to start for now.

What CI checks, on every push:

| Check | Where it runs |
|---|---|
| `make verify` (gofmt, vet, staticcheck, all tests) | linux/amd64 (`ubuntu-latest`), darwin/arm64 (`macos-latest`) |
| `make build-all` (all six targets compile, CGO off) | linux/amd64 |
| `govulncheck` and the licence check | linux/amd64 |
| `install.sh` tests (part of `make verify`) | linux/amd64, darwin/arm64 |

And on every release (and its dry run): the six archives, checksums and
SBOMs are checked, and the linux/amd64 and linux/arm64 binaries are checked
to be static, run in a container with no Go toolchain and no libc, and pass
the release smoke tests (no network request in local-only mode, no TCP
listener after joining a tailnet). darwin and windows release binaries are
built but not run in CI.

## Build

Requires the Go version pinned in `go.mod`. CGO is always off.

```sh
make build        # dist/gaia for this machine
make build-all    # darwin, linux, windows × amd64, arm64 into dist/
make verify       # gofmt, go vet, staticcheck, tests
```

Release tooling (fetches pinned tools, so it needs the network):

```sh
make snapshot     # release dry run into dist/: archives, checksums, SBOMs (needs syft)
make release-smoke GAIA_RELEASE_BIN=dist/gaia_linux_amd64_v1/gaia
make vulncheck    # govulncheck; fails on a reachable known vulnerability
make licenses     # every dependency under a permissive licence
```

```sh
dist/gaia --version
```

Every build, test and analysis uses the build tag `ts_omit_peerapiserver`,
set once in the `Makefile`. It removes the tailnet client's peer API server,
which would otherwise open a TCP listener on all interfaces. A build or test
run without the tag (plain `go test ./...`) fails the check that Gaia opens
no TCP listener outside the tailnet; use the `make` targets.

## Joining a tailnet

`gaia host --tailnet` joins a tailnet with an embedded Tailscale client and
serves the API over HTTPS on port 443 of its tailnet address only. Nothing
is opened on the machine's own network. Without `--tailnet` the host makes no
network call at all. Tailscale and Headscale work the same way; only the
control server differs.

```sh
# Tailscale (the default control server)
GAIA_AUTHKEY=tskey-auth-… gaia host --tailnet --allow you@example.com

# Headscale, or any other control server
gaia host --tailnet --control-url https://headscale.example.com \
  --auth-key-file ~/.config/gaia/authkey --allow tag:gaia
```

- **Control server.** `--control-url URL` names it. Leave it out for
  Tailscale's. For Headscale, give the URL of your Headscale server.
- **Auth key.** Create a key in the Tailscale admin console, or with
  `headscale preauthkeys create`. Gaia reads it from `$GAIA_AUTHKEY` or
  `$TS_AUTHKEY`, or from `--auth-key-file PATH` (the file must not be group
  or other readable). It is never a command-line argument and never logged.
  After the first join the node key lives in the state directory
  (`<state>/tsnet`), and later starts need no auth key.
- **Allowlist.** Every tailnet request is checked against the identity the
  control server gives the caller. An entry is a login name
  (`you@example.com`), a tag (`tag:gaia`) or a node's tailnet name
  (`node:gaia-laptop`). `node:` takes the name the control server assigned
  the node, the `TAILNET NAME` column of `gaia nodes` and the `tailnet_name`
  the node logs at start, never the name the node claims for itself.
  Give entries with `--allow ENTRY` (repeatable) or `--allow-file PATH`, one
  per line with `#` comments. A caller who matches no entry gets `403` and
  the host logs a warning. `--tailnet` with an empty allowlist refuses to
  start.
- **Certificates.** With Tailscale and HTTPS enabled for the tailnet, the
  host serves the Tailscale-issued certificate for its MagicDNS name. When
  none is available (Headscale, or a client using the bare tailnet address),
  it serves a self-signed certificate made on first start and kept in
  `<state>/tsnet/gaia-tls.{crt,key}`.
- **Name.** The host asks the control server for the tailnet name
  `gaia-<machine name>` (on a machine called `studio`, `gaia-studio`), so it
  never collides with the machine's own Tailscale device, which already
  holds the machine name. `--name NAME` chooses another. The name is kept
  in `<state>/name` and reused on every later start; with `--node`, the
  host's own node is registered under it too. Once joined, the host
  logs the name it was given (`"msg":"tailnet name assigned"`, with
  `tailnet_name` and `tailnet_dns_name`). If the control server still gave
  it another name, typically with a suffix because the name was taken, it
  also logs a warning naming the `--host` nodes must use.

## Adding a machine

Run `gaia node` on each other machine. It joins the same tailnet, registers
with the host and heartbeats, and answers `gaia` commands on that machine
by passing them to the host. So `gaia nodes` lists every machine wherever
you run it.

```sh
# Once: join the tailnet with an auth key (as for the host).
# The host runs on the machine "studio", so its tailnet name is gaia-studio.
GAIA_AUTHKEY=tskey-auth-… gaia node --host gaia-studio --join

# Every start after that needs no key.
gaia node --host gaia-studio
```

- **`--host NAME`** is the host's tailnet name, the `tailnet_name` the host
  logs at start: `gaia-<host machine name>` unless the host was given
  `--name`. Its full MagicDNS name and its tailnet IP work too. It is not
  the host machine's own Tailscale name: that device runs no Gaia host, and
  a node pointed at it logs that nothing answers on port 443. The host's
  allowlist must admit the node (for example `--allow tag:gaia`, or
  `node:<the node's tailnet name>` per machine).
- **`--name NAME`** is the node's name in the registry and the tailnet name
  it asks for. It defaults to `gaia-<machine name>` and is kept in
  `<state>/name`. Names use `a-z`, `0-9` and `-`. Each machine keeps its
  name: another machine cannot register under it. The node logs the tailnet
  name it was given at start. When the control server gave it another name
  than it asked for, the node logs a warning, `gaia nodes` shows that name
  under `TAILNET NAME`, and the host's `node:` entry must use it.
- **Machines set up with Gaia v0.0.1.** A state directory that joined the
  tailnet, or (for the host) held a registry, before names were kept keeps
  the name it had then, the machine name, and records it. To move such a
  machine to its `gaia-` name: stop the daemon, remove the device from the
  tailnet (admin console, or `headscale nodes delete`), delete
  `<state>/tsnet` and `<state>/name`, and join again with an auth key
  (`gaia node --host … --join`; for the host, keep `<state>/gaia.db`, the
  registry, and start `gaia host --tailnet --name gaia-<machine name>` with
  `$GAIA_AUTHKEY` set: without `--name` the kept registry makes it ask for the
  machine name again). The new name is recorded, so later starts need no
  `--name`. A re-joined node is a new tailnet identity and
  registers as a new node: remove the old entry with
  `gaia nodes remove <old name>`. Nodes then use the host's new name in
  `--host`, and `node:` entries the nodes' new names.
- **Heartbeats.** The host sets the interval (`gaia host
  --heartbeat-interval 10s`, the default) and marks a node `offline` after
  `--missed-heartbeats` (default 3) intervals without one. A node keeps
  running while the host is down, retries with backoff, and registers again
  when the host returns. Restarted with the same state directory, a node
  keeps its id. Until a node's first heartbeat after registering is
  answered, which is normally about a second, a `gaia` command on that machine
  waits at most 3 s for the host. If the host does not answer in that time,
  the command says the node is re-establishing its connection to the host.
  Run it again a few seconds later.
- **Versions.** The host accepts nodes at its own API version and one
  behind. Any other node is refused, with the command that fixes it.
- **State.** A node's state directory holds only the tailnet client's state
  (`<state>/tsnet`) and its name (`<state>/name`): no auth key and no
  credentials. The tailnet client writes nothing outside `<state>/tsnet`,
  its logs directory included.
- **Removing a machine.** `gaia nodes remove NAME` removes a retired
  machine. A node still running registers again at its next heartbeat, so
  stop `gaia node` on it first.

## Running as a service

`gaia service install` runs the daemon as a service of the current user,
started for you and restarted if it fails: a systemd user unit on Linux
(`~/.config/systemd/user/gaia.service`), a launchd agent on macOS
(`~/Library/LaunchAgents/io.github.techspeque.gaia.plist`, logging to
`~/Library/Logs/gaia/gaia.log`). It needs no root and never runs `sudo`.
How it behaves at logout and reboot differs by platform; see the last
points below.

```sh
gaia service install host --node                     # local-only
gaia service install host --tailnet --allow tag:gaia # a tailnet host
gaia service install node --host gaia-studio         # another machine
gaia service uninstall
```

- The role takes the flags of `gaia host` or `gaia node`, `--name`
  included. `--join` and
  `--auth-key-file` are refused: a unit never holds an auth key. For a
  tailnet host or a node, join once by hand first (see above), stop it, then
  install; install refuses a state directory with no tailnet state.
- The unit runs this binary with an absolute `--state` (default as for the
  daemon), and carries only `XDG_RUNTIME_DIR` and `TMPDIR` from your shell,
  so `gaia` commands in it find the daemon's socket.
- Installing twice changes nothing and says so; installing with other flags
  rewrites the unit and restarts the service. Gaia marks every unit it
  writes and refuses to overwrite or remove one it did not write.
- `gaia service uninstall` stops the service and removes the unit. The state
  directory stays.
- On Linux, a user service stops at logout and does not start at boot
  unless lingering is on. When it is off, install prints the one command to
  run, `loginctl enable-linger USER`; Gaia never runs it. With lingering
  on, it survives logout and starts at boot.
- On macOS, the launchd agent runs in your login session: it stops when you
  log out and starts again when you next log in. After a reboot it starts
  at your login, not at boot.
- Windows is not supported yet.

## Footprint

Measured on v0.0.2 (linux/amd64), Phase 0 gate, two Ubuntu 26.04 machines
on a tailnet, each daemon a user service idle for about 11 minutes with two
registered nodes and no sessions:

| | Resident memory (VmRSS) | CPU, idle |
|---|---|---|
| `gaia host --tailnet --node` | 41.7 MB | 0.09 s per minute |
| `gaia node` | 39.9 MB | 0.10 s per minute |

The service's cgroup reports about 20 MB (`MemoryCurrent`) for each; VmRSS
also counts the binary's shared pages. Release binary: 26.2 MB
(linux/amd64, 26,198,178 bytes); release archives 8.6–10.6 MB.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md), never in a public issue.
