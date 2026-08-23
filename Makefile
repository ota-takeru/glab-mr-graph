.PHONY: build test run

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o glab-mr-graph ./cmd/glab-mr-graph

test:
	go test ./...

run:
	go run -ldflags "$(LDFLAGS)" ./cmd/glab-mr-graph
