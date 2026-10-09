// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

var fakeRef = provider.Ref{Source: "example/fake", Version: "1.0.0"}

func newFakeManager(t *testing.T) (*provider.Manager, *testutil.FakeLauncher) {
	t.Helper()
	l := &testutil.FakeLauncher{}
	m := provider.NewManager(l, testutil.Logger(t))
	t.Cleanup(m.Shutdown)
	return m, l
}

func code(err error) provider.Code {
	var e *provider.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return -1
}

func TestSchemaIsCached(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()
	for range 3 {
		res, err := m.Schema(ctx, fakeRef)
		if err != nil {
			t.Fatal(err)
		}
		if res.Schema.Source != "example/fake" || res.Schema.Version != "1.0.0" || res.SHA256 != testutil.FakeSHA256 {
			t.Fatalf("schema identity = %s %s %s", res.Schema.Source, res.Schema.Version, res.SHA256)
		}
	}
	if l.Starts.Load() != 1 {
		t.Fatalf("started %d instances for one schema", l.Starts.Load())
	}
	if !l.Last().Killed() {
		t.Fatal("schema instance left running")
	}
	// The long form of the source shares the cache entry.
	if _, err := m.Schema(ctx, provider.Ref{Source: "registry.opentofu.org/example/fake", Version: "1.0.0"}); err != nil || l.Starts.Load() != 1 {
		t.Fatalf("long source form: err=%v starts=%d", err, l.Starts.Load())
	}
	// Configure seeds its instance with the known schema.
	if _, _, err := m.Configure(ctx, fakeRef, `{}`); err != nil {
		t.Fatal(err)
	}
	if l.Starts.Load() != 2 || !l.Last().Seeded.Load() {
		t.Fatalf("configure: starts=%d seeded=%v", l.Starts.Load(), l.Last().Seeded.Load())
	}
}

// The schema is cached beside the release for the same bridge build only.
func TestSchemaCacheFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	schemaOf := func() (*provider.SchemaResult, int32) {
		l := &testutil.FakeLauncher{Dir: dir}
		m := provider.NewManager(l, testutil.Logger(t))
		t.Cleanup(m.Shutdown)
		res, err := m.Schema(ctx, fakeRef)
		if err != nil {
			t.Fatal(err)
		}
		return res, l.Starts.Load()
	}
	first, starts := schemaOf()
	if starts != 1 {
		t.Fatalf("cold: %d starts", starts)
	}
	path := filepath.Join(dir, "schema.json")
	var file struct {
		Bridge string          `json:"bridge"`
		Schema json.RawMessage `json:"schema"`
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &file) != nil || file.Bridge == "" {
		t.Fatalf("cache file %s: %v %s", path, err, b)
	}
	second, starts := schemaOf()
	if starts != 0 || second.JSON != first.JSON {
		t.Fatalf("warm: %d starts, same document %v", starts, second.JSON == first.JSON)
	}
	// Written by another build: ignored and rewritten.
	file.Bridge = "another build"
	b, _ = json.Marshal(file)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, starts := schemaOf(); starts != 1 {
		t.Fatalf("other build's cache: %d starts", starts)
	}
	if _, starts := schemaOf(); starts != 0 {
		t.Fatalf("rewritten cache: %d starts", starts)
	}
}

func TestPinMismatch(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()
	bad := fakeRef
	bad.SHA256 = "00"
	check := func(what string, err error) {
		t.Helper()
		var pinErr *registry.PinMismatchError
		if code(err) != provider.CodeFailedPrecondition || !errors.As(err, &pinErr) || pinErr.Got != testutil.FakeSHA256 {
			t.Fatalf("%s: err = %v", what, err)
		}
	}
	_, err := m.Schema(ctx, bad)
	check("cold schema", err)
	_, _, err = m.Configure(ctx, bad, "{}")
	check("configure", err)

	// The same pin is checked on every path once the release is known.
	good := fakeRef
	good.SHA256 = strings.ToUpper(testutil.FakeSHA256)
	if _, err := m.Schema(ctx, good); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Configure(ctx, good, `{"api_key":"k"}`); err != nil {
		t.Fatal(err)
	}
	_, err = m.Schema(ctx, bad)
	check("warm schema", err)
	_, _, err = m.Import(ctx, bad, "fake_thing", "a")
	check("import", err)
	_, _, _, err = m.Read(ctx, bad, "fake_thing", []byte(`{"id":"a"}`), nil, nil)
	check("read", err)
	if _, _, err := m.Import(ctx, good, "fake_thing", "a"); err != nil {
		t.Fatal(err)
	}
	// The release was resolved (installed and hashed) once.
	if n := l.Resolves.Load(); n != 1 {
		t.Fatalf("resolved %d times", n)
	}
}

