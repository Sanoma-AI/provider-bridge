// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

// changeRun drives Plan and Apply on one configured provider.
type changeRun struct {
	t    *testing.T
	m    *provider.Manager
	ref  provider.Ref
	kind string
}

func (r changeRun) plan(prior *provider.Resource, config string) provider.Plan {
	r.t.Helper()
	plan, _, err := r.m.Plan(ctxTimeout(r.t), r.ref, r.change(prior, config, provider.Plan{}))
	if err != nil {
		r.t.Fatalf("plan %s: %v", config, err)
	}
	return plan
}

func (r changeRun) apply(prior *provider.Resource, config string, plan provider.Plan) *provider.Resource {
	r.t.Helper()
	res, errs, _, err := r.m.Apply(ctxTimeout(r.t), r.ref, r.change(prior, config, plan))
	if err != nil || len(errs) > 0 {
		r.t.Fatalf("apply %s: %v %v", config, err, errs)
	}
	return res
}

func (r changeRun) change(prior *provider.Resource, config string, plan provider.Plan) provider.Change {
	ch := provider.Change{TypeName: r.kind, Config: []byte(config), Planned: plan.Planned, PlannedPrivate: plan.Private}
	if prior != nil {
		ch.PriorState, ch.PriorPrivate, ch.PriorSchemaVersion = prior.State, prior.Private, &prior.SchemaVersion
	}
	return ch
}

func wantPlan(t *testing.T, plan provider.Plan, action provider.Action, unknown, replace []string) {
	t.Helper()
	if plan.Action != action || !slices.Equal(plan.Unknown, unknown) || !slices.Equal(plan.RequiresReplace, replace) {
		t.Fatalf("plan = action %d unknown %q replace %q, want %d %q %q (state %s)",
			plan.Action, plan.Unknown, plan.RequiresReplace, action, unknown, replace, plan.State)
	}
}

func attr(t *testing.T, state json.RawMessage, name string) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(state, &obj); err != nil {
		t.Fatal(err)
	}
	s, _ := obj[name].(string)
	return s
}

// TestIntegrationNullPlanApply creates, keeps, replaces and deletes a
// null_resource (plugin protocol 5, served by the Plugin Framework since
// null 3.2; SDKv2 is covered by TestIntegrationGitHubPlan): the create plan
// has id unknown,
// a plan from the applied state is a no-op, and a changed trigger (ForceNew)
// is a replace.
func TestIntegrationNullPlanApply(t *testing.T) {
	testutil.SkipShort(t)
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, false), log)
	t.Cleanup(m.Shutdown)
	ref := testutil.Ref(t, "hashicorp/null")
	if _, _, err := m.Configure(ctxTimeout(t), ref, `{}`); err != nil {
		t.Fatal(err)
	}
	r := changeRun{t, m, ref, "null_resource"}

	v1 := `{"triggers":{"v":"1"}}`
	plan := r.plan(nil, v1)
	wantPlan(t, plan, provider.ActionCreate, []string{"id"}, nil)
	if string(plan.State) != `{"id":null,"triggers":{"v":"1"}}` {
		t.Fatalf("planned state = %s", plan.State)
	}
	created := r.apply(nil, v1, plan)
	if attr(t, created.State, "id") == "" {
		t.Fatalf("created state = %s", created.State)
	}
	t.Logf("created %s, private %q", created.State, created.Private)

	wantPlan(t, r.plan(created, v1), provider.ActionNone, nil, nil)

	v2 := `{"triggers":{"v":"2"}}`
	plan = r.plan(created, v2)
	wantPlan(t, plan, provider.ActionReplace, []string{"id"}, []string{"triggers"})
	if gone := r.apply(created, "", provider.Plan{}); gone != nil {
		t.Fatalf("delete returned %s", gone.State)
	}
	replaced := r.apply(nil, v2, plan)
	if id := attr(t, replaced.State, "id"); id == "" || id == attr(t, created.State, "id") {
		t.Fatalf("replaced state = %s", replaced.State)
	}

	wantPlan(t, r.plan(replaced, ""), provider.ActionDelete, nil, nil)
	if gone := r.apply(replaced, "", provider.Plan{}); gone != nil {
		t.Fatalf("delete returned %s", gone.State)
	}
}

// TestIntegrationTestProviderPlanApply drives testprov_item (plugin
// protocol 6, Plugin Framework): id unknown at plan and known after apply,
// an in-place update, a replace on name (RequiresReplace), a create that
// fails half way and keeps its state, and validation errors.
func TestIntegrationTestProviderPlanApply(t *testing.T) {
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, true), log)
	t.Cleanup(m.Shutdown)
	ref := provider.Ref{Source: testutil.TestProviderSource, Version: testutil.TestProviderVersion}
	if _, _, err := m.Configure(ctxTimeout(t), ref, `{"api_key":"k"}`); err != nil {
		t.Fatal(err)
	}
	r := changeRun{t, m, ref, "testprov_item"}

	v1 := `{"name":"a","tags":{"env":"dev"},"endpoint":[{"url":"https://a.invalid"}]}`
	plan := r.plan(nil, v1)
	wantPlan(t, plan, provider.ActionCreate, []string{"id"}, nil)
	created := r.apply(nil, v1, plan)
	want := `{"endpoint":[{"url":"https://a.invalid"}],"id":"a","name":"a","rules":null,"settings":null,"tags":{"env":"dev"}}`
	if string(created.State) != want {
		t.Fatalf("created state =\n%s\nwant\n%s", created.State, want)
	}

	wantPlan(t, r.plan(created, v1), provider.ActionNone, nil, nil)

	v2 := `{"name":"a","tags":{"env":"prod"},"endpoint":[{"url":"https://a.invalid"}]}`
	plan = r.plan(created, v2)
	wantPlan(t, plan, provider.ActionUpdate, nil, nil)
	updated := r.apply(created, v2, plan)
	if attr(t, updated.State, "id") != "a" || string(updated.State) == string(created.State) {
		t.Fatalf("updated state = %s", updated.State)
	}

	wantPlan(t, r.plan(updated, `{"name":"b"}`), provider.ActionReplace, []string{"id"}, []string{"name"})

	// A create that fails after the object exists returns its state.
	partial := `{"name":"partial"}`
	res, errs, _, err := m.Apply(ctxTimeout(t), ref, r.change(nil, partial, r.plan(nil, partial)))
	if err != nil || res == nil || attr(t, res.State, "id") != "partial" || len(errs) != 1 || errs[0].Attribute != "tags" {
		t.Fatalf("partial create = %v, errors %v, %v", res, errs, err)
	}

	// Invalid config: the provider's diagnostic, with its path.
	_, _, err = m.Plan(ctxTimeout(t), ref, r.change(nil, `{"name":"x","rules":[{"port":1}]}`, provider.Plan{}))
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.CodeInvalidArgument || len(perr.Diagnostics) == 0 {
		t.Fatalf("invalid config: %v", err)
	}
	t.Logf("invalid config: %v (attribute %q)", err, perr.Diagnostics[0].Attribute)
}
