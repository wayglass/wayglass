BINARY  := wayglass
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all build test lint fmt vet wasm wasm-check worker-test desktop desktop-test desktop-release-windows desktop-release-mac desktop-release-linux docs deploy clean release-dry-run release-sign-mac

all: fmt vet lint test build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/wayglass

release-dry-run:
	VERSION="$(VERSION)" COMMIT="$(COMMIT)" sh scripts/release.sh

release-sign-mac:
	SIGN_MAC=1 VERSION="$(VERSION)" COMMIT="$(COMMIT)" sh scripts/release.sh

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w cmd internal

# The Worker's policy evaluator: internal/policy compiled to a WASI reactor
# (docs/ARCHITECTURE.md D3). The output is committed so Workers Builds and the
# Worker tests need no Go toolchain; wasm-check (run in CI) rebuilds it and
# fails if the committed file differs. Go builds are reproducible across
# hosts, but only for one Go version, so the toolchain is pinned here, and
# VCS stamping is off so a dirty or different checkout yields the same bytes.
WASM_GO  ?= go1.25.8
WASM_OUT := worker/src/wasm/policy.wasm

wasm:
	GOTOOLCHAIN=$(WASM_GO) GOOS=wasip1 GOARCH=wasm \
		go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -buildmode=c-shared -o $(WASM_OUT) ./cmd/policywasm
	@ls -l $(WASM_OUT) | awk '{print "wasm: " $$5 " bytes"}'
	@gzip -9 -c $(WASM_OUT) | wc -c | awk '{print "wasm: " $$1 " bytes gzipped"}'

wasm-check: wasm
	git diff --exit-code -- $(WASM_OUT)

worker-test:
	cd worker && npm ci && npx wrangler types && npx tsc --noEmit && npx vitest run

# The desktop app (desktop/, a nested module so the root stays free of cgo).
# Needs the Wails v2 CLI: go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
WAILS ?= $(shell command -v wails 2>/dev/null || echo $(shell go env GOPATH)/bin/wails)

desktop-test:
	cd desktop && go vet ./... && go test ./...

desktop: desktop-test
	cd desktop && $(WAILS) build -clean
	@echo "desktop: desktop/build/bin/"

# Release builds of the desktop app (docs/RELEASING.md). The macOS one signs
# with the Developer ID in the keychain and notarizes; DRY_RUN=1 prints the
# signing and notarization commands instead.
desktop-release-mac:
	VERSION="$(VERSION)" WAILS="$(WAILS)" sh scripts/desktop-release.sh mac

desktop-release-linux:
	VERSION="$(VERSION)" WAILS="$(WAILS)" sh scripts/desktop-release.sh linux

desktop-release-windows:
	VERSION="$(VERSION)" WAILS="$(WAILS)" sh scripts/desktop-release.sh windows

# The command reference on docs.wayglass.dev, from the CLI's own help text.
# Expects the wayglass/docs repo checked out next to this one.
DOCS_DIR ?= ../docs/src/content/docs/commands
docs: build
	bin/$(BINARY) gen-docs --dir $(DOCS_DIR)

# Deploying costs money and touches shared Cloudflare state. Never automatic.
deploy:
	@echo "Refusing to deploy: ask the maintainer first." >&2
	@echo "Once approved, deploy by hand from worker/." >&2
	@exit 1

clean:
	rm -rf bin dist desktop/build/bin
