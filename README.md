# provider-bridge

A Go sidecar that runs existing OpenTofu providers and serves them to the Sanoma engine. It downloads a provider from the registry, verifies it, starts it as a plugin subprocess, speaks the plugin protocol (versions 5 and 6) to it, and exposes `GetSchema`, `Configure`, `Import`, `Read` and `Close` over ConnectRPC on a unix socket. Sanoma can then read and detect drift on vendor resources through maintained providers instead of a hand-written connector per vendor.

GitHub (`integrations/github`, plugin protocol 5) is the first vendor and Stripe (`stripe/stripe`, plugin protocol 6) the second, so both provider build styles (SDKv2 and Plugin Framework) are covered. This version reads only: schema, import and read. Plan and apply come later.

## How it fits

Sanoma keeps a company's operational configuration in typed code and applies it to vendors. Most vendors are covered by first-party connectors in the [`sanoma`](https://github.com/Sanoma-AI/sanoma) monorepo. `provider-bridge` is the second path: Sanoma's worker spawns one bridge per tenant and calls it over a local socket. Each bridged resource type becomes ordinary Sanoma ops (`github.repository.read`, `.import`), and provider schemas map onto the Sanoma resource model:

| Provider schema | Sanoma resource model |
| --- | --- |
| attribute type, description | Zod field, form label |
| `sensitive` | `writeOnly`, masked in UI |
| `computed`, not optional | `vendorOwned`, ignored for drift |
| requires replacement | `immutable`, UI warns before recreate |
| nested block | nested form section |

It lives in its own repo because it uses the Go toolchain and ships binaries.

## Running it

Tools are pinned in `.mise.toml` (Go 1.26, buf, golangci-lint v2):

```sh
mise install
make build                      # bin/provider-bridge, bin/bridge-record
bin/provider-bridge serve --socket /tmp/bridge.sock --cache-dir ~/.cache/provider-bridge
```

`serve` prints one line on stdout once it accepts connections, `{"event":"ready","socket":"/tmp/bridge.sock"}`; every log line is JSON (`slog`) on stderr, including the providers' own logs, re-emitted with `logger: provider.<source>`. Provider configuration is never logged; only its sha256 is. It stops on SIGTERM, SIGINT or EOF on stdin (so it dies with the Node process that spawned it; pass `--watch-stdin=false` to run it detached), kills every provider and removes the socket.

