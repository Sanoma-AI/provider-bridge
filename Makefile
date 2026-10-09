# Tools come from .mise.toml; run `mise install` once, then `make <target>`.
# The main module builds from vendor/ (Go selects -mod=vendor automatically).

GO       ?= go
BUF      ?= buf
LINT     ?= golangci-lint
BINDIR   ?= bin
# The protocol 6 test provider, built once and handed to the tests.
TESTPROV ?= $(abspath $(BINDIR))/terraform-provider-testprov

.PHONY: all build generate vendor lint testprov test test-short test-integration record check-fresh guard clean

all: build

build:
	$(GO) build -o $(BINDIR)/ ./cmd/...

## generate: regenerate gen/ from proto/ (protoc plugins are go.mod tools)
generate:
	rm -rf gen
	$(BUF) lint
	$(BUF) generate --template buf.gen.tfplugin.yaml
	$(BUF) generate --template buf.gen.bridge.yaml

## vendor: tidy and re-vendor the main module
vendor:
	$(GO) mod tidy
	$(GO) mod vendor

## lint: golangci-lint (its standard linters include govet)
lint:
	$(LINT) run

## testprov: build the protocol 6 test provider (separate module, not vendored)
testprov:
	cd testprovider && GOFLAGS= GOWORK=off $(GO) build -mod=readonly -o $(TESTPROV) .

## test: everything, including network tests (downloads hashicorp/null)
test: testprov
	PROVIDER_BRIDGE_TESTPROV=$(TESTPROV) $(GO) test -race ./...

## test-short: unit tests only, no network
test-short:
	$(GO) test -race -short ./...

## test-integration: the provider-process tests only
test-integration: testprov
	PROVIDER_BRIDGE_TESTPROV=$(TESTPROV) $(GO) test -race -run 'Integration|E2E' ./...

## record: re-record GitHub fixtures (needs `gh auth login`); never runs in CI
record:
	GITHUB_TOKEN="$$(gh auth token)" $(GO) run ./cmd/bridge-record \
	  --provider integrations/github --version 6.13.0 \
	  --config '{"owner":"Sanoma-AI"}' --config-env token=GITHUB_TOKEN \
	  --import github_repository=provider-bridge \
	  --import github_repository=sanoma \
	  --import github_branch_protection=provider-bridge:main
	$(GO) run ./cmd/bridge-record --provider stripe/stripe --version 0.3.0 --schema-only

## check-fresh: fail when vendor/ or gen/ differ from what go.mod and proto/ produce
check-fresh: guard
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) mod vendor
	git diff --exit-code -- vendor
	test -z "$$(git status --porcelain -- vendor)"
	$(MAKE) generate
	git diff --exit-code -- gen
	test -z "$$(git status --porcelain -- gen)"

## guard: the shipped binary must never link terraform-plugin-go
guard:
	@if $(GO) list -deps ./cmd/provider-bridge | grep -q terraform-plugin-go; then \
	  echo "cmd/provider-bridge depends on terraform-plugin-go" >&2; exit 1; fi

clean:
	rm -rf $(BINDIR)
