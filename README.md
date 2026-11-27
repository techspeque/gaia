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

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md), never in a public issue.
