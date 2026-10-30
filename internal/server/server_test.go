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
	"strings"
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

var ref = &bridgev1.ProviderRef{SessionId: "00000000-0000-0000-0000-000000000001", Source: "example/fake", Version: "1.0.0"}

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
		if code := connect.CodeOf(call()); code != connect.CodeInvalidArgument {
			t.Errorf("%s: want invalid_argument, got %s", name, code)
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

func TestPlanApply(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	if _, err := c.Configure(ctx, connect.NewRequest(&bridgev1.ConfigureRequest{Provider: ref})); err != nil {
		t.Fatal(err)
	}
	plan := func(prior *bridgev1.Resource, config string) (*bridgev1.PlanResponse, error) {
		req := &bridgev1.PlanRequest{Provider: ref, TypeName: "fake_thing", ConfigJson: config}
		if prior != nil {
			req.PriorStateJson, req.PriorPrivate, req.PriorSchemaVersion = prior.GetStateJson(), prior.GetPrivate(), &prior.SchemaVersion
		}
		res, err := c.Plan(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	apply := func(prior *bridgev1.Resource, config string, p *bridgev1.PlanResponse) *bridgev1.ApplyResponse {
		t.Helper()
		req := &bridgev1.ApplyRequest{Provider: ref, TypeName: "fake_thing", ConfigJson: config, Planned: p.GetPlanned(), PlannedPrivate: p.GetPlannedPrivate(), PlanToken: p.GetPlanToken()}
		if prior != nil {
			req.PriorStateJson, req.PriorPrivate, req.PriorSchemaVersion = prior.GetStateJson(), prior.GetPrivate(), &prior.SchemaVersion
		}
		res, err := c.Apply(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		return res.Msg
	}

	p, err := plan(nil, `{"secret":"s"}`)
	if err != nil || p.GetAction() != bridgev1.PlanResponse_ACTION_CREATE ||
		p.GetPlannedStateJson() != `{"id":null,"name":null,"secret":"s"}` ||
		strings.Join(p.GetUnknownPaths(), ",") != "id,name" || len(p.GetRequiresReplace()) != 0 {
		t.Fatalf("Plan create = %v, %v", p, err)
	}
	created := apply(nil, `{"secret":"s"}`, p).GetResource()
	if created.GetStateJson() != `{"id":"new","name":"thing-new","secret":"s"}` || string(created.GetPrivate()) != "planned" || created.GetSchemaVersion() != 1 {
		t.Fatalf("Apply create = %v", created)
	}

	if p, err := plan(created, `{"secret":"s"}`); err != nil || p.GetAction() != bridgev1.PlanResponse_ACTION_NONE {
		t.Fatalf("Plan no change = %v, %v", p, err)
	}
	p, err = plan(created, `{"secret":"t"}`)
	if err != nil || p.GetAction() != bridgev1.PlanResponse_ACTION_REPLACE || strings.Join(p.GetRequiresReplace(), ",") != "secret" ||
		strings.Join(p.GetUnknownPaths(), ",") != "id,name" {
		t.Fatalf("Plan replace = %v, %v", p, err)
	}

	// Provider errors come back in the response, beside the new state.
	p, _ = plan(nil, `{"secret":"partial"}`)
	res := apply(nil, `{"secret":"partial"}`, p)
	if res.GetResource().GetStateJson() == "" || len(res.GetErrors()) != 1 || res.GetErrors()[0].GetAttributePath() != "secret" {
		t.Fatalf("Apply partial = %v", res)
	}

	p, err = plan(created, "")
	if err != nil || p.GetAction() != bridgev1.PlanResponse_ACTION_DELETE || len(p.GetPlanned()) != 0 {
		t.Fatalf("Plan delete = %v, %v", p, err)
	}
	if res := apply(created, "", p); res.GetResource() != nil || len(res.GetErrors()) != 0 {
		t.Fatalf("Apply delete = %v", res)
	}

	_, err = plan(nil, `{"secret":"invalid"}`)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument || len(cerr.Details()) != 1 {
		t.Fatalf("Plan invalid config = %v", err)
	}
	for name, call := range map[string]func() error{
		"nothing to plan":      func() error { _, err := plan(nil, ""); return err },
		"config not an object": func() error { _, err := plan(nil, `[]`); return err },
		"unknown type": func() error {
			_, err := c.Plan(ctx, connect.NewRequest(&bridgev1.PlanRequest{Provider: ref, TypeName: "fake_nope", ConfigJson: `{}`}))
			return err
		},
		"config without planned": func() error {
			_, err := c.Apply(ctx, connect.NewRequest(&bridgev1.ApplyRequest{Provider: ref, TypeName: "fake_thing", ConfigJson: `{}`}))
			return err
		},
		"planned without config": func() error {
			_, err := c.Apply(ctx, connect.NewRequest(&bridgev1.ApplyRequest{Provider: ref, TypeName: "fake_thing", Planned: p.GetPlanned()}))
			return err
		},
		"planned not msgpack": func() error {
			_, err := c.Apply(ctx, connect.NewRequest(&bridgev1.ApplyRequest{Provider: ref, TypeName: "fake_thing", ConfigJson: `{}`, Planned: []byte("x")}))
			return err
		},
		"nothing to apply": func() error {
			_, err := c.Apply(ctx, connect.NewRequest(&bridgev1.ApplyRequest{Provider: ref, TypeName: "fake_thing"}))
			return err
		},
	} {
		want := connect.CodeInvalidArgument
		if name == "config without planned" || name == "planned without config" || name == "planned not msgpack" || name == "nothing to apply" {
			want = connect.CodeFailedPrecondition // Missing session-bound plan proof is refused first.
		}
		if got := connect.CodeOf(call()); got != want {
			t.Errorf("%s: want %s, got %s", name, want, got)
		}
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
