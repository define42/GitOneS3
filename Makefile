BINARY_NAME := gitone
GO := go
NPM := npm
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
COMPOSE := docker compose
QUALIFICATION_OUTPUT ?= $(CURDIR)/tmp/qualification

.PHONY: all build build-s3check ui ui-check test-ui clean test test-integration qualify-s3 test-short lint lint-fix fmt audit run run-local stop logs smoke smoke-git

all: lint test build

build: ui
	$(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/gitone

build-s3check:
	$(GO) build -o bin/gitone-s3check ./cmd/gitone-s3check

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

# Race instrumentation can exhaust the real 90-second transfer budget for the
# two 80 MiB native regressions. Run those separately without instrumentation.
test-integration:
	$(GO) test -race -tags=integration -shuffle=on -count=1 -timeout=10m -skip '^(TestNativeGitLargeStreamingLifecycle|TestNativeSSHLargeShardPush)$$' ./...
	$(GO) test -tags=integration -shuffle=on -count=1 -timeout=10m -p=1 -run '^(TestNativeGitLargeStreamingLifecycle|TestNativeSSHLargeShardPush)$$' ./internal/gittransport ./internal/sshserver

# Creates only a fresh random test bucket; requires bucket create/delete rights.
# Preserve JSON measurements and storage metrics outside the source tree.
qualify-s3:
	mkdir -p "$(QUALIFICATION_OUTPUT)"
	GITONE_QUALIFY_S3=1 $(GO) test -tags=integration ./internal/gittransport -run '^TestS3GitQualification$$' -count=1 -v -timeout=45m -artifacts -outputdir="$(QUALIFICATION_OUTPUT)"

test-short:
	$(GO) test -short ./...

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --build-tags integration ./...


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

smoke-git:
	python3 deploy/compose/git_smoke.py

run-local: ui
	$(GO) run ./cmd/gitone
