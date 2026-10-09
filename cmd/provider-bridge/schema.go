// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/internal/plugin"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
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
	launcher := &provider.RegistryLauncher{
		Cache:  c.cache(log),
		Logger: log,
		TFLog:  c.tfLog,
	}
	ref := provider.Ref{Source: src.String(), Version: version, SHA256: pin}
	if !*raw {
		res, err := provider.NewManager(launcher, log).Schema(ctx, ref)
		if err != nil {
			return err
		}
		_, err = stdout.Write(res.JSON)
		return err
	}

	rel, err := launcher.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	inst, err := launcher.Start(ctx, rel)
	if err != nil {
		return err
	}
	defer inst.Kill()
	var msg proto.Message
	switch p := inst.(*plugin.Process).Provider.(type) {
	case *tfproto.Client5:
		msg, err = p.RawSchema(ctx)
	case *tfproto.Client6:
		msg, err = p.RawSchema(ctx)
	default:
		err = fmt.Errorf("unexpected client %T", p)
	}
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
