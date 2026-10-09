// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
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
	// Dev maps a source address (short form) to a local provider binary
	// used for any version, unverified: for tests and provider development
	// only. Its SHA256 is the binary's own hash.
	Dev map[string]string
}

var _ Launcher = (*RegistryLauncher)(nil)

// Resolve installs and verifies the release, or picks the dev binary.
func (l *RegistryLauncher) Resolve(ctx context.Context, ref Ref) (Release, error) {
	src, err := registry.ParseSource(ref.Source)
	if err != nil {
		return Release{}, err
	}
	if bin, ok := l.Dev[src.String()]; ok {
		sum, err := registry.FileSHA256(bin)
		if err != nil {
			return Release{}, fmt.Errorf("dev provider %s: %w", src, err)
		}
		return Release{Source: src.String(), Version: ref.Version, SHA256: sum, Binary: bin}, nil
	}
	inst, err := l.Cache.Install(ctx, src, ref.Version, ref.SHA256)
	if err != nil {
		return Release{}, err
	}
	return Release{Source: src.String(), Version: ref.Version, SHA256: inst.SumsSHA256, Binary: inst.Binary(), Dir: inst.Dir}, nil
}

// Start launches the release's binary.
func (l *RegistryLauncher) Start(_ context.Context, rel Release) (Instance, error) {
	return plugin.Start(plugin.Config{Binary: rel.Binary, Name: rel.Source, Logger: l.Logger, TFLog: l.TFLog})
}
