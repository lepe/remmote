GO ?= go
BIN := bin

.PHONY: all build build-webp test integration integration-shutdown lint vendor clean

all: build-webp

## build: pure-Go binaries (no CGo; jpeg/zraw/hybrid codecs, optional -tags webp)
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/ ./cmd/...

## build-webp: binaries with the optional lossy WebP encoder (requires libwebp-dev)
build-webp:
	CGO_ENABLED=1 $(GO) build -tags webp -o $(BIN)/ ./cmd/...

test:
	CGO_ENABLED=0 $(GO) test ./...

integration: build
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
