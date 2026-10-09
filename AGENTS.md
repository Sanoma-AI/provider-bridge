# provider-bridge: notes for coding agents

A Go sidecar that runs OpenTofu providers (plugin protocol 5 and 6) and serves `GetSchema`, `Configure`, `Import`, `Read`, `Close` over ConnectRPC on a unix socket. Read [README.md](README.md) for the API and supply-chain rules; package docs live in each package's `doc.go` or package comment.

## Contents

| Path | What |
| --- | --- |
| `cmd/provider-bridge` | the sidecar: `serve` (default), `fetch`, `schema` |
| `cmd/bridge-record` | records schemas and Import/Read replies into `testdata/` (needs credentials; never in CI) |
| `internal/registry` | registry discovery, download, SHA256SUMS + OpenPGP verification, cache |
| `internal/plugin` | go-plugin client config (handshake, AutoMTLS, env, stderr re-logging) |
| `internal/tfproto` | `Provider` interface; `Client5`, `Client6`; proto schema → `schema` model; diagnostics |
| `internal/schema` | schema model, JSON document, `ImpliedType`, `CoerceConfig` |
| `internal/values` | JSON ↔ cty ↔ msgpack |
| `internal/provider` | `Manager`: schema cache, one configured instance per (source, version), Import = ImportResourceState + ReadResource |
| `internal/server` | Connect handlers, error mapping, unix socket listener |
| `internal/testutil` | golden files, pinned downloads, test provider build, in-process fake provider |
| `proto/` | `bridge/v1/bridge.proto` (ours); `tfplugin5/`, `tfplugin6/` (verbatim OpenTofu copies, see `UPSTREAM`) |
| `gen/` | generated Go, committed; never edit by hand |
| `vendor/` | vendored modules, committed |
| `testprovider/` | separate Go module: Plugin Framework test provider (protocol 6) |
| `testdata/` | `pins.json`, `schemas/`, `replies/`; see [testdata/README.md](testdata/README.md) |

## Rules

- Toolchain from `.mise.toml` (`mise install`; prefix commands with `mise x --` if PATH lacks them).
- Before committing: `go vet ./...`, `golangci-lint run`, `go test -race ./...`, `go test -race -short ./...`, `make check-fresh`.
- Changed `go.mod`? Run `make vendor` and commit `vendor/`. Changed `proto/`? Run `make generate` and commit `gen/`.
- `terraform-plugin-go` and `terraform-plugin-framework` must never be imported by the main module (CI guard: `make guard`). They belong in `testprovider/` only.
- Never edit `proto/tfplugin5` or `proto/tfplugin6`; replace them from upstream and update `UPSTREAM`.
- Downloaded providers are untrusted data: they live in the cache, never in the repo; only the verified binary is executed, through go-plugin.
- Never commit secrets. Fixtures come from `bridge-record`, which scrubs and refuses to write a file containing a secret; do not hand-edit them to add real values.
- Tests that need the network or a provider process call `testutil.SkipShort`. Golden files: `go test ./internal/schema ./internal/provider -update`.
- Wording: "OpenTofu provider" in prose, `tf` in identifiers, "plugin protocol" for the protocol.
- Commits are signed off (`git commit -s`, DCO).
