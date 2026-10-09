# testdata

Fixtures recorded from real OpenTofu providers. They let the bridge's tests, and the Sanoma TypeScript client's tests, run without credentials or network. `cmd/bridge-record`'s `TestReplayFixtures` parses every recorded state against its schema's implied type and checks that no sensitive value escaped scrubbing; `TestSchemaFixtures` checks every schema document is canonical.

## Files

### `pins.json`

Provider releases tests and the recorder may download, each pinned by the sha256 of its `SHA256SUMS` file (one pin for all platforms):

| Source | Version | Protocol | Use |
| --- | --- | --- | --- |
| `hashicorp/null` | 3.3.2 | 5 | integration tests (no credentials) |
| `integrations/github` | 6.13.0 | 5 (SDKv2) | recorded fixtures |
| `stripe/stripe` | 0.3.0 | 6 (Plugin Framework) | recorded schema |

Every provider comes from `registry.opentofu.org`. It lists no signing key for `stripe/stripe` 0.3.0, so the bridge verifies that release's `SHA256SUMS` (the GitHub release's own file, which the registry points at) against Stripe's key `A6C87736961A5EF6`, shipped in [`internal/registry/keys/`](../internal/registry/keys/README.md) with its provenance; the bridge still refuses a release no key signs.

### `schemas/`

| File | Recorded with | Notes |
| --- | --- | --- |
| `hashicorp_null_3.3.2.json` | no credentials | golden of `TestIntegrationNull`; rewrite with `go test ./internal/provider -update` |
| `sanoma_testprov_0.0.1.json` | no credentials | golden of the in-tree protocol 6 test provider (`testprovider/`) |
| `integrations_github_6.13.0.json` | no credentials (`GetSchema` needs no `Configure`) | 88 resource types, 75 data sources, 356 KB |
| `stripe_stripe_0.3.0.json` | no credentials | 49 resource types, no data sources, 2.3 MB |

### `replies/`

`replies/<ns>_<type>_<version>/<resource type>/<import id>/{import,read}.json`. Each file is `{ provider, request, response, scrubbed }`: `request` and `response` are the bridge API messages (`ImportRequest`/`ImportResponse`, `ReadRequest`/`ReadResponse`) in Connect's JSON form, so a fake bridge can return them verbatim; a failed call has `response: { "error": { code, message, diagnostics } }`. `read.json` reads the state the import returned (what a drift check does). `scrubbed` lists the attribute paths replaced by `"<scrubbed>"` and the environment variables whose values were replaced everywhere.

| Fixture | Recorded with | Outcome |
| --- | --- | --- |
| `integrations_github_6.13.0/github_repository/provider-bridge/` | Leon's `gh auth token` as `GITHUB_TOKEN`, config `{"owner":"Sanoma-AI"}`, 2026-10-09 | import + read OK |
| `integrations_github_6.13.0/github_repository/sanoma/` | same | import + read OK |
| `integrations_github_6.13.0/github_branch_protection/provider-bridge_main/` | same | `import.json` only: `failed_precondition`, "could not find a branch protection rule with the pattern 'main'" (the branch is unprotected) |

Not recorded:

- **`github_team_membership`**: the `Sanoma-AI` org has no teams, and none was created for this.
- **Stripe imports and reads**: no Stripe key on the recording machine. Re-record with `STRIPE_API_KEY` (see the README's recorder example).

## Findings

- **Stripe import by ID works** for both `stripe_product` (whose docs have no Import section) and `stripe_webhook_endpoint`: a probe with a fake test key (not committed) got through `ImportResourceState` (passthrough of the ID) and failed only in the following `ReadResource` with Stripe's 401 `invalid_request_error`. The bridge reports that as `failed_precondition` with the provider's diagnostic; Stripe masks the key in the message.
- **A missing GitHub object fails inside `ImportResourceState`** (error diagnostic, `failed_precondition`), not as an empty import or a null read, so clients cannot rely on `not_found` for "does not exist" with this provider; they must read the diagnostic.
- **`github_repository.etag` is `computed` and `optional`**, so the "computed, not optional → vendorOwned" rule does not exclude it from drift, yet it changes whenever the repository changes. Wave 2 needs an overlay for it (and checks the other computed+optional attributes: `allow_forking`, `default_branch`, `fork`, `id`, `private`, `source_owner`, `source_repo`, `topics`, `visibility`, `vulnerability_alerts`, `web_commit_signoff_required`). Two successive reads returned identical state, `etag` included.
- **SDKv2 quirks in GitHub's state**: `fork` is a string (`"false"`); single objects are lists with `maxItems: 1` (`pages`, `security_and_analysis`, `template`); no nested attribute types at all (protocol 5).
- **Stripe's schema is large and deep**: 2.3 MB, of which about 450 KB is descriptions; 4,949 attributes, 1,135 nested attribute types, nesting five levels deep; the biggest type, `stripe_payment_intent`, is 210 KB on its own. Codegen should be per resource type and lazy.
- **Stripe has 7 `DynamicPseudoType` attributes**, all card or payment-method details (`stripe_charge.payment_method_details`, `.card`, `.card.wallet`, `stripe_payment_method.card`, `.card.wallet`, `stripe_source.card`, `stripe_source.three_d_secure`). Their state is `{"value": …, "type": …}`; none is on `stripe_product` or `stripe_webhook_endpoint`.
- **Stripe uses write-only attributes** (369, across 20 resource types: charge, customer, invoice, price, subscription and others). They are always null in state. The bridge sends `write_only_attributes_allowed: false`, which matters only once plan/apply exist.
- **Stripe marks 10 attributes sensitive**, including `stripe_webhook_endpoint.secret` ("only returned at creation"), so the scrubber covers it without `--scrub`.
