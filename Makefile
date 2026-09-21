BINARY_NAME := gitone
GO := go
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
COMPOSE := docker compose

.PHONY: all build clean test test-short lint lint-fix fmt audit run run-local stop logs smoke

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
	mkdir -p .local
	$(COMPOSE) run --build --rm --no-deps --user "$$(id -u):$$(id -g)" init
	$(COMPOSE) up --build --wait --wait-timeout 180
	@echo "GitOne: https://gitone.localhost:8443 (see deploy/compose/README.md for local TLS trust and demo logins)"

stop:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

smoke:
	$(COMPOSE) run --build --rm --no-deps smoke

run-local:
	$(GO) run ./cmd/gitone
