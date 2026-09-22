BINARY_NAME := gitone
GO := go
NPM := npm
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
COMPOSE := docker compose

.PHONY: all build ui ui-check test-ui clean test test-short lint lint-fix fmt audit run run-local stop logs smoke

all: lint test build

build: ui
	$(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/gitone

ui:
	$(NPM) --prefix web ci
	$(NPM) --prefix web run build
	rm -rf -- internal/webui/dist/assets
	cp -R web/dist/. internal/webui/dist/

ui-check:
	$(NPM) --prefix web ci
	$(NPM) --prefix web run build

test-ui:
	$(NPM) --prefix web run test:e2e

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

run-local: ui
	$(GO) run ./cmd/gitone