func TestInvalidRefs(t *testing.T) {
	m, _ := newFakeManager(t)
	for _, ref := range []provider.Ref{{}, {Source: "x", Version: "1.0.0"}, {Source: "a/b", Version: "~> 1.0"}} {
		if _, err := m.Schema(context.Background(), ref); code(err) != provider.CodeInvalidArgument {
			t.Errorf("Schema(%+v) err = %v", ref, err)
		}
	}
}

func TestConfigureLifecycle(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()

	if _, _, err := m.Import(ctx, fakeRef, "fake_thing", "a"); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("import before configure: %v", err)
	}
	if l.Resolves.Load() != 0 {
		t.Fatal("import before configure resolved the release")
	}
	warnings, sha, err := m.Configure(ctx, fakeRef, `{"api_key":"k"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || sha != testutil.FakeSHA256 {
		t.Fatalf("warnings=%v sha=%s", warnings, sha)
	}
	// Same config: idempotent, no new process.
	if _, _, err := m.Configure(ctx, fakeRef, `{"api_key":"k"}`); err != nil || l.Starts.Load() != 1 {
		t.Fatalf("reconfigure: err=%v starts=%d", err, l.Starts.Load())
	}
	// Other config: refused.
	if _, _, err := m.Configure(ctx, fakeRef, `{"api_key":"other"}`); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("other config: %v", err)
	}

	res, _, err := m.Import(ctx, fakeRef, "fake_thing", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || string(res[0].State) != `{"id":"a","name":"thing-a","secret":"s3cret"}` || string(res[0].Private) != "priv+" || res[0].SchemaVersion != 1 {
		t.Fatalf("import = %+v (%s)", res, res[0].State)
	}

	// Several imported objects are all read, in the provider's order.
	many, _, err := m.Import(ctx, fakeRef, "fake_thing", "a,b,c,d")
	if err != nil || len(many) != 4 {
		t.Fatalf("import of four = %v, %v", many, err)
	}
	for i, id := range []string{"a", "b", "c", "d"} {
		if want := `{"id":"` + id + `","name":"thing-` + id + `","secret":"s3cret"}`; string(many[i].State) != want {
			t.Fatalf("import %d = %s", i, many[i].State)
		}
	}
	if _, _, err := m.Import(ctx, fakeRef, "fake_thing", "a,gone"); code(err) != provider.CodeNotFound {
		t.Fatalf("import with one gone: %v", err)
	}

	got, gone, _, err := m.Read(ctx, fakeRef, "fake_thing", []byte(`{"id":"b"}`), nil, nil)
	if err != nil || gone || string(got.State) != `{"id":"b","name":"thing-b","secret":"s3cret"}` {
		t.Fatalf("read = %+v %v %v", got, gone, err)
	}
	if _, gone, _, err := m.Read(ctx, fakeRef, "fake_thing", []byte(`{"id":"gone"}`), nil, nil); err != nil || !gone {
		t.Fatalf("read gone = %v %v", gone, err)
	}
	if _, _, err := m.Import(ctx, fakeRef, "fake_thing", "gone"); code(err) != provider.CodeNotFound {
		t.Fatalf("import gone: %v", err)
	}
	_, _, _, err = m.Read(ctx, fakeRef, "fake_thing", []byte(`{"id":"fail"}`), nil, nil)
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || len(perr.Diagnostics) != 1 || perr.Diagnostics[0].Attribute != "name" {
		t.Fatalf("read fail: %#v", err)
	}
	for _, bad := range []string{`{"nope":1}`, `[]`, `null`, ``} {
		if _, _, _, err := m.Read(ctx, fakeRef, "fake_thing", []byte(bad), nil, nil); code(err) != provider.CodeInvalidArgument {
			t.Errorf("read %q: %v", bad, err)
		}
	}
	if _, _, _, err := m.Read(ctx, fakeRef, "fake_nope", []byte(`{}`), nil, nil); code(err) != provider.CodeInvalidArgument {
		t.Fatalf("unknown type: %v", err)
	}
	if _, _, err := m.Import(ctx, fakeRef, "fake_nope", "x"); code(err) != provider.CodeInvalidArgument {
		t.Fatalf("unknown type import: %v", err)
	}

	// The process dies: calls fail Unavailable, Configure starts a new one.
	l.Last().Exit()
	if _, _, err := m.Import(ctx, fakeRef, "fake_thing", "a"); code(err) != provider.CodeUnavailable {
		t.Fatalf("after exit: %v", err)
	}
	if _, _, err := m.Configure(ctx, fakeRef, `{"api_key":"other"}`); err != nil || l.Starts.Load() != 2 {
		t.Fatalf("configure after exit: err=%v starts=%d", err, l.Starts.Load())
	}

	m.Close(provider.Ref{Source: "registry.opentofu.org/example/fake", Version: "1.0.0"})
	if !l.Last().Killed() {
		t.Fatal("Close did not kill the instance")
	}
	if _, _, err := m.Import(ctx, fakeRef, "fake_thing", "a"); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("after close: %v", err)
	}
}

// A state written with an older schema version is upgraded by the provider
// before it is read.
func TestReadUpgradesState(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()
	if _, _, err := m.Configure(ctx, fakeRef, `{}`); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"id":"a","title":"old"}`)
	if _, _, _, err := m.Read(ctx, fakeRef, "fake_thing", old, nil, nil); code(err) != provider.CodeInvalidArgument {
		t.Fatalf("version 0 state read as current: %v", err)
	}
	v0, v1, v9 := int64(0), int64(1), int64(9)
	got, gone, _, err := m.Read(ctx, fakeRef, "fake_thing", old, nil, &v0)
	if err != nil || gone || string(got.State) != `{"id":"a","name":"thing-a","secret":"s3cret"}` || got.SchemaVersion != 1 {
		t.Fatalf("read v0 = %s %v %v", got.State, gone, err)
	}
	if _, _, _, err := m.Read(ctx, fakeRef, "fake_thing", []byte(`{"id":"b"}`), nil, &v1); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = m.Read(ctx, fakeRef, "fake_thing", old, nil, &v9)
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || len(perr.Diagnostics) != 1 {
		t.Fatalf("read v9: %v", err)
	}
	if n := l.Last().Upgrades.Load(); n != 3 {
		t.Fatalf("UpgradeResourceState called %d times, want 3", n)
	}
}

