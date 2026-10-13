// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider

import (
	"context"
	"log/slog"

	"github.com/Sanoma-AI/provider-bridge/internal/plugin"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
)

// RegistryLauncher resolves releases through a verified registry cache and
// starts them with go-plugin.
type RegistryLauncher struct {
	Cache  *registry.Cache
	Logger *slog.Logger
	// TFLog is passed to providers as TF_LOG when set.
	TFLog string
}

var _ Launcher = (*RegistryLauncher)(nil)

// Resolve installs (when absent) and verifies the release.
func (l *RegistryLauncher) Resolve(ctx context.Context, source, version string) (Release, error) {
	src, err := registry.ParseSource(source)
	if err != nil {
		return Release{}, err
	}
	inst, err := l.Cache.Install(ctx, src, version, "")
	if err != nil {
		return Release{}, err
	}
	return Release{Source: src.String(), Version: version, SHA256: inst.SumsSHA256, Binary: inst.Binary(), Dir: inst.Dir}, nil
}

// Start launches the release's binary.
func (l *RegistryLauncher) Start(_ context.Context, rel Release) (Instance, error) {
	return plugin.Start(plugin.Config{Binary: rel.Binary, Name: rel.Source, Logger: l.Logger, TFLog: l.TFLog})
}

// OverrideLauncher serves the sources in Binaries from local, unverified
// executables (for any version) and every other source through Next. It is
// for tests and provider development only: an override's Release has no
// SHA256 (there is no SHA256SUMS to hash) and no cache Dir.
type OverrideLauncher struct {
	// Binaries maps a source address, in short form, to an executable.
	Binaries map[string]string
	Next     Launcher
}

var _ Launcher = (*OverrideLauncher)(nil)

// Resolve picks the override binary for source, or asks Next.
func (l *OverrideLauncher) Resolve(ctx context.Context, source, version string) (Release, error) {
	if bin, ok := l.Binaries[source]; ok {
		return Release{Source: source, Version: version, Binary: bin}, nil
	}
	return l.Next.Resolve(ctx, source, version)
}

// Start starts rel through Next.
func (l *OverrideLauncher) Start(ctx context.Context, rel Release) (Instance, error) {
	return l.Next.Start(ctx, rel)
}
