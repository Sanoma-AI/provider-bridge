// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Command provider-bridge serves OpenTofu providers to the Sanoma engine over
// ConnectRPC on a unix socket.
//
// Usage:
//
//	provider-bridge [serve] --socket PATH [--cache-dir DIR] [--allow-host HOST] [--log-level LEVEL] [--tf-log LEVEL]
//	provider-bridge fetch  [--cache-dir DIR] [--allow-host HOST] SOURCE VERSION [SHA256]
//	provider-bridge schema [--cache-dir DIR] [--allow-host HOST] [--raw] SOURCE VERSION [SHA256]
//
// serve prints one line, {"event":"ready","socket":"..."}, on stdout once it
// accepts connections; every log line is slog JSON on stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Sanoma-AI/provider-bridge/internal/registry"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "provider-bridge:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "serve":
		return serve(ctx, args, stdin, stdout, stderr)
	case "fetch":
		return fetch(ctx, args, stdout, stderr)
	case "schema":
		return schemaCmd(ctx, args, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q (want serve, fetch or schema)", cmd)
	}
}

// common holds the flags every subcommand shares.
type common struct {
	cacheDir   string
	logLevel   string
	tfLog      string
	allowHosts hostsFlag
}

// register adds the shared flags; withProviders adds the ones that only
// matter to subcommands that start providers (--tf-log).
func (c *common) register(fs *flag.FlagSet, withProviders bool) {
	fs.StringVar(&c.cacheDir, "cache-dir", registry.CacheDir("provider-bridge"), "provider cache directory")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.Var(&c.allowHosts, "allow-host", "HOST: also download providers from this registry host (default: "+registry.DefaultHost+" only; repeatable)")
	if withProviders {
		fs.StringVar(&c.tfLog, "tf-log", "", "TF_LOG level passed to provider processes (e.g. debug); empty disables provider logs")
	}
}

// cache returns the verified provider cache the flags describe.
func (c *common) cache(log *slog.Logger) *registry.Cache {
	return registry.NewCache(c.cacheDir, log, c.allowHosts)
}

// hostsFlag collects repeated --allow-host flags.
type hostsFlag []string

func (h *hostsFlag) String() string { return strings.Join(*h, ",") }

func (h *hostsFlag) Set(v string) error {
	v = strings.ToLower(v)
	if err := registry.CheckHost(v); err != nil {
		return err
	}
	*h = append(*h, v)
	return nil
}

func (c *common) logger(stderr io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	return slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl})), nil
}
