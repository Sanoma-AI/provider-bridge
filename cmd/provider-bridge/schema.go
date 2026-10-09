// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/Sanoma-AI/provider-bridge/internal/plugin"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
)

// schemaCmd prints a provider's schema document, or with --raw the
// provider's unconverted GetSchema/GetProviderSchema response.
func schemaCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var c common
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs)
	raw := fs.Bool("raw", false, "print the raw plugin protocol response as protojson")
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
	if !*raw {
		launcher := &provider.RegistryLauncher{Cache: c.cache(log), Logger: log, TFLog: c.tfLog}
		res, err := provider.NewManager(launcher, log).Schema(ctx, provider.Ref{Source: src.String(), Version: version, SHA256: pin})
		if err != nil {
			return err
		}
		_, err = stdout.Write(res.JSON)
		return err
	}

	inst, err := c.cache(log).Install(ctx, src, version, pin)
	if err != nil {
		return err
	}
	proc, err := plugin.Start(plugin.Config{Binary: inst.Binary(), Name: src.String(), Logger: log, TFLog: c.tfLog})
	if err != nil {
		return err
	}
	defer proc.Kill()
	msg, err := proc.RawSchema(ctx)
	if err != nil {
		return err
	}
	b, err := protojson.MarshalOptions{Multiline: true}.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", b)
	return err
}
