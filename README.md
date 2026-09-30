# provider-bridge

A Go sidecar that loads Terraform/OpenTofu providers (Okta, Stripe, Google Workspace) over the plugin protocol and serves them to the Sanoma planner. It downloads providers from the OpenTofu registry, completes the go-plugin handshake, and exposes `schema`, `plan`, `apply`, `read` and `import` to the engine, so Sanoma can manage IT tools through existing, maintained providers without writing a connector for each one.

## How it fits

Sanoma keeps a company's operational configuration in typed code and applies it to vendors. Most vendors are covered by first-party connectors in the [`sanoma`](https://github.com/Sanoma-AI/sanoma) monorepo. `provider-bridge` is the second path: it consumes existing Terraform/OpenTofu providers inside the Sanoma engine. Provider schemas are mapped onto the Sanoma resource model:

| Provider schema | Sanoma resource model |
| --- | --- |
| attribute type, description | Zod field, form label |
| `sensitive` | `writeOnly`, masked in UI |
| `computed`, not optional | `vendorOwned`, ignored for drift |
| requires replacement | `immutable`, UI warns before recreate |
| nested block | nested form section |

It lives in its own repo because it uses the Go toolchain and ships binaries. The reverse direction, publishing Sanoma connectors as a provider, is [`terraform-provider-sanoma`](https://github.com/Sanoma-AI/terraform-provider-sanoma).

## Status

Early. No code yet; the module path and layout are not decided.

## License

[Apache-2.0](LICENSE).

## Contributing

Contributions are accepted under the [Developer Certificate of Origin](https://developercertificate.org/) (DCO). Sign off each commit with `git commit -s`.
