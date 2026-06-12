BINARY := bin/mcpobserve
PKG := github.com/ujai/mcpobserve
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)

# CGO_ENABLED=0 keeps the binary statically linked — "single static binary"
# is a published promise, not an optimization.
GOFLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: build test smoke fmt vet clean

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS) -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Builds the binary and a bundled fake MCP server, pipes a few JSON-RPC
# requests through the proxy, and checks that /metrics reflects them.
smoke: build
	@mkdir -p bin
	go build -o bin/fakeserver ./testdata/fakeserver
	@echo ">> running smoke test"
	@./scripts/smoke.sh

clean:
	rm -rf bin