| Flag | Meaning |
| --- | --- |
| `--socket PATH` | unix socket to listen on; created `0600`; a stale socket is replaced |
| `--cache-dir DIR` | verified provider binaries and cached schemas (default `provider-bridge` in the user cache directory: `$XDG_CACHE_HOME` or `~/.cache` on Linux, `~/Library/Caches` on macOS) |
| `--allow-host HOST` | also download providers from this registry host (repeatable); by default only `registry.opentofu.org` is used, see [Supply chain](#supply-chain) |
| `--log-level LEVEL` | `debug`, `info`, `warn`, `error` |
| `--tf-log LEVEL` | `TF_LOG` for provider processes (their logs are off by default) |
| `--dev-provider SOURCE=PATH` | serve SOURCE, any version, from a local unverified binary (repeatable): tests and provider development only. Composed around the registry launcher only when given, so the verified path is untouched; such a provider has no `sha256` (empty in responses), and a request that pins one is refused. |

Two more subcommands help by hand (both take `--cache-dir`, `--allow-host`, `--log-level`): `provider-bridge fetch SOURCE VERSION [SHA256]` downloads and verifies a release and prints its install record; `provider-bridge schema [--raw] [--tf-log LEVEL] SOURCE VERSION [SHA256]` prints the schema document (or the provider's raw protocol response).

### Smoke test with curl

Connect speaks plain HTTP/1.1 + JSON, so curl is enough:

```sh
S=/tmp/bridge.sock
P='"provider":{"source":"hashicorp/null","version":"3.3.2"}'
curl -s --unix-socket $S -H 'Content-Type: application/json' \
  -d "{$P}" http://bridge/sanoma.providerbridge.v1.BridgeService/GetSchema
curl -s --unix-socket $S -H 'Content-Type: application/json' \
  -d "{$P,\"configJson\":\"{}\"}" http://bridge/sanoma.providerbridge.v1.BridgeService/Configure
curl -s --unix-socket $S -H 'Content-Type: application/json' \
  -d "{$P,\"typeName\":\"null_resource\",\"stateJson\":\"{\\\"id\\\":\\\"1\\\"}\"}" \
  http://bridge/sanoma.providerbridge.v1.BridgeService/Read
# {"resource":{"typeName":"null_resource","stateJson":"{\"id\":\"1\",\"triggers\":null}"}}
```

gRPC and the Connect binary protocol work too, over HTTP/2 without TLS.

## The API

[`proto/bridge/v1/bridge.proto`](proto/bridge/v1/bridge.proto), package `sanoma.providerbridge.v1`, service `BridgeService`. Every call names its provider with a `ProviderRef`:

```json
{ "source": "integrations/github", "version": "6.13.0", "sha256": "2d688e83…" }
```

`source` is `namespace/type` (registry.opentofu.org) or `host/namespace/type` (only for a host the bridge runs with `--allow-host`). `version` is exact; ranges are refused. `sha256` is optional: the hex sha256 of the release's `SHA256SUMS` file (one pin for every platform). When set the bridge refuses any other release, on every call; it is echoed by `GetSchema` and `Configure` either way.

| RPC | Request | Response | Notes |
| --- | --- | --- | --- |
| `GetSchema` | `provider` | `schemaJson`, `protocol` (5 or 6), `sha256` | No credentials. Starts an unconfigured provider once, then serves from memory and from a copy cached beside the binary (used only by the same bridge build). |
| `Configure` | `provider`, `configJson` | `warnings`, `sha256` | Starts a long-lived provider (seeded with the known schema, so it is not fetched again) and configures it. Same config again: no-op. Another config for the same (source, version): `FAILED_PRECONDITION` until `Close`. |
| `Import` | `provider`, `typeName`, `id` | `resources[]`, `warnings` | `ImportResourceState`, then `ReadResource` on each result (concurrently, results in the provider's order). A missing object: `NOT_FOUND` (or the provider's own error). |
| `Read` | `provider`, `typeName`, `stateJson`, `private`, `schemaVersion`? | `resource`, `gone`, `warnings` | Refresh from the vendor. With `schemaVersion` (the `Resource.schemaVersion` the state was written with), the provider upgrades the state (`UpgradeResourceState`) first, as OpenTofu does on every refresh; without it, `stateJson` must match the current schema. `gone: true` when the object no longer exists. |
| `Close` | `provider` (unset: all) | `{}` | Stops configured providers. |

A `Resource` is `{ typeName, stateJson, private, schemaVersion }`. `private` is opaque provider data (base64 in JSON): pass it back unchanged on the next `Read`, with `schemaVersion` so state stored before a provider upgrade is upgraded.

Calls for one (source, version) that start or configure a provider wait for each other; calls for different providers never do, and a waiting call gives up when its deadline passes.

**Values are JSON text in string fields** (`configJson`, `stateJson`, `schemaJson`), not `google.protobuf.Struct`, so numbers keep full precision and a null attribute stays distinct from an absent one. State follows the resource type's implied type: every attribute is present (`null` when unset), sets are arrays, nested blocks and nested attributes are arrays (list, set), objects (single, group) or records (map), and a dynamic attribute is `{"value": …, "type": …}`. In requests, omitted attributes are null; unknown values cannot be expressed.

**The schema document** (`formatVersion: 1`, keys sorted):

```jsonc
{
  "source": "integrations/github", "version": "6.13.0", "protocol": 5, "formatVersion": 1,
  "providerConfig": Block, "providerMeta"?: Block,   // provider_meta, for the few providers with one
  "resources":   { "github_repository": { "schemaVersion": 1, "block": Block } },
  "dataSources": { "github_repository": { "schemaVersion": 0, "block": Block } }
}
// Block
{ "attributes": { "<name>": Attribute }, "blocks": { "<name>": NestedBlock },
  "description"?, "descriptionKind"?, "deprecated"? }
// Attribute: `type` is always present (cty JSON type, e.g. "string", ["list","string"]);
// for a nested attribute it is the implied object type, and nestedType carries per-child flags.
{ "type": …, "required"?, "optional"?, "computed"?, "sensitive"?, "writeOnly"?, "deprecated"?,
  "description"?, "descriptionKind"?: "plain" | "markdown",
  "nestedType"?: { "nesting": "single"|"list"|"set"|"map", "attributes": {…}, "minItems"?, "maxItems"? } }
// NestedBlock
{ "nesting": "single"|"group"|"list"|"set"|"map", "block": Block, "minItems"?, "maxItems"? }
```

A list or set block with `maxItems: 1` is SDKv2's way of saying "one object". The document has no `immutable` flag: "requires replacement" is not in the schema (it is decided during plan).

**Errors.** Error diagnostics from the provider become a Connect error whose details are `Diagnostic { severity, summary, detail, attributePath }` messages, one per diagnostic; warnings come back in the response. Codes: `INVALID_ARGUMENT` (bad JSON, unknown resource type, provider rejected the config), `FAILED_PRECONDITION` (not configured, configured differently, pin mismatch, unverifiable release, registry host not allowed, provider error diagnostic), `UNAVAILABLE` (the provider process failed or exited: configure again), `NOT_FOUND` (import found nothing).

## Supply chain

- **Vendored Go modules.** Everything the bridge links is in `vendor/` and built with `-mod=vendor`; CI fails when `vendor/` or `gen/` drift from `go.mod` and `proto/`. `terraform-plugin-go` never enters the bridge's module graph (CI guard; its stubs register the same proto names).
- **One registry.** Providers come from `registry.opentofu.org` only, unless the bridge runs with `--allow-host HOST`. Registries have their own terms of use (registry.terraform.io's restrict it to Terraform), and one registry means one verification story. Every registry URL (discovery, download, `SHA256SUMS`, signature) must be https, and redirects to anything else are refused.
- **Pinned, verified provider binaries.** A release is installed only if its `SHA256SUMS` carries a valid OpenPGP signature from a key the registry lists for that release, or from a key the bridge trusts for that provider, and the zip matches its line in `SHA256SUMS`. Releases no key signs are refused (OpenTofu itself would accept them with a warning). Only the `terraform-provider-<type>` executable is extracted; its sha256 is re-checked against the install record the first time each bridge process uses it. Callers pin a release by the sha256 of its `SHA256SUMS`.
- **Trusted keys.** Some providers sign their releases but registry.opentofu.org lists no key for them (Stripe's `stripe/stripe` 0.3.0). The bridge ships those providers' public keys in [`internal/registry/keys/`](internal/registry/keys/README.md), embedded in the binary, each with its fingerprint and where it was obtained. A trusted key widens who may sign a provider's releases; it never makes an unsigned release acceptable.
- **Never auto-upgraded.** Versions are exact; the bridge never resolves ranges or picks "latest".
- **One bridge per tenant.** Configured providers hold a tenant's credentials, so bridges are not shared. Providers get a minimal environment (`HOME`, `PATH`, proxy and TLS variables, `TF_LOG` when asked): credentials reach them only through `Configure`, never through inherited variables such as `GITHUB_TOKEN`.
- **Process hygiene.** Each provider runs in its own process group with go-plugin's automatic mTLS. See the process model below for what happens when the bridge dies.

### Process model

The bridge starts each provider as a child process and stops it on `Close`, and every provider on shutdown (SIGTERM, SIGINT or EOF on stdin): go-plugin asks it to stop over gRPC, then kills it. A bridge killed with SIGKILL cannot do that, and its providers are orphaned: go-plugin's plugin server (v1.8.0, `server.go`) waits only for its gRPC server to stop, which happens when the client sends the controller's `Shutdown` call, not when the connection drops (the broker's stream ending closes only the broker). Checked on macOS with `hashicorp/null` 3.3.2: after `kill -9` of the bridge the provider kept running under PID 1 until it was sent SIGTERM. Providers run in their own process group, so killing the bridge's group does not reach them either. Stop the bridge with SIGTERM or by closing its stdin; a supervisor that must SIGKILL it should also kill its children. (The kernel's parent-death signal is no help in Go: Linux ties it to the OS thread that forked, not to the process, golang/go#27505.)

## Development

```sh
make test-short        # unit tests, no network
make test              # plus integration: downloads hashicorp/null 3.3.2 into provider-bridge-test in the user cache directory,
                       # builds testprovider/ (a protocol 6 Plugin Framework provider) once into bin/
                       # (PROVIDER_BRIDGE_TESTPROV), runs the server end to end
make lint              # golangci-lint (includes govet)
make generate          # regenerate gen/ from proto/ with buf (protoc plugins are go.mod tools)
make vendor            # go mod tidy + go mod vendor
make check-fresh       # what CI checks: go mod verify, vendor/ and gen/ up to date, no terraform-plugin-go
go test ./internal/schema ./internal/provider -update   # rewrite golden files
```

Layout: `cmd/provider-bridge` (the sidecar), `cmd/bridge-record` (fixture recorder), `internal/registry` (download, verification, trusted keys), `internal/plugin` (go-plugin client), `internal/tfproto` (one plugin protocol client over protocol 5 and 6 wire adapters), `internal/pins` (`testdata/pins.json` reader), `internal/schema` (schema model and document), `internal/values` (JSON, cty, msgpack), `internal/provider` (instance manager), `internal/server` (Connect handlers, socket), `testprovider/` (separate module), `testdata/` (pins, schemas, replies; see [testdata/README.md](testdata/README.md)).

### Recording fixtures

`bridge-record` runs a real provider with real credentials and writes its schema and Import/Read replies into `testdata/`, scrubbed: sensitive attributes and `--scrub TYPE.PATH` become `"<scrubbed>"`, every `--config-env` secret is replaced wherever it appears, and a file in which a secret survives is never written. It never runs in CI.

```sh
GITHUB_TOKEN="$(gh auth token)" go run ./cmd/bridge-record \
  --provider integrations/github --version 6.13.0 \
  --config '{"owner":"Sanoma-AI"}' --config-env token=GITHUB_TOKEN \
  --import github_repository=provider-bridge --import github_branch_protection=provider-bridge:main
STRIPE_API_KEY=sk_test_… go run ./cmd/bridge-record \
  --provider stripe/stripe --version 0.3.0 \
  --config-env api_key=STRIPE_API_KEY --scrub stripe_webhook_endpoint.secret \
  --import stripe_product=prod_… --import stripe_webhook_endpoint=we_…
```

`make record` re-records the committed GitHub fixtures and the Stripe schema.

## License

[Apache-2.0](LICENSE). Third-party material (the OpenTofu plugin protocol definitions, vendored modules) keeps its own license; see [NOTICE](NOTICE).

## Contributing

Contributions are accepted under the [Developer Certificate of Origin](https://developercertificate.org/) (DCO). Sign off each commit with `git commit -s`.
