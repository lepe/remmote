GO ?= go
BIN := bin

.PHONY: all build build-webp test integration lint vendor clean

all: build

## build: pure-Go binaries (no CGo, JPEG codec only)
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/ ./cmd/...

## build-webp: binaries with the optional lossy WebP encoder (requires libwebp-dev)
build-webp:
	CGO_ENABLED=1 $(GO) build -tags webp -o $(BIN)/ ./cmd/...

test:
	CGO_ENABLED=0 $(GO) test ./...

integration: build
	./scripts/integration.sh

lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || { echo 'gofmt needed on:'; gofmt -l .; exit 1; }

vendor:
	$(GO) mod vendor

clean:
	rm -rf $(BIN)
