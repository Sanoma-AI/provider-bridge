// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

func ctxTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestIntegrationNull drives hashicorp/null 3.3.2 (plugin protocol 5),
// downloaded from the registry and verified against testdata/pins.json.
func TestIntegrationNull(t *testing.T) {
	testutil.SkipShort(t)
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, false), log)
	t.Cleanup(m.Shutdown)
	ctx := ctxTimeout(t)
	ref := testutil.Ref(t, "hashicorp/null")

	res, err := m.Schema(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Schema.Protocol != 5 || res.SHA256 != ref.SHA256 {
		t.Fatalf("protocol %d sha %s", res.Schema.Protocol, res.SHA256)
	}
	testutil.Golden(t, filepath.Join(testutil.RepoRoot(t), "testdata", "schemas", "hashicorp_null_3.3.2.json"), res.JSON)

	if _, _, err := m.Configure(ctx, ref, `{}`); err != nil {
		t.Fatal(err)
	}
	got, gone, _, err := m.Read(ctx, ref, "null_resource", []byte(`{"id":"123","triggers":{"a":"b"}}`), nil)
	if err != nil || gone {
		t.Fatalf("read: %v gone=%v", err, gone)
	}
	if string(got.State) != `{"id":"123","triggers":{"a":"b"}}` {
		t.Fatalf("state = %s", got.State)
	}

	// null_resource has no importer: the error diagnostic surfaces.
	_, _, err = m.Import(ctx, ref, "null_resource", "123")
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || len(perr.Diagnostics) == 0 {
		t.Fatalf("import err = %v", err)
	}
	t.Logf("null_resource import: %v", err)
}

// TestIntegrationTestProvider drives the in-tree Plugin Framework provider
// (plugin protocol 6) through AutoMTLS.
func TestIntegrationTestProvider(t *testing.T) {
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, true), log)
	t.Cleanup(m.Shutdown)
	ctx := ctxTimeout(t)
	ref := provider.Ref{Source: testutil.TestProviderSource, Version: testutil.TestProviderVersion}

	res, err := m.Schema(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Schema.Protocol != 6 {
		t.Fatalf("protocol %d", res.Schema.Protocol)
	}
	testutil.Golden(t, filepath.Join(testutil.RepoRoot(t), "testdata", "schemas", "sanoma_testprov_0.0.1.json"), res.JSON)

	_, _, err = m.Configure(ctx, ref, `{"api_key":"invalid"}`)
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || perr.Diagnostics[0].Attribute != "api_key" {
		t.Fatalf("invalid key: %v", err)
	}
	if _, _, err := m.Configure(ctx, ref, `{"api_key":"k"}`); err != nil {
		t.Fatal(err)
	}

	imported, _, err := m.Import(ctx, ref, "testprov_item", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported %d resources", len(imported))
	}
	want := `{"endpoint":[{"url":"https://example.invalid/abc"}],"id":"abc","name":"item-abc","rules":[{"name":"https","port":443}],"settings":{"level":12345678901234567890.5,"secret":"s3cret-abc"},"tags":{"id":"abc"}}`
	if string(imported[0].State) != want {
		t.Fatalf("import state =\n%s\nwant\n%s", imported[0].State, want)
	}

	read, gone, _, err := m.Read(ctx, ref, "testprov_item", imported[0].State, imported[0].Private)
	if err != nil || gone || string(read.State) != want {
		t.Fatalf("read = %s gone=%v err=%v", read.State, gone, err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(read.State, &state); err != nil {
		t.Fatal(err)
	}

	if _, gone, _, err := m.Read(ctx, ref, "testprov_item", []byte(`{"id":"gone"}`), nil); err != nil || !gone {
		t.Fatalf("read gone: %v %v", gone, err)
	}
	if _, _, err := m.Import(ctx, ref, "testprov_item", "gone"); !errors.As(err, &perr) || perr.Code != provider.CodeNotFound {
		t.Fatalf("import gone: %v", err)
	}
	_, _, _, err = m.Read(ctx, ref, "testprov_item", []byte(`{"id":"fail"}`), nil)
	if !errors.As(err, &perr) || perr.Code != provider.CodeFailedPrecondition || perr.Diagnostics[0].Attribute != "name" {
		t.Fatalf("read fail: %v", err)
	}
	if !strings.Contains(err.Error(), "Read failed") {
		t.Fatalf("read fail message: %v", err)
	}
}
