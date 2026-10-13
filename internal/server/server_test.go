// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/gen/bridge/v1/providerbridgev1connect"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/server"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

func newClient(t *testing.T) (providerbridgev1connect.BridgeServiceClient, *testutil.FakeLauncher) {
	t.Helper()
	l := &testutil.FakeLauncher{}
	log := testutil.Logger(t)
	m := provider.NewManager(l, log)
	t.Cleanup(m.Shutdown)
	srv := httptest.NewServer(server.NewHandler(m, log))
	t.Cleanup(srv.Close)
	return providerbridgev1connect.NewBridgeServiceClient(srv.Client(), srv.URL, connect.WithProtoJSON()), l
}

var ref = &bridgev1.ProviderRef{Source: "example/fake", Version: "1.0.0"}

func TestHandlers(t *testing.T) {
	c, l := newClient(t)
	ctx := context.Background()

	s, err := c.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{Provider: ref}))
	if err != nil {
		t.Fatal(err)
	}
	if s.Msg.GetProtocol() != 6 || s.Msg.GetSha256() != testutil.FakeSHA256 || s.Msg.GetSchemaJson() == "" {
		t.Fatalf("GetSchema = %v", s.Msg)
	}

	cres, err := c.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: ref, ConfigJson: `{"api_key":"k"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cres.Msg.GetWarnings()) != 1 || cres.Msg.GetWarnings()[0].GetSeverity() != bridgev1.Diagnostic_SEVERITY_WARNING {
		t.Fatalf("warnings = %v", cres.Msg.GetWarnings())
	}

	ires, err := c.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "fake_thing", Id: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	r := ires.Msg.GetResources()[0]
	if r.GetStateJson() != `{"id":"a","name":"thing-a","secret":"s3cret"}` || string(r.GetPrivate()) != "priv+" || r.GetSchemaVersion() != 1 {
		t.Fatalf("Import = %v", r)
	}

	v0 := int64(0)
	ures, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "fake_thing", StateJson: `{"id":"u","title":"t"}`, SchemaVersion: &v0}))
	if err != nil || ures.Msg.GetResource().GetSchemaVersion() != 1 || l.Last().Upgrades.Load() != 1 {
		t.Fatalf("Read with schema_version = %v %v", ures, err)
	}

	rres, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "fake_thing", StateJson: `{"id":"gone"}`}))
	if err != nil || !rres.Msg.GetGone() {
		t.Fatalf("Read gone = %v %v", rres, err)
	}

	_, err = c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "fake_thing", StateJson: `{"id":"fail"}`}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition || len(cerr.Details()) != 1 {
		t.Fatalf("Read fail = %v", err)
	}
	v, _ := cerr.Details()[0].Value()
	if d, ok := v.(*bridgev1.Diagnostic); !ok || d.GetAttributePath() != "name" || d.GetSummary() != "Read failed" {
		t.Fatalf("detail = %v", v)
	}

	for name, call := range map[string]func() error{
		"bad json": func() error {
			_, err := c.Read(ctx, connect.NewRequest(&bridgev1.ReadRequest{Provider: ref, TypeName: "fake_thing", StateJson: `{`}))
			return err
		},
		"no provider": func() error {
			_, err := c.GetSchema(ctx, connect.NewRequest(&bridgev1.GetSchemaRequest{}))
			return err
		},
	} {
		if connect.CodeOf(call()) != connect.CodeInvalidArgument {
			t.Errorf("%s: want invalid_argument", name)
		}
	}

	l.Last().Exit()
	if _, err := c.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "fake_thing", Id: "a"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("after exit: %v", err)
	}
	if _, err := c.Import(ctx, connect.NewRequest(&bridgev1.ImportRequest{Provider: ref, TypeName: "fake_thing", Id: "gone"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("after exit, before configure: %v", err)
	}
	if _, err := c.Close(ctx, connect.NewRequest(&bridgev1.CloseRequest{})); err != nil {
		t.Fatal(err)
	}
}

func TestListen(t *testing.T) {
	dir, err := os.MkdirTemp("", "pb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "s.sock")

	ln, err := server.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v", fi.Mode(), err)
	}
	go func(l net.Listener) { _ = http.Serve(l, http.NotFoundHandler()) }(ln)
	if _, err := server.Listen(path); err == nil {
		t.Fatal("listened on a socket in use")
	}
	_ = ln.Close()

	// A stale socket (file left, nobody listening) is replaced.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}
	ln, err = server.Listen(path)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = ln.Close()

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Listen(path); err == nil {
		t.Fatal("replaced a regular file")
	}
}
