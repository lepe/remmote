GO ?= go
BIN := bin

.PHONY: all build build-hub build-webp test integration integration-shutdown lint vendor clean

all: build-webp build-hub

## build: pure-Go binaries (no CGo; jpeg/zraw/hybrid codecs, optional -tags webp)
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/ ./cmd/...

## build-hub: the connection manager — the one binary that needs a webview
## (Wails on webkit2gtk). Skipped when the headers are not installed:
##   Debian/Ubuntu: libgtk-3-dev libwebkit2gtk-4.1-dev
build-hub:
	@if pkg-config --exists webkit2gtk-4.1 2>/dev/null; then \
		CGO_ENABLED=1 $(GO) build -tags "hub,webkit2_41" -o $(BIN)/remmote-hub ./cmd/remmote-hub; \
	elif pkg-config --exists webkit2gtk-4.0 2>/dev/null; then \
		CGO_ENABLED=1 $(GO) build -tags hub -o $(BIN)/remmote-hub ./cmd/remmote-hub; \
	else \
		echo "skip: remmote-hub needs the webview headers (Debian: libgtk-3-dev libwebkit2gtk-4.1-dev)"; \
	fi

## build-webp: binaries with the optional lossy WebP encoder (requires libwebp-dev)
build-webp:
	CGO_ENABLED=1 $(GO) build -tags webp -o $(BIN)/ ./cmd/...

test:
	CGO_ENABLED=0 $(GO) test ./...

integration: build
	./scripts/integration-control.sh
	./scripts/integration-session.sh
	./scripts/integration-auth.sh
	./scripts/integration.sh
	./scripts/integration.sh jpeg
	./scripts/integration-delta.sh
	./scripts/integration-delta.sh hybrid 2 2
	./scripts/integration-exec.sh
	./scripts/integration-maximize.sh
	./scripts/integration-resize.sh
	./scripts/integration-tls.sh

integration-shutdown: build
	python3 scripts/integration-shutdown.py

lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || { echo 'gofmt needed on:'; gofmt -l .; exit 1; }

vendor:
	$(GO) mod vendor

clean:
	rm -rf $(BIN)
