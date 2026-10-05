# gharun — build, test and tidy the CLI. Mirrors make.sh / make.ps1.

GO     ?= go
BINARY ?= gharun
PKG    := .

# Version info baked into the binary; make.sh does the same for dist.
LDFLAGS = -X main.Version=$(shell tr -d '[:space:]' < VERSION) -X main.Commit=$(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown) -X main.BuiltAt=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

SOURCES := $(shell find . -name '*.go' -not -path './.git/*')

.DEFAULT_GOAL := build

.PHONY: build
build: $(BINARY)

$(BINARY): $(SOURCES) go.mod $(wildcard go.sum) VERSION
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ $(PKG)

.PHONY: test
test:
	$(GO) test ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: check
check: vet test

# A release zip for GOOS/GOARCH (default: this machine) in dist/ — see
# make.sh, which does the packaging for both.
.PHONY: dist
dist:
	./make.sh dist

.PHONY: clean
clean:
	rm -f $(BINARY)
	rm -rf dist
