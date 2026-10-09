// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package plugin starts an OpenTofu provider binary as a go-plugin
// subprocess and completes the plugin protocol handshake, the way OpenTofu
// core does: magic cookie, protocol versions 5 and 6, gRPC only, AutoMTLS.
//
// The provider gets a minimal environment (see Env): credentials reach it
// only through Configure, never through inherited variables such as
// GITHUB_TOKEN. Its process runs in its own process group. Clients are not
// go-plugin "managed": the bridge's Manager kills every provider it started
// on Close and Shutdown. Its stderr (hclog JSON or plain text) is re-emitted
// through the bridge's slog logger.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	// Logger receives the provider's stderr as provider.<Name>.
	Logger *slog.Logger
	// TFLog, when non-empty, is passed to the provider as TF_LOG.
	TFLog string
}

// Process is a running, handshaken provider.
type Process struct {
	tfproto.Provider
	client *goplugin.Client
	stderr *lineLogger
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

	stderr := &lineLogger{log: log.With("logger", "provider."+cfg.Name)}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		VersionedPlugins: plugins,
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		SkipHostEnv:      true,
		// go-plugin's own hclog output is discarded; the provider's raw
		// stderr goes to Stderr and is re-logged by lineLogger.
		Logger:          hclog.NewNullLogger(),
		Stderr:          stderr,
		SyncStdout:      io.Discard,
		SyncStderr:      io.Discard,
		GRPCDialOptions: []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize), grpc.MaxCallSendMsgSize(maxMsgSize))},
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		stderr.flush()
		return nil, fmt.Errorf("start provider %s: %w", cfg.Name, err)
	}
	raw, err := rpc.Dispense(pluginName)
	if err != nil {
		client.Kill()
		stderr.flush()
		return nil, fmt.Errorf("start provider %s: %w", cfg.Name, err)
	}
	prov, ok := raw.(tfproto.Provider)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("start provider %s: unexpected client %T", cfg.Name, raw)
	}
	p := &Process{Provider: prov, client: client, stderr: stderr}
	log.Debug("provider started", "provider", cfg.Name, "protocol", client.NegotiatedVersion(), "binary", cfg.Binary)
	return p, nil
}

// Exited reports whether the provider process has exited.
func (p *Process) Exited() bool { return p.client.Exited() }

// Kill stops the provider: gracefully if it responds, else by force.
func (p *Process) Kill() {
	p.client.Kill()
	p.stderr.flush()
}

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

// lineLogger re-emits provider stderr lines through slog: hclog JSON lines
// keep their message, level and fields; anything else (panics, plain
// log.Printf output) is logged as "provider stderr" with the line attached.
type lineLogger struct {
	log *slog.Logger
	mu  sync.Mutex
	buf bytes.Buffer
}

const maxLine = 1 << 20

func (l *lineLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			if l.buf.Len() > maxLine {
				l.emit(l.buf.String())
				l.buf.Reset()
			}
			return len(p), nil
		}
		line := string(l.buf.Next(i + 1))
		l.emit(strings.TrimRight(line, "\r\n"))
	}
}

func (l *lineLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() > 0 {
		l.emit(l.buf.String())
		l.buf.Reset()
	}
}

func (l *lineLogger) emit(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	var entry map[string]any
	if json.Unmarshal([]byte(line), &entry) != nil {
		l.log.Warn("provider stderr", "line", line)
		return
	}
	msg, _ := entry["@message"].(string)
	lvl, _ := entry["@level"].(string)
	delete(entry, "@message")
	delete(entry, "@level")
	delete(entry, "@timestamp")
	attrs := make([]slog.Attr, 0, len(entry))
	for k, v := range entry {
		attrs = append(attrs, slog.Any(k, v))
	}
	l.log.LogAttrs(context.Background(), level(lvl), msg, attrs...)
}

func level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return slog.LevelDebug - 4
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
