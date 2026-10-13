// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package tfproto

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// wire6 speaks plugin protocol 6.
type wire6 struct{ rpc tfplugin6.ProviderClient }

func (wire6) protocol() int { return 6 }

func dv6(mp []byte) *tfplugin6.DynamicValue { return &tfplugin6.DynamicValue{Msgpack: mp} }

func fromDV6(d *tfplugin6.DynamicValue) dynamicValue {
	return dynamicValue{msgpack: d.GetMsgpack(), json: d.GetJson()}
}

func (w wire6) getSchema(ctx context.Context) (proto.Message, *schema.ProviderSchema, Diagnostics, error) {
	resp, err := w.rpc.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GetProviderSchema: %w", err)
	}
	diags := diagnostics6(resp.GetDiagnostics())
	if diags.HasErrors() {
		return resp, nil, diags, nil
	}
	s, err := providerSchema6(resp)
	if err != nil {
		return resp, nil, diags, fmt.Errorf("GetProviderSchema: %w", err)
	}
	return resp, s, diags, nil
}

// prepareConfig calls ValidateProviderConfig; protocol 6 has no prepared
// config.
func (w wire6) prepareConfig(ctx context.Context, cfg []byte) (dynamicValue, Diagnostics, error) {
	resp, err := w.rpc.ValidateProviderConfig(ctx, &tfplugin6.ValidateProviderConfig_Request{Config: dv6(cfg)})
	if err != nil {
		return dynamicValue{}, nil, fmt.Errorf("ValidateProviderConfig: %w", err)
	}
	return dynamicValue{}, diagnostics6(resp.GetDiagnostics()), nil
}

func (w wire6) configure(ctx context.Context, terraformVersion string, cfg []byte) (Diagnostics, error) {
	resp, err := w.rpc.ConfigureProvider(ctx, &tfplugin6.ConfigureProvider_Request{
		TerraformVersion:   terraformVersion,
		Config:             dv6(cfg),
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	})
	if err != nil {
		return nil, fmt.Errorf("ConfigureProvider: %w", err)
	}
	return diagnostics6(resp.GetDiagnostics()), nil
}

func (w wire6) importResourceState(ctx context.Context, typeName, id string) ([]importedRaw, Diagnostics, error) {
	resp, err := w.rpc.ImportResourceState(ctx, &tfplugin6.ImportResourceState_Request{
		TypeName:           typeName,
		Id:                 id,
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ImportResourceState: %w", err)
	}
	out := make([]importedRaw, 0, len(resp.GetImportedResources()))
	for _, ir := range resp.GetImportedResources() {
		out = append(out, importedRaw{typeName: ir.GetTypeName(), state: fromDV6(ir.GetState()), private: ir.GetPrivate()})
	}
	return out, diagnostics6(resp.GetDiagnostics()), nil
}

func (w wire6) upgradeResourceState(ctx context.Context, typeName string, version int64, rawJSON []byte) (dynamicValue, Diagnostics, error) {
	resp, err := w.rpc.UpgradeResourceState(ctx, &tfplugin6.UpgradeResourceState_Request{
		TypeName: typeName,
		Version:  version,
		RawState: &tfplugin6.RawState{Json: rawJSON},
	})
	if err != nil {
		return dynamicValue{}, nil, fmt.Errorf("UpgradeResourceState: %w", err)
	}
	return fromDV6(resp.GetUpgradedState()), diagnostics6(resp.GetDiagnostics()), nil
}

func (w wire6) readResource(ctx context.Context, typeName string, state, private, meta []byte) (dynamicValue, []byte, Diagnostics, error) {
	req := &tfplugin6.ReadResource_Request{
		TypeName:           typeName,
		CurrentState:       dv6(state),
		Private:            private,
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	}
	if meta != nil {
		req.ProviderMeta = dv6(meta)
	}
	resp, err := w.rpc.ReadResource(ctx, req)
	if err != nil {
		return dynamicValue{}, nil, nil, fmt.Errorf("ReadResource: %w", err)
	}
	return fromDV6(resp.GetNewState()), resp.GetPrivate(), diagnostics6(resp.GetDiagnostics()), nil
}
