# phase-0-ws-0.8 — Release pipeline and install script

- **Type:** feat | **Risk:** medium | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.8

## Goal

A `v*` tag builds the six targets with GoReleaser (same build tags as the
`Makefile`), smoke-tests the linux binaries, and publishes archives,
checksums, keyless cosign signatures and SBOMs; a manual dispatch does the
same as a dry run that publishes nothing; `scripts/install.sh` installs a
release only after the checksum and, where cosign is available, the
signature match; CI runs `govulncheck` and a licence check.

## Architectural context

- OVERVIEW §3.1 (CGO off, six static targets), §3.2/§3.3 (no telemetry,
  listeners), §5 (signed releases), §8 (install script verifying checksums
  and signatures, no root).
- ADR-0002 rules 1, 2, 5; ADR-0012 rules 2, 3, 7.
- Consumes: `Makefile` `TAGS := ts_omit_peerapiserver` and `LDFLAGS`
  (`main.version`, `main.commit`) from 0.1/0.6; `cmd/gaia` `--version`
  output; the test helpers in `cmd/gaia/daemon_test.go` and
  `cmd/gaia/tailnet_test.go` (`daemon`, `inetSockets`, `tcpListeners`,
  `waitReady*`, `errNoSocketLister`) and `internal/net/nettest.StartControl`
  for the tailnet listener check. Report of 0.6: the release build must carry
  `ts_omit_peerapiserver`.

Design decisions:

- **`.goreleaser.yaml`** (GoReleaser v2): one build, `CGO_ENABLED=0`,
  `-trimpath`, `tags: [ts_omit_peerapiserver]`, ldflags as the `Makefile`
  (`-s -w -X main.version={{.Version}} -X main.commit={{.ShortCommit}}`),
  goos darwin/linux/windows × goarch amd64/arm64. Archives
  `gaia_<version>_<os>_<arch>.tar.gz` (zip for windows) with LICENSE and
  README. `checksums.txt` (sha256). `sboms:` one SPDX SBOM per archive
  (syft). `signs:` cosign `sign-blob --bundle` keyless (GitHub OIDC) over
  every archive and `checksums.txt`, producing `<file>.sigstore.json`.
  Release is created as a **draft**; it is published only after the smoke job
  passes.
- **`.github/workflows/release.yml`:** on `push: tags: v*` and
  `workflow_dispatch`. Job `goreleaser`: tag → `goreleaser release --clean`
  (draft); dispatch → `goreleaser release --snapshot --clean --skip=sign`
  (dry run: nothing pushed to GitHub and nothing written to the public
  transparency log). Both then run `scripts/check-dist.sh dist` (six
  archives, `checksums.txt` listing them, one SBOM per archive) and upload
  `dist/` as a workflow artifact. Job `smoke` (matrix ubuntu-latest amd64,
  ubuntu-24.04-arm arm64): checks the linux binary is statically linked
  (`file`), runs `--version` in a `busybox` container (no Go toolchain, no
  libc), and runs the release smoke tests with `GAIA_RELEASE_BIN` under
  `strace`. Job `publish` (tags only, needs smoke):
  `gh release edit <tag> --draft=false`. Permissions per job, minimal
  (`contents: write` and `id-token: write` only where needed).
- **Release smoke tests** (`cmd/gaia/release_test.go`), skipped unless
  `GAIA_RELEASE_BIN` names a binary:
  - local-only, no network request: on linux, run `--version`, `help`,
    `host`, `host --node` (with `nodes`, `nodes -o json` against it, then
    SIGTERM) under `strace -f -e trace=network`; fail on any `connect`,
    `sendto`, `sendmsg`, `bind` or `socket` call with `AF_INET`/`AF_INET6`.
    With `GAIA_RELEASE_BIN` set on linux, a missing `strace` fails the test
    (no silent skip in CI). On every OS, the running daemons also hold no
    inet socket (`inetSockets`).
  - after joining a tailnet (`host --tailnet` against `nettest.StartControl`),
    the release binary has no TCP listener (`tcpListeners`), the check of
    plan §0.8 task 1.
- **`scripts/install.sh`** (POSIX sh, no root): detects os/arch
  (darwin/linux × amd64/arm64; others refused), resolves the version
  (`GAIA_VERSION`, else the redirect of `<releases>/latest`), downloads the
  archive, `checksums.txt` and `checksums.txt.sigstore.json` into a private
  temp dir, verifies with `cosign verify-blob` (certificate identity: this
  repository's `release.yml` workflow on a `refs/tags/v` ref; issuer GitHub
  Actions) when `cosign` is on `PATH`, then the archive's sha256 against
  `checksums.txt` (`sha256sum` or `shasum -a 256`), and only then extracts
  and installs `gaia` into `GAIA_INSTALL_DIR` (default `$HOME/.local/bin`).
  Any mismatch, a missing entry or a failed download exits non-zero and
  installs nothing. Without `cosign` it prints that only the checksum was
  verified (ADR-0012 rule 3: "where available") and continues, unless
  `GAIA_REQUIRE_SIGNATURE=1`, which refuses. `GAIA_RELEASES_URL` overrides the
  release base URL (tests, mirrors).
- **CI (`ci.yml`):** job `vulncheck` runs `make vulncheck`
  (`go run golang.org/x/vuln/cmd/govulncheck@v1.10.0` with the build tags;
  govulncheck exits non-zero on a reachable known vulnerability), plus a step
  proving that: govulncheck on `testdata/vulnmod` (a module calling a known
  vulnerable `golang.org/x/text` function) must fail. Job `licenses` runs
  `make licenses` (`go run github.com/google/go-licenses/v2@v2.0.1 check`
  with an allowlist of permissive licences). Neither is in `make verify`,
  which stays offline.
