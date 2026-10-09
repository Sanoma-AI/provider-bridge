// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/gen/bridge/v1/providerbridgev1connect"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

// startBridge runs `provider-bridge serve` in-process on a fresh socket and
// returns clients speaking the Connect protocol (HTTP/1.1, JSON) and gRPC
// (HTTP/2 without TLS). Closing stdin stops it, as a dying parent would.
func startBridge(t *testing.T, extra ...string) (connectClient, grpcClient providerbridgev1connect.BridgeServiceClient, stop func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pb") // short: unix socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "bridge.sock")

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	args := append([]string{"serve", "--socket", socket, "--cache-dir", testutil.CacheDir(t), "--log-level", "debug"}, extra...)
	done := make(chan error, 1)
	go func() {
		done <- run(args, stdinR, stdoutW, testWriter{t})
		_ = stdoutW.Close()
	}()

	line, err := bufio.NewReader(stdoutR).ReadString('\n')
	if err != nil {
		t.Fatalf("no ready line: %v (run returned %v)", err, <-done)
	}
	var ready struct{ Event, Socket string }
	if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.Event != "ready" || ready.Socket != socket {
		t.Fatalf("ready line = %q", line)
	}
	fi, err := os.Stat(socket)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", fi.Mode(), err)
	}

	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	h1 := &http.Client{Transport: &http.Transport{DialContext: dial}}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	h2 := &http.Client{Transport: &http.Transport{DialContext: dial, Protocols: &protocols}}
	connectClient = providerbridgev1connect.NewBridgeServiceClient(h1, "http://bridge", connect.WithProtoJSON())
	grpcClient = providerbridgev1connect.NewBridgeServiceClient(h2, "http://bridge", connect.WithGRPC())

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = stdinW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("serve did not stop after stdin EOF")
		}
		if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("socket left behind: %v", err)
		}
	}
	t.Cleanup(stop)
	return connectClient, grpcClient, stop
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestE2ENull(t *testing.T) {
	testutil.SkipShort(t)
	c, g, stop := startBridge(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pin := testutil.Ref(t, "hashicorp/null")
	ref := &bridgev1.ProviderRef{Source: pin.Source, Version: pin.Version, Sha256: pin.SHA256}

	sres, err := c.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{Provider: ref}))
	if err != nil {
		t.Fatal(err)
	}
	if sres.Msg.GetProtocol() != 5 || sres.Msg.GetSha256() != pin.SHA256 || !strings.Contains(sres.Msg.GetSchemaJson(), `"null_resource"`) {
		t.Fatalf("GetSchema = protocol %d sha %s", sres.Msg.GetProtocol(), sres.Msg.GetSha256())
	}

	// A wrong pin is refused.
	bad := &bridgev1.ProviderRef{Source: pin.Source, Version: pin.Version, Sha256: strings.Repeat("0", 64)}
	if _, err := c.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{Provider: bad})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wrong pin: %v", err)
	}

	if _, err := g.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: ref, ConfigJson: "{}"})); err != nil {
		t.Fatal(err)
	}
	rres, err := g.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "null_resource", StateJson: `{"id":"7","triggers":{"k":"v"}}`}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rres.Msg.GetResource().GetStateJson(); got != `{"id":"7","triggers":{"k":"v"}}` {
		t.Fatalf("Read state = %s", got)
	}
	stop()
}

func TestE2ETestProvider(t *testing.T) {
	bin := testutil.TestProviderBinary(t)
	c, g, _ := startBridge(t, "--dev-provider", testutil.TestProviderSource+"="+bin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ref := &bridgev1.ProviderRef{Source: testutil.TestProviderSource, Version: testutil.TestProviderVersion}

	if _, err := c.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "testprov_item", Id: "x"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("import before configure: %v", err)
	}

	_, err := c.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: ref, ConfigJson: `{"api_key":"invalid"}`}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("invalid key: %v", err)
	}
	diag := firstDiagnostic(t, err)
	if diag.GetAttributePath() != "api_key" || diag.GetSeverity() != bridgev1.Diagnostic_SEVERITY_ERROR {
		t.Fatalf("diagnostic = %v", diag)
	}

	cres, err := c.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: ref, ConfigJson: `{"api_key":"k"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if cres.Msg.GetSha256() != "" {
		t.Fatalf("Configure echoed sha256 %s for an unverified dev provider", cres.Msg.GetSha256())
	}
	// A pin cannot match a dev provider.
	pinned := &bridgev1.ProviderRef{Source: ref.GetSource(), Version: ref.GetVersion(), Sha256: strings.Repeat("0", 64)}
	if _, err := c.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{Provider: pinned})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("pinned dev provider: %v", err)
	}

	ires, err := g.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "testprov_item", Id: "abc"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ires.Msg.GetResources()) != 1 {
		t.Fatalf("Import returned %d resources", len(ires.Msg.GetResources()))
	}
	res := ires.Msg.GetResources()[0]
	if !strings.Contains(res.GetStateJson(), `"level":12345678901234567890.5`) {
		t.Fatalf("number precision lost: %s", res.GetStateJson())
	}

	rres, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: res.GetTypeName(), StateJson: res.GetStateJson(), Private: res.GetPrivate()}))
	if err != nil {
		t.Fatal(err)
	}
	if rres.Msg.GetGone() || rres.Msg.GetResource().GetStateJson() != res.GetStateJson() {
		t.Fatalf("Read = %v", rres.Msg)
	}

	gres, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "testprov_item", StateJson: `{"id":"gone"}`}))
	if err != nil || !gres.Msg.GetGone() || gres.Msg.GetResource() != nil {
		t.Fatalf("Read gone = %v, %v", gres.Msg, err)
	}
	if _, err := c.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "testprov_item", Id: "gone"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("Import gone: %v", err)
	}
	_, err = g.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "testprov_item", StateJson: `{"id":"fail"}`}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || firstDiagnostic(t, err).GetAttributePath() != "name" {
		t.Fatalf("Read fail: %v", err)
	}
	if _, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "testprov_nope", StateJson: `{}`})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown type: %v", err)
	}

	if _, err := c.Close(ctx, connect.NewRequest(&bridgev1.CloseRequest{Provider: ref})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "testprov_item", StateJson: `{"id":"a"}`})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("after Close: %v", err)
	}
}

func firstDiagnostic(t *testing.T, err error) *bridgev1.Diagnostic {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || len(cerr.Details()) == 0 {
		t.Fatalf("no error details in %v", err)
	}
	v, derr := cerr.Details()[0].Value()
	if derr != nil {
		t.Fatal(derr)
	}
	d, ok := v.(*bridgev1.Diagnostic)
	if !ok {
		t.Fatalf("detail is %T", v)
	}
	return d
}
