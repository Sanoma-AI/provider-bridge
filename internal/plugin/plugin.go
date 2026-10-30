// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package plugin starts an OpenTofu provider binary as a go-plugin
// subprocess and completes the plugin protocol handshake, the way OpenTofu
// core does: magic cookie, protocol versions 5 and 6, gRPC only, AutoMTLS.
//
// The provider gets a minimal environment (see Env): credentials reach it
// only through Configure, never through inherited variables such as
// GITHUB_TOKEN. Its process runs in its own process group. Clients are not
// go-plugin "managed": the bridge's Manager kills every provider it started
// on Close and Shutdown. Its stderr content is suppressed; only severity and origin are emitted
// through the bridge's slog logger.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
)

// Handshake is the plugin protocol handshake every OpenTofu and Terraform
// provider expects.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  4,
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

// maxMsgSize bounds gRPC messages from providers; large providers' schemas
// exceed gRPC's 4 MiB default.
const maxMsgSize = 256 << 20

// pluginName is the name providers register their plugin under.
const pluginName = "provider"

// grpcPlugin dispenses a tfproto.Provider for one plugin protocol version.
type grpcPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	client func(*grpc.ClientConn) tfproto.Provider
}

func (grpcPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return errors.New("provider-bridge is a plugin client only")
}

func (p grpcPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	return p.client(conn), nil
}

var plugins = map[int]goplugin.PluginSet{
	5: {pluginName: grpcPlugin{client: func(conn *grpc.ClientConn) tfproto.Provider {
		return tfproto.NewClient5(tfplugin5.NewProviderClient(conn))
	}}},
	6: {pluginName: grpcPlugin{client: func(conn *grpc.ClientConn) tfproto.Provider {
		return tfproto.NewClient6(tfplugin6.NewProviderClient(conn))
	}}},
}

// Config describes one provider process.
type Config struct {
	// Binary is the provider executable (verified by the caller).
	Binary string
	// Name labels log lines, e.g. "integrations/github".
	Name string
	// Logger receives severity-only provider log events as provider.<Name>.
	Logger *slog.Logger
	// TFLog, when non-empty, is passed to the provider as TF_LOG.
	TFLog string
}

// Process is a running, handshaken provider.
type Process struct {
	tfproto.Provider
	client *goplugin.Client
}

// Start launches the provider and dispenses its client for the negotiated
// protocol. The caller must Kill the process.
func Start(cfg Config) (*Process, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	cmd := exec.Command(cfg.Binary)
	cmd.Env = Env(cfg.TFLog)
	// Own process group: a terminal's SIGINT to the bridge's group does not
	// reach providers; the bridge stops them itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// go-plugin parses the provider's stderr into this logger; its output
	// is discarded and the sink emits only severity and origin through slog.
	hl := hclog.NewInterceptLogger(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Trace})
	hl.RegisterSink(slogSink{log: log.With("logger", "provider."+cfg.Name)})
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		VersionedPlugins: plugins,
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		SkipHostEnv:      true,
		Logger:           hl,
		Stderr:           io.Discard,
		SyncStdout:       io.Discard,
		SyncStderr:       io.Discard,
		GRPCDialOptions:  []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize), grpc.MaxCallSendMsgSize(maxMsgSize))},
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("start provider %s: %w", cfg.Name, err)
	}
	raw, err := rpc.Dispense(pluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("start provider %s: %w", cfg.Name, err)
	}
	prov, ok := raw.(tfproto.Provider)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("start provider %s: unexpected client %T", cfg.Name, raw)
	}
	p := &Process{Provider: prov, client: client}
	log.Debug("provider started", "provider", cfg.Name, "protocol", client.NegotiatedVersion(), "binary", cfg.Binary)
	return p, nil
}

// Exited reports whether the provider process has exited.
func (p *Process) Exited() bool { return p.client.Exited() }

// Kill stops the provider: gracefully if it responds, else by force. It
// returns once the provider process and its log reader have stopped.
func (p *Process) Kill() { p.client.Kill() }

// passEnv lists host variables a provider may need to reach its API.
var passEnv = []string{
	"HOME", "PATH", "TMPDIR", "USER", "LANG",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

// Env is the provider's environment: passEnv from the host, plus TF_LOG
// (and TF_LOG_PROVIDER) when tfLog is set. go-plugin adds its own handshake
// variables.
func Env(tfLog string) []string {
	var env []string
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if tfLog != "" {
		env = append(env, "TF_LOG="+tfLog, "TF_LOG_PROVIDER="+tfLog)
	}
	return env
}

// slogSink receives the lines go-plugin parsed from the provider's stderr
// (hclog JSON, or plain text with a level inferred from "[DEBUG]"-style
// prefixes and panics flagged as errors) and go-plugin's own messages, and
// emits their severity and origin through slog without their content.
type slogSink struct{ log *slog.Logger }

// Provider text and go-plugin errors can echo credential values. Preserve severity
// and origin without forwarding untrusted messages or attributes into shared logs.
func (s slogSink) Accept(name string, level hclog.Level, _ string, _ ...any) {
	lvl := slogLevel(level)
	source := "provider"
	if name == "" {
		source = "go-plugin"
		if lvl < slog.LevelWarn {
			lvl = slog.LevelDebug
		}
	}
	s.log.Log(context.Background(), lvl, "provider log suppressed", "source", source)
}

func slogLevel(l hclog.Level) slog.Level {
	switch l {
	case hclog.Trace:
		return slog.LevelDebug - 4
	case hclog.Debug:
		return slog.LevelDebug
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
