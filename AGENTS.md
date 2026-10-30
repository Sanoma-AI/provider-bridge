# provider-bridge: notes for coding agents

A Go sidecar that runs OpenTofu providers (plugin protocol 5 and 6) and serves `GetSchema`, `Configure`, `Import`, `Read`, `Plan`, `Apply`, `Close` over ConnectRPC on a unix socket. Read [README.md](README.md) for the API and supply-chain rules; package docs live in each package's `doc.go` or package comment.

## Contents

| Path | What |
| --- | --- |
| `cmd/provider-bridge` | the sidecar: `serve` (default), `fetch`, `schema` |
| `cmd/bridge-record` | records schemas and Import/Read and Plan/Apply replies into `testdata/` (needs credentials; never in CI) |
| `internal/registry` | registry discovery (host policy, https only), download, SHA256SUMS + OpenPGP verification, cache, `CacheDir` |
| `internal/registry/keys` | trusted signing keys, embedded; `README.md` records each key's provenance |
| `internal/plugin` | go-plugin client config (handshake, AutoMTLS, env, stderr text/attributes suppressed; severity and origin into slog) |
| `internal/tfproto` | `Provider` interface; one `client` (`client.go`) over `wire5.go` / `wire6.go`; proto schema → `schema` model; diagnostics |
| `internal/schema` | schema model, JSON document, `ImpliedType` (memoized), `CoerceConfig`, `MapObjects` |
| `internal/values` | JSON ↔ cty ↔ msgpack; unknowns as `null` + paths; `ProposedNew` (OpenTofu's, rewritten) |
| `internal/provider` | `Manager`: per-release locks, resolved releases and the pin check, schema cache (`schemacache.go`, keyed on the bridge build), one configured process per (source, version, operation UUID), up to 64 active/configuring sessions, process-bound plan MACs, Import = ImportResourceState + ReadResource, Read = optional UpgradeResourceState + ReadResource; Plan/Apply (`change.go`) as OpenTofu plans and applies; `RegistryLauncher`, `OverrideLauncher` (`--dev-provider`) |
| `internal/pins` | reads `testdata/pins.json` |
| `internal/server` | Connect handlers, error mapping, unix socket listener |
| `internal/testutil` | golden files, pinned downloads, test provider build, in-process fake provider |
| `proto/` | `bridge/v1/bridge.proto` (ours); `tfplugin5/`, `tfplugin6/` (verbatim OpenTofu copies, see `UPSTREAM`) |
| `gen/` | generated Go, committed; never edit by hand |
| `vendor/` | vendored modules, committed |
| `testprovider/` | separate Go module: Plugin Framework test provider (protocol 6) |
| `testdata/` | `pins.json`, `schemas/`, `replies/`; see [testdata/README.md](testdata/README.md) |

## Rules

- Toolchain and tasks from `.mise.toml`: `mise install` once, then `mise run <task>`; `mise tasks` describes them and `mise run check` is what CI runs (`check-fresh`, `lint`, `test`). Plain `go` commands: `mise x -- go …` (`go` on PATH may be another version).
- Before committing: `mise run lint` and `mise run test` (or `test-short`). Before pushing, on a clean tree: `mise run check` (`check-fresh` rewrites and diffs `vendor/` and `gen/`, so it fails on uncommitted changes there).
- Changed `go.mod`? Run `mise run vendor` and commit `vendor/`. Changed `proto/`? Run `mise run generate` and commit `gen/`.
- `terraform-plugin-go` and `terraform-plugin-framework` must never be imported by the main module (CI guard: `mise run guard`). They belong in `testprovider/` only.
- Never edit `proto/tfplugin5` or `proto/tfplugin6`; replace them from upstream and update `UPSTREAM`.
- Downloaded providers are untrusted data: they live in the cache, never in the repo; only the verified binary is executed, through go-plugin.
- Providers come from registry.opentofu.org only (`--allow-host` adds hosts). A trusted key in `internal/registry/keys` needs a provenance row in its `README.md`; never accept unsigned releases or "pin-only" releases.
- Never commit secrets. Fixtures come from `bridge-record`, which scrubs and refuses to write a file containing a secret; do not hand-edit them to add real values.
- Tests that need the network or a provider process call `testutil.SkipShort`. Golden files: `go test ./internal/schema ./internal/provider -update`.
- Wording: "OpenTofu provider" in prose, `tf` in identifiers, "plugin protocol" for the protocol.
- Commits are signed off (`git commit -s`, DCO).
