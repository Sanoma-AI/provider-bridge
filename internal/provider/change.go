// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// Action is what a plan does to a resource.
type Action int

// Actions.
const (
	ActionNone Action = iota + 1
	ActionCreate
	ActionUpdate
	ActionReplace
	ActionDelete
)

// Plan is a planned change to one resource.
type Plan struct {
	Action Action
	// State is the planned new state as JSON, unknown values null; nil
	// for a delete.
	State json.RawMessage
	// Unknown lists the paths of the unknown values in State.
	Unknown []string
	// RequiresReplace lists the changed paths that force a replacement.
	RequiresReplace []string
	// Planned is the planned state as msgpack, for Apply; nil for a delete.
	Planned []byte
	Private []byte
	Token   []byte
}

// Change is the input of Plan and Apply. Empty PriorState means create;
// empty Config means delete.
type Change struct {
	TypeName           string
	PriorState         []byte
	PriorPrivate       []byte
	PriorSchemaVersion *int64
	Config             []byte
	// Planned and PlannedPrivate are Plan's result; Apply only.
	Planned        []byte
	PlannedPrivate []byte
	PlanToken      []byte
}

// Plan plans the change from ch.PriorState to ch.Config as OpenTofu does:
// it validates the config, lays it over the prior state (ProposedNew) and
// calls PlanResourceChange. A replace is planned again as a create, so its
// plan is the new object's. A delete is not sent to the provider: OpenTofu
// does so only for providers that ask (the plan_destroy capability), and
// its plan is always null.
func (m *Manager) Plan(ctx context.Context, ref Ref, ch Change) (Plan, tfproto.Diagnostics, error) {
	ref, c, r, err := m.resourceInstance(ctx, ref, ch.TypeName)
	if err != nil {
		return Plan{}, nil, err
	}
	prior, warnings, err := m.prior(ctx, ref, c, r, ch)
	if err != nil {
		return Plan{}, nil, err
	}
	if len(ch.Config) == 0 {
		if prior.IsNull() {
			return Plan{}, nil, newError(CodeInvalidArgument, "prior_state_json or config_json is required", nil)
		}
		return Plan{Action: ActionDelete, Token: c.planToken(ch)}, warnings, nil
	}
	config, err := object(r.Block, "config_json", ch.Config)
	if err != nil {
		return Plan{}, nil, err
	}
	diags, err := c.inst.ValidateResourceConfig(ctx, ch.TypeName, config)
	if err != nil {
		return Plan{}, nil, m.callErr(ref, c, "validate config", err)
	}
	if diags.HasErrors() {
		return Plan{}, nil, diagError(CodeInvalidArgument, fmt.Sprintf("invalid %s config", ch.TypeName), diags)
	}
	warnings = append(warnings, diags...)

	planned, diags, err := m.planChange(ctx, ref, c, r, ch.TypeName, prior, config, ch.PriorPrivate)
	warnings = append(warnings, diags...)
	if err != nil {
		return Plan{}, nil, err
	}
	plan := Plan{Action: ActionCreate}
	if !prior.IsNull() {
		plan.RequiresReplace = requiresReplace(prior, planned.State, planned.RequiresReplace)
		switch {
		case len(plan.RequiresReplace) > 0:
			plan.Action = ActionReplace
			planned, diags, err = m.planChange(ctx, ref, c, r, ch.TypeName, cty.NullVal(prior.Type()), config, nil)
			warnings = append(warnings, diags...)
			if err != nil {
				return Plan{}, nil, err
			}
		case equal(planned.State, prior):
			plan.Action = ActionNone
		default:
			plan.Action = ActionUpdate
		}
	}
	plan.State, plan.Unknown, err = values.ToJSONUnknown(planned.State, r.Block.ImpliedType())
	if err != nil {
		return Plan{}, nil, newError(CodeInternal, fmt.Sprintf("plan %s: encode planned state", ch.TypeName), err)
	}
	plan.Planned, plan.Private = planned.Msgpack, planned.Private
	apply := ch
	apply.Planned, apply.PlannedPrivate = plan.Planned, plan.Private
	if plan.Action == ActionReplace {
		// A replace authorizes exactly the old-object delete and the newly planned create.
		deletion := ch
		deletion.Config, deletion.Planned, deletion.PlannedPrivate = nil, nil, nil
		apply.PriorState, apply.PriorPrivate, apply.PriorSchemaVersion = nil, nil, nil
		plan.Token = append(c.planToken(deletion), c.planToken(apply)...)
	} else {
		plan.Token = c.planToken(apply)
	}
	return plan, warnings, nil
}

// planChange calls PlanResourceChange with the proposed new state.
func (m *Manager) planChange(ctx context.Context, ref Ref, c *configured, r *schema.Resource, typeName string, prior, config cty.Value, priorPrivate []byte) (tfproto.PlannedChange, tfproto.Diagnostics, error) {
	proposed := values.ProposedNew(r.Block, prior, config)
	planned, diags, err := c.inst.PlanResourceChange(ctx, typeName, prior, proposed, config, priorPrivate)
	if err != nil {
		return planned, nil, m.callErr(ref, c, "plan", err)
	}
	if diags.HasErrors() {
		return planned, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("plan %s", typeName), diags)
	}
	if planned.State.IsNull() {
		return planned, nil, newError(CodeFailedPrecondition, fmt.Sprintf("plan %s: provider planned no object", typeName), nil)
	}
	return planned, diags.Warnings(), nil
}

