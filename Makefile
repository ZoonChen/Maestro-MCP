.PHONY: build test lint clean docker-build

BINARY=maestro
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags="-s -w -X main.Version=$(VERSION) -X github.com/anthropics/maestro-mcp/internal/mcp.ServerVersion=$(VERSION)"

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/maestro

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -f $(BINARY) $(BINARY).exe

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t maestro-mcp:$(VERSION) .
