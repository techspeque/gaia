# Gaia build and verify targets. `make verify` is the gate every change passes.

GO      ?= go
PKG     := github.com/techspeque/gaia
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
DIST    := dist

# Build tags for every build, test and analysis. ts_omit_peerapiserver drops
# tsnet's peerapi server, which would otherwise bind a TCP listener on all
# interfaces (OVERVIEW §3.3). The release build must carry the same tags.
TAGS    := ts_omit_peerapiserver

# Release targets, ADR-0002 rule 5.
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: all build build-all verify fmt-check vet staticcheck test clean

all: verify

verify: fmt-check vet staticcheck test

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet -tags '$(TAGS)' ./...

staticcheck:
	$(GO) tool staticcheck -tags '$(TAGS)' ./...

test:
	CGO_ENABLED=0 $(GO) test -tags '$(TAGS)' ./...

build: cgo-guard
	CGO_ENABLED=0 $(GO) build -tags '$(TAGS)' -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/gaia ./cmd/gaia

# Cross-compile every release target with CGO off, then prove from each
# binary's embedded build settings that CGO was off.
build-all: cgo-guard
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		out=$(DIST)/gaia_$${os}_$${arch}$$ext; \
		echo "build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -tags '$(TAGS)' -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/gaia; \
		$(GO) version -m $$out | grep -q 'CGO_ENABLED=0' || { echo "$$out was built with CGO enabled"; exit 1; }; \
	done

# Fail when the caller asks for CGO (ADR-0002 rule 1).
.PHONY: cgo-guard
cgo-guard:
	@if [ -n "$(CGO_ENABLED)" ] && [ "$(CGO_ENABLED)" != 0 ]; then \
		echo "CGO_ENABLED=$(CGO_ENABLED): Gaia builds with CGO disabled only (ADR-0002)"; exit 1; fi

clean:
	rm -rf $(DIST)

# Release tooling (workstream 0.8). These need the network to fetch the
# pinned tools, so they are CI jobs, not part of `make verify`.
GORELEASER_VERSION := v2.20.3
GOVULNCHECK        := golang.org/x/vuln/cmd/govulncheck@v1.10.0
GOLICENSES         := github.com/google/go-licenses/v2@v2.0.1
# Licences a dependency may carry: permissive ones compatible with MIT.
ALLOWED_LICENSES   := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0,0BSD,Unlicense

.PHONY: snapshot release-smoke vulncheck vulncheck-proof licenses

# The release dry run: every archive, the checksum file and the SBOMs into
# dist/, nothing signed and nothing published. Needs syft on PATH.
# GoReleaser is installed rather than run with `go run`: it needs a newer Go
# than go.mod pins, and `go run` would pass that toolchain to Gaia's build.
GORELEASER_BIN := $(CURDIR)/.local/bin/goreleaser
$(GORELEASER_BIN):
	GOBIN=$(CURDIR)/.local/bin $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

snapshot: $(GORELEASER_BIN)
	$(GORELEASER_BIN) release --snapshot --clean --skip=sign
	sh scripts/check-dist.sh $(DIST)

# Smoke tests against a built release binary: GAIA_RELEASE_BIN=path.
release-smoke:
	@if [ -z "$(GAIA_RELEASE_BIN)" ]; then echo "set GAIA_RELEASE_BIN to a release binary"; exit 1; fi
	GAIA_RELEASE_BIN='$(abspath $(GAIA_RELEASE_BIN))' CGO_ENABLED=0 $(GO) test -tags '$(TAGS)' -count=1 -v -run '^TestRelease' ./cmd/gaia

# govulncheck is installed, not run with `go run`, which hides its exit code.
GOVULNCHECK_BIN := $(CURDIR)/.local/bin/govulncheck
$(GOVULNCHECK_BIN):
	GOBIN=$(CURDIR)/.local/bin $(GO) install $(GOVULNCHECK)

# Fails on any known vulnerability reachable from Gaia's code.
vulncheck: $(GOVULNCHECK_BIN)
	$(GOVULNCHECK_BIN) -tags '$(TAGS)' ./...

# Proves vulncheck can fail: govulncheck must exit 3 (vulnerability found) on
# testdata/vulnmod and name the one planted there, GO-2021-0113.
vulncheck-proof: $(GOVULNCHECK_BIN)
	@out=$$(cd testdata/vulnmod && $(GOVULNCHECK_BIN) ./... 2>&1); code=$$?; \
	if [ $$code -ne 3 ]; then echo "$$out"; echo "govulncheck exited $$code on testdata/vulnmod, want 3"; exit 1; fi; \
	if ! echo "$$out" | grep -q 'GO-2021-0113'; then echo "$$out"; echo "govulncheck did not report GO-2021-0113"; exit 1; fi; \
	echo "govulncheck fails on a known vulnerability (GO-2021-0113): OK"

licenses:
	GOFLAGS='-tags=$(TAGS)' $(GO) run $(GOLICENSES) check ./... --allowed_licenses=$(ALLOWED_LICENSES)
