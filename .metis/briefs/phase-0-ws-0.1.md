# phase-0-ws-0.1 — Module scaffold and verify gate

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.1

## Goal

Create the Go module `github.com/techspeque/gaia` with the OVERVIEW §2.3 package
layout, a `make verify` gate that every later slice runs, a test that enforces
the §2.3 dependency rule, cross-compilation of the six release targets with CGO
off, CI, the open-source scaffolding files and `gaia --version`.

## Architectural context

- OVERVIEW §2.3 component structure and dependency rule: `host` and `node`
  depend on the leaves; leaves depend on `core` and never import each other
  except `proxy → creds`; `internal/metis` is imported only by `host`,
  `internal/ship` only by `node`.
- ADR-0001: no import of `github.com/techspeque/metis` (any package under it).
- ADR-0002: one static binary, `CGO_ENABLED=0` in every build, CI fails otherwise;
  targets darwin, linux, windows × amd64, arm64; every new direct dependency
  is justified.
- ADR-0012: MIT, SECURITY.md with a private channel and response target, no
  real hostnames, accounts or personal names in the repository.
- No prior briefs; `metis interfaces` is not configured.

Decisions:
- Packages are placeholders (`doc.go` with a package comment only), except
  `cmd/gaia`, which implements `--version`.
- Version, commit and platform: `version` and `commit` are set by `-ldflags -X`
  at build time; when unset, commit falls back to `vcs.revision` from
  `runtime/debug.ReadBuildInfo`; platform is `runtime.GOOS/GOARCH`.
- The dependency-rule test lives at the module root (`deps_test.go`, a
  test-only package) and parses imports with the standard library
  (`go/parser`, ImportsOnly) over `cmd/` and `internal/`, test files included.
  A package's group is its first path element under `internal/`, so
  sub-packages belong to their parent's group. Any `internal/` package other
  than `core`, `host` and `node` is a leaf, so packages added later are
  covered by default.
- staticcheck is pinned as a Go `tool` dependency in `go.mod`
  (`go tool staticcheck`). This is the one new dependency: it pins the linter
  version for local and CI runs and yields the `go.sum` the plan asks for. It
  is a tool directive only, so it is not linked into the binary.
- `make build-all` sets `CGO_ENABLED=0`, refuses to run when the environment
  sets `CGO_ENABLED` to anything else, and checks each binary's embedded
  build settings (`go version -m`) for `CGO_ENABLED=0`.

## Declared file scope

- **owned_paths:** go.mod, go.sum, Makefile, deps_test.go, cmd/, internal/, .github/workflows/ci.yml, .gitignore, LICENSE, README.md, SECURITY.md, CONTRIBUTING.md, .metis/briefs/phase-0-ws-0.1.md
- **read_only_paths:** OVERVIEW.md, .metis/plans/phase-0-plan.md, .metis/adr/, docs/contracts/, testdata/

## Definition of Done

- `go.mod` declares `module github.com/techspeque/gaia` and pins the Go version; `go.sum` is committed.
- `cmd/gaia` and `internal/{core,host,node,repo,supervise,creds,proxy,net,store,metis,ship}` exist and compile.
- `make verify` runs `gofmt -l` (fails on any listed file), `go vet ./...`, `staticcheck ./...` and `CGO_ENABLED=0 go test ./...`, and passes on a clean checkout.
- The dependency-rule test fails on a leaf-to-leaf import other than `proxy → creds`, on `core` importing any module package, on `metis` imported by anything but `host`, on `ship` imported by anything but `node`, and on any import of `github.com/techspeque/metis`.
- `make build-all` produces six binaries (darwin, linux, windows × amd64, arm64) and fails when `CGO_ENABLED` is set to anything other than 0.
- `.github/workflows/ci.yml` runs `make verify` on ubuntu and macOS and `make build-all` on ubuntu.
- LICENSE (MIT), README (pre-alpha), SECURITY.md (private reporting through GitHub, response target), CONTRIBUTING.md (build and test commands).
- `gaia --version` prints version, commit and platform.
- No real hostname, account or personal name is added.

## Test plan

- `deps_test.go`:
  - `TestDependencyRule`: the real module's imports satisfy the rule.
  - `TestDependencyRuleRejects`: table test over synthetic import sets; each forbidden edge (leaf→leaf, `creds → proxy`, `core → store`, `node → metis`, `host → ship`, `store → github.com/techspeque/metis/...`) is reported, and the allowed edges (`proxy → creds`, `host → store`, `node → ship`, `host → metis`, leaf → `core`) are not.
  - `TestCollectImports`: writes a small fake module tree to a temp dir with a forbidden import and checks the parser-based collector finds it, so the real-repo check is not vacuous.
- `cmd/gaia/main_test.go`: `--version` output contains the version, a commit and `GOOS/GOARCH`; ldflags-style values are used when set.
- Manual: `make verify`, `make build-all`, `CGO_ENABLED=1 make build-all` (must fail).

## Out-of-scope touches

None.

## Report

- Added: `go.mod` (go 1.26.3, tool `honnef.co/go/tools/cmd/staticcheck` v0.8.2), `go.sum`, `Makefile`, `deps_test.go`, `cmd/gaia/{main,main_test}.go`, `internal/*/doc.go` (11 placeholders), `.github/workflows/ci.yml`, `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`. Changed: `README.md`, `.gitignore` (`/dist/`).
- `make verify`: pass (gofmt, vet, staticcheck, tests).
- `make build-all`: six binaries in `dist/`, each checked `CGO_ENABLED=0` via `go version -m`.
- `CGO_ENABLED=1 make build-all` and `make build-all CGO_ENABLED=1`: both exit non-zero at `cgo-guard`.
- `go run -ldflags "-X main.version=v0.0.0-test -X main.commit=abc123" ./cmd/gaia --version` → `gaia v0.0.0-test (commit abc123) darwin/arm64`.
- A temporary `internal/store` → `internal/creds` import made `TestDependencyRule` fail with the leaf-rule message; removed.
- Not verified here: CI green on the integration branch (needs a push).
- Grep for personal names and email in added files: no hits. The module path `github.com/techspeque/gaia` is the one the plan names.
