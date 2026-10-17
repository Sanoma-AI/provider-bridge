// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	bridgev1 "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

// TestIntegrationGitHubPlan plans github_repository changes with
// integrations/github (plugin protocol 5, SDKv2) and no credentials: its
// plans are computed locally, so spike S1's protocol 5 questions are
// answered offline; only apply needs the API.
func TestIntegrationGitHubPlan(t *testing.T) {
	testutil.SkipShort(t)
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, false), log)
	t.Cleanup(m.Shutdown)
	ref := testutil.Ref(t, "integrations/github")
	if _, _, err := m.Configure(ctxTimeout(t), ref, `{"owner":"example-org"}`); err != nil {
		t.Fatal(err)
	}
	r := changeRun{t, m, ref, "github_repository"}

	// An empty repository: node_id is known only after apply.
	plan := r.plan(nil, `{"name":"s1-empty","visibility":"private"}`)
	if plan.Action != provider.ActionCreate || !slices.Contains(plan.Unknown, "node_id") || !slices.Contains(plan.Unknown, "id") {
		t.Fatalf("create plan = %d, unknown %q", plan.Action, plan.Unknown)
	}

	// The recorded state, with its configurable attributes as config, is a
	// no-op; a new description is an update in place.
	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "testdata", "replies", "integrations_github_6.13.0", "github_repository", "provider-bridge", "read.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Response json.RawMessage }
	var read bridgev1.ReadResponse
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(fixture.Response, &read); err != nil {
		t.Fatal(err)
	}
	rr := read.GetResource()
	prior := &provider.Resource{TypeName: rr.GetTypeName(), State: []byte(rr.GetStateJson()), Private: rr.GetPrivate(), SchemaVersion: rr.GetSchemaVersion()}
	res, err := m.Schema(ctxTimeout(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(prior.State, &state); err != nil {
		t.Fatal(err)
	}
	for name, a := range res.Schema.Resources["github_repository"].Block.Attributes {
		if a.Computed || a.Deprecated {
			delete(state, name)
		}
	}
	state["visibility"] = "public"
	config, _ := json.Marshal(state)
	plan = r.plan(prior, string(config))
	t.Logf("no-op plan: action %d unknown %q replace %q", plan.Action, plan.Unknown, plan.RequiresReplace)
	if plan.Action != provider.ActionNone {
		t.Fatalf("plan of the recorded state = %d, unknown %q\n%s", plan.Action, plan.Unknown, plan.State)
	}
	state["description"] = "changed"
	config, _ = json.Marshal(state)
	wantPlan(t, r.plan(prior, string(config)), provider.ActionUpdate, nil, nil)
}
