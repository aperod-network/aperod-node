BINARY_NODE=aperod-node
BINARY_CLI=aperod
BINARY_INDEXER=aperod-explorer-indexer
BUILD_DIR=build
GO=go
GOFLAGS=-ldflags="-s -w"

.PHONY: all build build-node build-cli build-explorer-indexer test lint fmt clean docker deps

all: deps build test

deps:
	$(GO) mod download
	$(GO) mod tidy

build: build-node build-cli build-explorer-indexer

build-node:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_NODE) ./cmd/node

build-cli:
	mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_CLI) ./cmd/cli

build-explorer-indexer:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_INDEXER) ./cmd/explorer-indexer

test:
	$(GO) test -v -race -count=1 ./...

test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

bench:
	$(GO) test -bench=. -benchmem ./...

fuzz-crypto:
	$(GO) test -fuzz=FuzzRingSign ./crypto/ -fuzztime=60s

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	goimports -w .

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

docker:
	docker build -f deploy/Dockerfile -t aperod-node:latest .

docker-up:
	docker-compose -f deploy/docker-compose.yml up -d

docker-down:
	docker-compose -f deploy/docker-compose.yml down

run-node:
	$(GO) run ./cmd/node --config config/testnet.yaml

run-cli:
	$(GO) run ./cmd/cli
