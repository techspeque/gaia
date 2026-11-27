# Contributing to Gaia

Thanks for helping. Gaia is MIT licensed; by contributing you agree that your
contribution is licensed under the same terms.

## Build and test

You need the Go version pinned in `go.mod`. Every build runs with
`CGO_ENABLED=0`.

```sh
make verify      # gofmt -l, go vet ./..., staticcheck ./..., CGO_ENABLED=0 go test ./...
make build       # dist/gaia for this machine
make build-all   # the six release targets into dist/; fails if CGO is enabled
```

`make verify` must pass before a change is merged. CI runs it on Linux and
macOS and cross-compiles every target on Linux.

staticcheck is pinned as a Go tool in `go.mod`; `go tool staticcheck` runs
that version with no separate install.

## Rules for changes

- Package dependencies follow OVERVIEW.md §2.3: `host` and `node` use the
  leaf packages; a leaf imports only `internal/core` (`proxy → creds` is the
  one exception); `internal/metis` is imported only by `host` and
  `internal/ship` only by `node`. Gaia never imports Metis Go packages.
  `deps_test.go` enforces this.
- Every new direct dependency needs a line in the pull request saying why.
- Examples, fixtures and docs contain no real hostnames, accounts or
  credentials.
- Security issues go through [SECURITY.md](SECURITY.md), not public issues.
