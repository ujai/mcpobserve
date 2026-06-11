BINARY := bin/mcpobserve
PKG := github.com/ujai/mcpobserve

.PHONY: build test smoke fmt vet clean

build:
	@mkdir -p bin
	go build -o $(BINARY) .

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
