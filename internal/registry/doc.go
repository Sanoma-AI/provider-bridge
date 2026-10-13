// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package registry downloads OpenTofu provider releases from a provider
// registry and installs them into a local cache after verifying them.
//
// Only registry.opentofu.org is used unless Client.AllowHosts names another
// host: registries have their own terms of use, and one registry means one
// verification story.
//
// Verification follows OpenTofu's internal/getproviders: the release's
// SHA256SUMS document must carry a valid detached OpenPGP signature from one
// of the signing keys the registry lists for the release or the bridge
// trusts for the provider (keys/, see TrustedKeys), and the downloaded zip's
// sha256 must match the SHA256SUMS line for its filename. Unlike OpenTofu, a
// release without any signing key is refused. A caller may also pin the
// sha256 of the SHA256SUMS document itself; the pin covers every platform of
// the release at once.
//
// Versions are always exact: nothing here resolves ranges or picks "latest".
//
// The cache layout is <dir>/<host>/<namespace>/<type>/<version>/<os>_<arch>/
// holding the provider binary and install.json; installs are serialized per
// release by an flock on <os>_<arch>.lock, so several bridges may share one
// cache. Every Install re-hashes the cached binary against install.json.
package registry
