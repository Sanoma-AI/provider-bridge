// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package tfproto

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// wire5 speaks plugin protocol 5.
type wire5 struct{ rpc tfplugin5.ProviderClient }

func (wire5) protocol() int { return 5 }

func dv5(mp []byte) *tfplugin5.DynamicValue { return &tfplugin5.DynamicValue{Msgpack: mp} }

func fromDV5(d *tfplugin5.DynamicValue) dynamicValue {
	return dynamicValue{msgpack: d.GetMsgpack(), json: d.GetJson()}
}

func (w wire5) getSchema(ctx context.Context) (proto.Message, *schema.ProviderSchema, Diagnostics, error) {
	resp, err := w.rpc.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GetSchema: %w", err)
	}
	diags := diagnostics(resp.GetDiagnostics())
	if diags.HasErrors() {
		return resp, nil, diags, nil
	}
	s, err := providerSchema5(resp)
	if err != nil {
		return resp, nil, diags, fmt.Errorf("GetSchema: %w", err)
	}
	return resp, s, diags, nil
}

func (w wire5) prepareConfig(ctx context.Context, cfg []byte) (dynamicValue, Diagnostics, error) {
	resp, err := w.rpc.PrepareProviderConfig(ctx, &tfplugin5.PrepareProviderConfig_Request{Config: dv5(cfg)})
	if err != nil {
		return dynamicValue{}, nil, fmt.Errorf("PrepareProviderConfig: %w", err)
	}
	return fromDV5(resp.GetPreparedConfig()), diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) configure(ctx context.Context, terraformVersion string, cfg []byte) (Diagnostics, error) {
	resp, err := w.rpc.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion:   terraformVersion,
		Config:             dv5(cfg),
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	})
	if err != nil {
		return nil, fmt.Errorf("Configure: %w", err)
	}
	return diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) importResourceState(ctx context.Context, typeName, id string) ([]importedRaw, Diagnostics, error) {
	resp, err := w.rpc.ImportResourceState(ctx, &tfplugin5.ImportResourceState_Request{
		TypeName:           typeName,
		Id:                 id,
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ImportResourceState: %w", err)
	}
	out := make([]importedRaw, 0, len(resp.GetImportedResources()))
	for _, ir := range resp.GetImportedResources() {
		out = append(out, importedRaw{typeName: ir.GetTypeName(), state: fromDV5(ir.GetState()), private: ir.GetPrivate()})
	}
	return out, diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) upgradeResourceState(ctx context.Context, typeName string, version int64, rawJSON []byte) (dynamicValue, Diagnostics, error) {
	resp, err := w.rpc.UpgradeResourceState(ctx, &tfplugin5.UpgradeResourceState_Request{
		TypeName: typeName,
		Version:  version,
		RawState: &tfplugin5.RawState{Json: rawJSON},
	})
	if err != nil {
		return dynamicValue{}, nil, fmt.Errorf("UpgradeResourceState: %w", err)
	}
	return fromDV5(resp.GetUpgradedState()), diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) readResource(ctx context.Context, typeName string, state, private, meta []byte) (dynamicValue, []byte, Diagnostics, error) {
	req := &tfplugin5.ReadResource_Request{
		TypeName:           typeName,
		CurrentState:       dv5(state),
		Private:            private,
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	}
	if meta != nil {
		req.ProviderMeta = dv5(meta)
	}
	resp, err := w.rpc.ReadResource(ctx, req)
	if err != nil {
		return dynamicValue{}, nil, nil, fmt.Errorf("ReadResource: %w", err)
	}
	return fromDV5(resp.GetNewState()), resp.GetPrivate(), diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) validateResourceConfig(ctx context.Context, typeName string, config []byte) (Diagnostics, error) {
	resp, err := w.rpc.ValidateResourceTypeConfig(ctx, &tfplugin5.ValidateResourceTypeConfig_Request{
		TypeName:           typeName,
		Config:             dv5(config),
		ClientCapabilities: &tfplugin5.ClientCapabilities{WriteOnlyAttributesAllowed: true},
	})
	if err != nil {
		return nil, fmt.Errorf("ValidateResourceTypeConfig: %w", err)
	}
	return diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) planResourceChange(ctx context.Context, typeName string, prior, proposed, config, priorPrivate, meta []byte) (plannedRaw, Diagnostics, error) {
	req := &tfplugin5.PlanResourceChange_Request{
		TypeName:           typeName,
		PriorState:         dv5(prior),
		ProposedNewState:   dv5(proposed),
		Config:             dv5(config),
		PriorPrivate:       priorPrivate,
		ClientCapabilities: &tfplugin5.ClientCapabilities{WriteOnlyAttributesAllowed: true},
	}
	if meta != nil {
		req.ProviderMeta = dv5(meta)
	}
	resp, err := w.rpc.PlanResourceChange(ctx, req)
	if err != nil {
		return plannedRaw{}, nil, fmt.Errorf("PlanResourceChange: %w", err)
	}
	return plannedRaw{
		state:           fromDV5(resp.GetPlannedState()),
		requiresReplace: paths5(resp.GetRequiresReplace()),
		private:         resp.GetPlannedPrivate(),
	}, diagnostics(resp.GetDiagnostics()), nil
}

func (w wire5) applyResourceChange(ctx context.Context, typeName string, prior, planned, config, plannedPrivate, meta []byte) (dynamicValue, []byte, Diagnostics, error) {
	req := &tfplugin5.ApplyResourceChange_Request{
		TypeName:       typeName,
		PriorState:     dv5(prior),
		PlannedState:   dv5(planned),
		Config:         dv5(config),
		PlannedPrivate: plannedPrivate,
	}
	if meta != nil {
		req.ProviderMeta = dv5(meta)
	}
	resp, err := w.rpc.ApplyResourceChange(ctx, req)
	if err != nil {
		return dynamicValue{}, nil, nil, fmt.Errorf("ApplyResourceChange: %w", err)
	}
	return fromDV5(resp.GetNewState()), resp.GetPrivate(), diagnostics(resp.GetDiagnostics()), nil
}
