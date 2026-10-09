// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package registry downloads OpenTofu provider releases from a provider
// registry (registry.opentofu.org by default) and installs them into a local
// cache after verifying them.
//
// Verification follows OpenTofu's internal/getproviders: the release's
// SHA256SUMS document must carry a valid detached OpenPGP signature from one
// of the signing keys the registry lists for the release, and the downloaded
// zip's sha256 must match the SHA256SUMS line for its filename. Unlike
// OpenTofu, a release without signing keys is refused. A caller may also pin
// the sha256 of the SHA256SUMS document itself; the pin covers every platform
// of the release at once.
//
// Versions are always exact: nothing here resolves ranges or picks "latest".
//
// The cache layout is <dir>/<host>/<namespace>/<type>/<version>/<os>_<arch>/
// holding the provider binary and install.json; installs are serialized per
// release by an flock on <os>_<arch>.lock, so several bridges may share one
// cache. Every Install re-hashes the cached binary against install.json.
package registry
