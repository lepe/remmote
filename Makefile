GO ?= go
BIN := bin

.PHONY: all build build-hub build-webp test integration integration-shutdown lint vendor clean

all: build-webp build-hub

## build: pure-Go binaries (no CGo; jpeg/zraw/hybrid codecs, optional -tags webp)
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/ ./cmd/...

## build-hub: the connection manager — the one binary that needs a webview
## (Wails on webkit2gtk). Skipped politely when the headers are missing:
##   Debian/Ubuntu: libgtk-3-dev libwebkit2gtk-4.1-dev
##
## Every standard pkgconfig directory is put in front of pkg-config: on
## multiarch systems the pkg-config binary is sometimes built for another
## architecture (an i386 one on an amd64 box, say) and never looks in
## /usr/lib/<triplet>/pkgconfig — hiding perfectly installed libraries
## from both this check and cgo's own lookups.
PKGCONFIG_DIRS := $(shell for d in /usr/lib/*/pkgconfig /usr/lib/pkgconfig /usr/lib64/pkgconfig /usr/share/pkgconfig /usr/local/lib/pkgconfig; do [ -d "$$d" ] && printf '%s:' "$$d"; done)

build-hub:
	@export PKG_CONFIG_PATH="$(PKGCONFIG_DIRS)$${PKG_CONFIG_PATH}"; \
	pkg=""; tags=""; \
	if command -v pkg-config >/dev/null 2>&1; then \
		if pkg-config --exists webkit2gtk-4.1 2>/dev/null; then pkg=webkit2gtk-4.1; tags=hub,desktop,production,webkit2_41; \
		elif pkg-config --exists webkit2gtk-4.0 2>/dev/null; then pkg=webkit2gtk-4.0; tags=hub,desktop,production; fi; \
	fi; \
	if [ -z "$$pkg" ]; then \
		echo "skip: remmote-hub needs the webkit2gtk development files."; \
		echo "      Debian/Ubuntu: apt install libgtk-3-dev libwebkit2gtk-4.1-dev"; \
		echo "      pkg-config sees: $$(pkg-config --list-all 2>/dev/null | grep -iE 'webkit|gtk\+-3' | awk '{print $$1}' | tr '\n' ' ')"; \
		exit 0; \
	fi; \
	echo "building remmote-hub against $$pkg"; \
	CGO_ENABLED=1 $(GO) build -tags "$$tags" -o $(BIN)/remmote-hub ./cmd/remmote-hub

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
