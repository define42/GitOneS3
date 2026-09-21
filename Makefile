BINARY_NAME := gitone
GO := go
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build clean test test-short lint lint-fix fmt audit run

all: lint test build

build:
	$(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/gitone

clean:
	rm -rf bin coverage.out coverage.html

test:
	$(GO) test -race -coverprofile=coverage.out ./...

test-short:
	$(GO) test -short ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')
	$(GO) vet ./...

audit:
	govulncheck ./...

run:
	$(GO) run ./cmd/gitone