// Dynamic attributes arrive with a concrete type, in config and in state.
func TestDynamicAttributes(t *testing.T) {
	m, _ := newFakeManager(t)
	ctx := context.Background()
	if _, _, err := m.Configure(ctx, fakeRef, `{"api_key":"k","extra":{"value":{"a":[1,2]},"type":["object",{"a":["list","number"]}]}}`); err != nil {
		t.Fatal(err)
	}
	state := `{"id":"d","value":{"value":["x",true],"type":["tuple",["string","bool"]]}}`
	got, gone, _, err := m.Read(ctx, fakeRef, "fake_dyn", []byte(state), nil, nil)
	if err != nil || gone || string(got.State) != state {
		t.Fatalf("read = %s %v %v", got.State, gone, err)
	}
}

// A slow Configure holds up only calls for its own provider, and only
// until their context gives up; Shutdown during it kills its instance.
func TestSlowConfigureBlocksOnlyItsProvider(t *testing.T) {
	m, l := newFakeManager(t)
	l.Gate = make(chan struct{})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Configure(ctx, fakeRef, `{"api_key":"slow"}`)
		done <- err
	}()
	for l.Starts.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	slow := l.Last()

	other := provider.Ref{Source: "example/other", Version: "1.0.0"}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := m.Schema(tctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Configure(tctx, other, `{}`); err != nil {
		t.Fatal(err)
	}
	m.Close(other)

	short, cancelShort := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelShort()
	if _, err := m.Schema(short, fakeRef); code(err) != provider.CodeUnavailable {
		t.Fatalf("schema while configuring: %v", err)
	}

	m.Shutdown()
	close(l.Gate)
	if err := <-done; code(err) != provider.CodeUnavailable {
		t.Fatalf("configure across shutdown: %v", err)
	}
	if !slow.Killed() {
		t.Fatal("instance configured across Shutdown left running")
	}
}

func TestConfigureErrors(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()
	_, _, err := m.Configure(ctx, fakeRef, `{"api_key":"bad"}`)
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || perr.Diagnostics[0].Attribute != "api_key" {
		t.Fatalf("bad key: %v", err)
	}
	if !l.Last().Killed() {
		t.Fatal("failed instance left running")
	}
	for _, bad := range []string{`{"nope":1}`, `[1]`, `null`, `{"api_key":`} {
		if _, _, err := m.Configure(ctx, fakeRef, bad); code(err) != provider.CodeInvalidArgument {
			t.Errorf("config %q: %v", bad, err)
		}
	}
	m.Shutdown()
	if _, _, err := m.Configure(ctx, fakeRef, `{}`); code(err) != provider.CodeUnavailable {
		t.Fatalf("after shutdown: %v", err)
	}
}
