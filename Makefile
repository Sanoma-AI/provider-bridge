# Tools come from .mise.toml; prefer `mise run <target>` (`mise tasks` describes
# each). With mise installed each tool runs through `mise x --`, so a bare
# `make <target>` also uses the pinned Go; `make MISE=` uses PATH.
# The main module builds from vendor/ (Go selects -mod=vendor automatically).

MISE     ?= $(shell command -v mise 2>/dev/null)
TOOL     := $(if $(MISE),$(MISE) x --)
GO       ?= $(TOOL) go
BUF      ?= $(TOOL) buf
# Not LINT: make 3.81 (macOS) predefines LINT = lint, so ?= would never apply.
GOLANGCI ?= $(TOOL) golangci-lint
GORELEASER ?= $(TOOL) goreleaser
BINDIR   ?= bin
# The protocol 6 test provider, built once and handed to the tests.
TESTPROV ?= $(abspath $(BINDIR))/terraform-provider-testprov

.PHONY: all build generate vendor lint testprov test test-short test-integration record check-fresh guard snapshot clean

all: build

build:
	$(GO) build -o $(BINDIR)/ ./cmd/...

# protoc plugins are go.mod tools.
generate:
	rm -rf gen
	$(BUF) lint
	$(BUF) generate --template buf.gen.tfplugin.yaml
	$(BUF) generate --template buf.gen.bridge.yaml

vendor:
	$(GO) mod tidy
	$(GO) mod vendor

lint:
	$(GOLANGCI) run

# Separate module, not vendored.
testprov:
	cd testprovider && GOFLAGS= GOWORK=off $(GO) build -mod=readonly -o $(TESTPROV) .

# Downloads hashicorp/null into the user cache directory.
test: testprov
	PROVIDER_BRIDGE_TESTPROV=$(TESTPROV) $(GO) test -race ./...

test-short:
	$(GO) test -race -short ./...

test-integration: testprov
	PROVIDER_BRIDGE_TESTPROV=$(TESTPROV) $(GO) test -race -run 'Integration|E2E' ./...

# Needs `gh auth login`; never runs in CI.
record:
	GITHUB_TOKEN="$$(gh auth token)" $(GO) run ./cmd/bridge-record \
	  --provider integrations/github --version 6.13.0 \
	  --config '{"owner":"Sanoma-AI"}' --config-env token=GITHUB_TOKEN \
	  --import github_repository=provider-bridge \
	  --import github_repository=sanoma \
	  --import github_branch_protection=provider-bridge:main
	$(GO) run ./cmd/bridge-record --provider stripe/stripe --version 0.3.0 --schema-only
	$(GO) run ./cmd/bridge-record --provider supabase/supabase --version 1.11.0 --schema-only
	$(GO) run ./cmd/bridge-record --provider kreuzwerker/docker --version 4.6.0 --schema-only
	$(GO) run ./cmd/bridge-record --provider hashicorp/null --version 3.3.2 \
	  --change 'null_resource=example={"triggers":{"v":"1"}}' \
	  --change 'null_resource=example={"triggers":{"v":"1"}}' \
	  --change 'null_resource=example={"triggers":{"v":"2"}}'

check-fresh: guard
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) mod vendor
	git diff --exit-code -- vendor
	test -z "$$(git status --porcelain -- vendor)"
	$(MAKE) generate
	git diff --exit-code -- gen
	test -z "$$(git status --porcelain -- gen)"

guard:
	@if $(GO) list -deps ./cmd/provider-bridge | grep -q terraform-plugin-go; then \
	  echo "cmd/provider-bridge depends on terraform-plugin-go" >&2; exit 1; fi

# What a v* tag publishes (.goreleaser.yaml), built into dist/ without publishing.
snapshot:
	$(GORELEASER) release --snapshot --clean

clean:
	rm -rf $(BINDIR) dist
