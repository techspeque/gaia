# Gaia build and verify targets. `make verify` is the gate every change passes.

GO      ?= go
PKG     := github.com/techspeque/gaia
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
DIST    := dist

# Release targets, ADR-0002 rule 5.
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

.PHONY: all build build-all verify fmt-check vet staticcheck test clean

all: verify

verify: fmt-check vet staticcheck test

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) tool staticcheck ./...

test:
	CGO_ENABLED=0 $(GO) test ./...

build: cgo-guard
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/gaia ./cmd/gaia

# Cross-compile every release target with CGO off, then prove from each
# binary's embedded build settings that CGO was off.
build-all: cgo-guard
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		out=$(DIST)/gaia_$${os}_$${arch}$$ext; \
		echo "build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/gaia; \
		$(GO) version -m $$out | grep -q 'CGO_ENABLED=0' || { echo "$$out was built with CGO enabled"; exit 1; }; \
	done

# Fail when the caller asks for CGO (ADR-0002 rule 1).
.PHONY: cgo-guard
cgo-guard:
	@if [ -n "$(CGO_ENABLED)" ] && [ "$(CGO_ENABLED)" != 0 ]; then \
		echo "CGO_ENABLED=$(CGO_ENABLED): Gaia builds with CGO disabled only (ADR-0002)"; exit 1; fi

clean:
	rm -rf $(DIST)