- **README:** a "Platforms" section listing exactly what CI builds and tests,
  and an "Install" section for `install.sh`.

## Declared file scope

- **owned_paths:** .goreleaser.yaml, .github/workflows/, scripts/, cmd/gaia/release_test.go, install_test.go, testdata/vulnmod/, Makefile, README.md
- **read_only_paths:** cmd/gaia/, internal/, deps_test.go, go.mod, go.sum, OVERVIEW.md, .metis/adr/

## Definition of Done

- `goreleaser release --snapshot --clean --skip=sign` (the dry run) produces
  six archives, `checksums.txt` and one SBOM per archive, publishes nothing,
  and `scripts/check-dist.sh` passes on it; `goreleaser check` passes.
- `install.sh` installs a matching archive and refuses (non-zero, nothing
  installed) a checksum mismatch, a missing checksum entry and a failed
  signature check; without cosign it warns, and refuses with
  `GAIA_REQUIRE_SIGNATURE=1`.
- The linux release binaries are static and `--version` runs in a container
  with no Go toolchain (release workflow).
- `make vulncheck` passes on the module and govulncheck fails on
  `testdata/vulnmod`; `make licenses` passes.
- The release binary makes no network request on any Phase 0 local-only
  command, and has no TCP listener after joining a tailnet (release smoke
  tests, run locally against a snapshot build where the OS allows).
- README lists the platforms CI builds and tests.
- `make verify` green.

## Test plan

- `install_test.go` (module root, runs in `make verify` on linux/darwin):
  serves a fake release (tar.gz holding a stub `gaia`, `checksums.txt`,
  bundle) from `httptest`, with a fake `cosign` on `PATH`; cases: good
  install; `latest` resolved by redirect; checksum mismatch refused; archive
  missing from `checksums.txt` refused; signature failure refused; no cosign
  warns and installs; no cosign with `GAIA_REQUIRE_SIGNATURE=1` refused;
  unsupported OS refused. Each refusal asserts the install dir is empty.
- `scripts/check-dist.sh` exercised on a real snapshot build locally and in
  the release workflow.
- `cmd/gaia/release_test.go`: the two smoke tests above, skipped without
  `GAIA_RELEASE_BIN`; run locally against the snapshot darwin binary
  (socket checks) and in the release workflow on linux (strace).
- `testdata/vulnmod` proves govulncheck fails on a known vulnerability.

## Out-of-scope touches

None.

## Report

- **Built:** `.goreleaser.yaml` (six targets, CGO off, `-trimpath`,
  `ts_omit_peerapiserver`, Makefile ldflags; tar.gz/zip archives, sha256
  `checksums.txt`, SPDX SBOM per archive, cosign keyless `--bundle`
  signatures on every artifact, draft release); `.github/workflows/release.yml`
  (tag: draft release → smoke on linux amd64 + arm64 → publish; dispatch:
  snapshot with `--skip=sign`, no publish); `ci.yml` jobs `vulncheck`
  (+ `vulncheck-proof`) and `licenses`; `scripts/install.sh`;
  `scripts/check-dist.sh`; `cmd/gaia/release_test.go`; `install_test.go`;
  `testdata/vulnmod`; Makefile targets `snapshot`, `release-smoke`,
  `vulncheck`, `vulncheck-proof`, `licenses`; README "Install" and "Platforms".
- **Tool pins:** GoReleaser v2.20.3, govulncheck v1.10.0, go-licenses v2.0.1.
  GoReleaser and govulncheck are `go install`ed into `.local/bin` (ignored):
  `go run` hides govulncheck's exit code, and for GoReleaser (needs Go ≥
  1.27.4) it passed the switched toolchain into Gaia's build, which then
  failed (go-json-experiment). Installed binaries build Gaia with the
  `go.mod` toolchain.
- **Run locally (darwin/arm64):** `goreleaser check` OK; `make snapshot` →
  6 archives, `checksums.txt`, 6 SBOMs, `check-dist` OK, nothing published;
  `make release-smoke` on the snapshot darwin binary: 3/3 pass; against a
  binary built without the tag, `TestReleaseBuildSettings` and
  `TestReleaseTailnetNoTCPListener` fail (`TCP *:58436 (LISTEN)`), so both
  checks bite; `go test -run TestInstall .` 10/10; `make vulncheck-proof` OK
  (exit 3, GO-2021-0113); `make licenses` OK; `make verify` green.
- **Not run locally:** the strace path of `TestReleaseNoNetworkLocalOnly`,
  the static/busybox checks and arm64 (linux-only; Docker not permitted in
  this session). They run first in the release workflow.
- **Open, needs a person:** `make vulncheck` FAILS on the current module:
  ~15 reachable stdlib vulnerabilities in go1.26.3 (e.g. GO-2026-6859,
  GO-2026-6613, fixed by go1.26.11) and GO-2026-6865 in
  `github.com/klauspost/compress` v1.18.5 (indirect via tailscale, fixed
  v1.20.2). Tried in a throwaway worktree: `go get go@1.26.11
  github.com/klauspost/compress@v1.20.3` gives "No vulnerabilities found" and
  `make verify` green. `go.mod`/`go.sum` are read-only for this slice and the
  toolchain pin was kept deliberately in 0.6, so the bump is not made here.