// requiresReplace keeps the paths, among those the provider says force a
// replacement, whose value differs between prior and planned (or is
// unknown), as OpenTofu does.
func requiresReplace(prior, planned cty.Value, paths []cty.Path) []string {
	var out []string
	for _, p := range paths {
		before, errBefore := p.Apply(prior)
		after, errAfter := p.Apply(planned)
		switch {
		case errBefore != nil && errAfter != nil:
			continue // in neither: nothing changed
		case errBefore != nil:
			before = cty.NullVal(after.Type())
		case errAfter != nil:
			after = cty.NullVal(before.Type())
		}
		if !equal(before, after) {
			out = append(out, values.FormatPath(p))
		}
	}
	return out
}

// equal reports whether a and b are known to be equal.
func equal(a, b cty.Value) bool {
	eq := a.Equals(b)
	return eq.IsKnown() && eq.True()
}

// Apply applies a plan made by Plan from the same prior state and config.
// Error diagnostics from the provider are returned as errs, not as an
// error, beside the new state the provider returned (nil when it returned
// none, as after a delete), so a create that failed half way keeps its id.
func (m *Manager) Apply(ctx context.Context, ref Ref, ch Change) (res *Resource, errs, warnings tfproto.Diagnostics, err error) {
	ref, c, r, err := m.resourceInstance(ctx, ref, ch.TypeName)
	if err != nil {
		return nil, nil, nil, err
	}
	if !c.validPlanToken(ch) {
		return nil, nil, nil, newError(CodeFailedPrecondition, "plan does not belong to this session and apply input; plan again", nil)
	}

	ty := r.Block.ImpliedType()
	prior, warnings, err := m.prior(ctx, ref, c, r, ch)
	if err != nil {
		return nil, nil, nil, err
	}
	planned, config := cty.NullVal(ty), cty.NullVal(ty)
	plannedPrivate := ch.PlannedPrivate
	switch {
	case len(ch.Planned) == 0 && len(ch.Config) > 0:
		return nil, nil, nil, newError(CodeInvalidArgument, "planned is required with config_json", nil)
	case len(ch.Planned) == 0:
		if prior.IsNull() {
			return nil, nil, nil, newError(CodeInvalidArgument, "prior_state_json or planned is required", nil)
		}
		if plannedPrivate == nil {
			plannedPrivate = ch.PriorPrivate
		}
	default:
		if planned, err = values.FromDynamic(ch.Planned, nil, ty); err != nil || planned.IsNull() {
			return nil, nil, nil, newError(CodeInvalidArgument, "planned is not a plan of "+ch.TypeName, err)
		}
		if len(ch.Config) == 0 {
			return nil, nil, nil, newError(CodeInvalidArgument, "config_json is required with planned", nil)
		}
		if config, err = object(r.Block, "config_json", ch.Config); err != nil {
			return nil, nil, nil, err
		}
	}

	newState, newPrivate, diags, err := c.inst.ApplyResourceChange(ctx, ch.TypeName, prior, planned, config, plannedPrivate)
	if err != nil {
		return nil, nil, nil, m.callErr(ref, c, "apply", err)
	}
	errs, warnings = diags.Errors(), append(warnings, diags.Warnings()...)
	if newState.IsNull() {
		return nil, errs, warnings, nil
	}
	state, unknown, err := values.ToJSONUnknown(newState, ty)
	if err != nil {
		return nil, nil, nil, newError(CodeInternal, fmt.Sprintf("apply %s: encode new state", ch.TypeName), err)
	}
	if len(unknown) > 0 {
		errs = append(errs, tfproto.Diagnostic{
			Severity: tfproto.SeverityError,
			Summary:  "Provider returned invalid result object after apply",
			Detail:   fmt.Sprintf("Unknown values remain in the new state (null here): %v. This is a bug in the provider.", unknown),
		})
	}
	return &Resource{TypeName: ch.TypeName, State: state, Private: newPrivate, SchemaVersion: r.SchemaVersion}, errs, warnings, nil
}

// resourceInstance returns the configured instance for ref and the schema
// of typeName.
func (m *Manager) resourceInstance(ctx context.Context, ref Ref, typeName string) (Ref, *configured, *schema.Resource, error) {
	ref, c, err := m.instance(ctx, ref)
	if err != nil {
		return ref, nil, nil, err
	}
	r, ok := c.schema.Resource(typeName)
	if !ok {
		return ref, nil, nil, newError(CodeInvalidArgument, "", &tfproto.UnknownTypeError{TypeName: typeName})
	}
	return ref, c, r, nil
}

// prior decodes ch's prior state (null when empty), upgrading it first
// when ch names its schema version.
func (m *Manager) prior(ctx context.Context, ref Ref, c *configured, r *schema.Resource, ch Change) (cty.Value, tfproto.Diagnostics, error) {
	if len(ch.PriorState) == 0 {
		return cty.NullVal(r.Block.ImpliedType()), nil, nil
	}
	return m.state(ctx, ref, c, r, ch.TypeName, "prior_state_json", ch.PriorState, ch.PriorSchemaVersion)
}
