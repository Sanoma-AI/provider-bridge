// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
	"github.com/Sanoma-AI/provider-bridge/internal/server"
)

// devFlag collects repeated --dev-provider source=path flags.
type devFlag map[string]string

func (d devFlag) String() string { return fmt.Sprint(map[string]string(d)) }

func (d devFlag) Set(v string) error {
	source, path, ok := strings.Cut(v, "=")
	if !ok || path == "" {
		return errors.New("want SOURCE=PATH")
	}
	src, err := registry.ParseSource(source)
	if err != nil {
		return err
	}
	d[src.String()] = path
	return nil
}

// serve runs the bridge until SIGINT/SIGTERM or, with --watch-stdin, until
// stdin reaches EOF (the parent process died or closed the pipe).
func serve(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var c common
	dev := devFlag{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c.register(fs)
	socket := fs.String("socket", "", "unix socket path to listen on (required)")
	watchStdin := fs.Bool("watch-stdin", true, "exit when stdin reaches EOF, so the bridge dies with its parent")
	fs.Var(dev, "dev-provider", "SOURCE=PATH: serve SOURCE from an unverified local binary (tests and provider development only; repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || fs.NArg() > 0 {
		return errors.New("usage: provider-bridge serve --socket PATH [flags]")
	}
	log, err := c.logger(stderr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *watchStdin {
		go func() {
			_, _ = io.Copy(io.Discard, stdin)
			log.Info("stdin closed; shutting down")
			cancel()
		}()
	}

	var launcher provider.Launcher = &provider.RegistryLauncher{Cache: c.cache(log), Logger: log, TFLog: c.tfLog}
	if len(dev) > 0 {
		for source, path := range dev {
			log.Warn("dev provider override: binary is not verified", "provider", source, "binary", path)
		}
		launcher = &provider.OverrideLauncher{Binaries: dev, Next: launcher}
	}
	m := provider.NewManager(launcher, log)
	defer m.Shutdown()

	ln, err := server.Listen(*socket)
	if err != nil {
		return err
	}
	ready, _ := json.Marshal(map[string]string{"event": "ready", "socket": *socket})
	if _, err := fmt.Fprintf(stdout, "%s\n", ready); err != nil {
		_ = ln.Close()
		return err
	}
	log.Info("serving", "socket", *socket, "cache_dir", c.cacheDir)
	err = server.Serve(ctx, ln, server.NewHandler(m, log), 5*time.Second)
	log.Info("stopped")
	return err
}
