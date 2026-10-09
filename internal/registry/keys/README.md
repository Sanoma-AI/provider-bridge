# Trusted signing keys

OpenPGP public keys the bridge trusts for a provider's releases in addition to the keys the registry lists (`internal/registry/keys.go` maps each provider to its files). A release is still installed only if its `SHA256SUMS` carries a valid signature from one of the keys; a key here never makes an unsigned release acceptable.

Add a key only when the provider's own release pipeline signs with it and the registry the bridge uses does not list it. Record where it was obtained and how it was checked.

| File | Provider | Fingerprint | User ID | Obtained | Date |
| --- | --- | --- | --- | --- | --- |
| `stripe_stripe.asc` | `stripe/stripe` | `D50A 40BB C524 CBE0 0E7E CC6F A6C8 7736 961A 5EF6` | Michael Selander <selander@stripe.com> | registry.terraform.io's download listing for `stripe/stripe` 0.3.0 (`signing_keys.gpg_public_keys`); registry.opentofu.org lists none for that release. Also verifiable against the release signatures at https://github.com/stripe/terraform-provider-stripe. Checked: `gpg --verify` gives a good signature over the 0.3.0 `SHA256SUMS` that registry.opentofu.org points at (sha256 `2e3ae569e1bee7c64ae7f10e6ceaaf5bfab4db7af38979bab28f9b7a35a260c5`). | 2026-10-09 |
