// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/Sanoma-AI/provider-bridge/internal/registry"
)

// parseRef reads SOURCE VERSION [SHA256] positional arguments.
func parseRef(fs *flag.FlagSet) (registry.Source, string, string, error) {
	if fs.NArg() < 2 || fs.NArg() > 3 {
		return registry.Source{}, "", "", fmt.Errorf("usage: %s [flags] SOURCE VERSION [SHA256]", fs.Name())
	}
	src, err := registry.ParseSource(fs.Arg(0))
	if err != nil {
		return registry.Source{}, "", "", err
	}
	return src, fs.Arg(1), fs.Arg(2), nil
}

// fetch downloads and verifies a release into the cache and prints its
// install record.
func fetch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var c common
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	src, version, pin, err := parseRef(fs)
	if err != nil {
		return err
	}
	log, err := c.logger(stderr)
	if err != nil {
		return err
	}
	inst, err := c.cache(log).Install(ctx, src, version, pin)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		*registry.Installed
		Binary string `json:"binary"`
	}{inst, inst.Binary()})
}
