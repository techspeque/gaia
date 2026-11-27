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

## Build

Requires the Go version pinned in `go.mod`. CGO is always off.

```sh
make build        # dist/gaia for this machine
make build-all    # darwin, linux, windows × amd64, arm64 into dist/
make verify       # gofmt, go vet, staticcheck, tests
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
  (`you@example.com`), a tag (`tag:gaia`) or a node name (`node:laptop`).
  Give entries with `--allow ENTRY` (repeatable) or `--allow-file PATH`, one
  per line with `#` comments. A caller who matches no entry gets `403` and
  the host logs a warning. `--tailnet` with an empty allowlist refuses to
  start.
- **Certificates.** With Tailscale and HTTPS enabled for the tailnet, the
  host serves the Tailscale-issued certificate for its MagicDNS name. When
  none is available (Headscale, or a client using the bare tailnet address),
  it serves a self-signed certificate made on first start and kept in
  `<state>/tsnet/gaia-tls.{crt,key}`.

## Adding a machine

Run `gaia node` on each other machine. It joins the same tailnet, registers
with the host and heartbeats, and answers `gaia` commands on that machine
by passing them to the host. So `gaia nodes` lists every machine wherever
you run it.

```sh
# Once: join the tailnet with an auth key (as for the host).
GAIA_AUTHKEY=tskey-auth-… gaia node --host my-host --join

# Every start after that needs no key.
gaia node --host my-host
```

- **`--host NAME`** is the host's tailnet name: its MagicDNS name, the first
  label of it, or its tailnet IP. The host's allowlist must admit the node
  (for example `--allow tag:gaia`, or `node:NAME` per machine).
- **`--name NAME`** is the node's name in the registry and on the tailnet.
  It defaults to the machine name. Names use `a-z`, `0-9` and `-`. Each
  machine keeps its name: another machine cannot register under it.
- **Heartbeats.** The host sets the interval (`gaia host
  --heartbeat-interval 10s`, the default) and marks a node `offline` after
  `--missed-heartbeats` (default 3) intervals without one. A node keeps
  running while the host is down, retries with backoff, and registers again
  when the host returns. Restarted with the same state directory, a node
  keeps its id.
- **Versions.** The host accepts nodes at its own API version and one
  behind. Any other node is refused, with the command that fixes it.
- **State.** A node's state directory holds only the tailnet client's state
  (`<state>/tsnet`): no auth key and no credentials.
- **Removing a machine.** `gaia nodes remove NAME` removes a retired
  machine. A node still running registers again at its next heartbeat, so
  stop `gaia node` on it first.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md), never in a public issue.
