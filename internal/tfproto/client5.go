// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"context"
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// Client5 speaks plugin protocol 5 (SDKv2 providers such as GitHub's).
type Client5 struct {
	rpc tfplugin5.ProviderClient

	mu    sync.Mutex
	types *typeCache
	meta  cty.Type
	raw   *tfplugin5.GetProviderSchema_Response
}

var _ Provider = (*Client5)(nil)

// NewClient5 wraps a protocol 5 gRPC client.
func NewClient5(rpc tfplugin5.ProviderClient) *Client5 { return &Client5{rpc: rpc} }

// Protocol returns 5.
func (c *Client5) Protocol() int { return 5 }

// GetSchema calls GetSchema once and caches the result.
func (c *Client5) GetSchema(ctx context.Context) (*schema.ProviderSchema, Diagnostics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.types != nil {
		return c.types.schema, nil, nil
	}
	resp, err := c.rpc.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, fmt.Errorf("GetSchema: %w", err)
	}
	diags := diagnostics5(resp.GetDiagnostics())
	if diags.HasErrors() {
		return nil, diags, nil
	}
	s, err := providerSchema5(resp)
	if err != nil {
		return nil, diags, fmt.Errorf("GetSchema: %w", err)
	}
	c.meta = cty.NilType
	if pm := resp.GetProviderMeta().GetBlock(); pm != nil {
		b, err := block5(pm)
		if err != nil {
			return nil, diags, fmt.Errorf("GetSchema: provider_meta: %w", err)
		}
		c.meta = b.ImpliedType()
	}
	c.types = &typeCache{schema: s}
	c.raw = resp
	return s, diags, nil
}

// RawSchema returns the unconverted GetSchema response (for debugging).
func (c *Client5) RawSchema(ctx context.Context) (*tfplugin5.GetProviderSchema_Response, error) {
	if _, _, err := c.GetSchema(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw, nil
}

func (c *Client5) loaded(ctx context.Context) (*typeCache, error) {
	if _, diags, err := c.GetSchema(ctx); err != nil {
		return nil, err
	} else if err := diags.Err(); err != nil {
		return nil, fmt.Errorf("GetSchema: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.types, nil
}

func dv5(v cty.Value, ty cty.Type) (*tfplugin5.DynamicValue, error) {
	mp, err := values.ToMsgpack(v, ty)
	if err != nil {
		return nil, err
	}
	return &tfplugin5.DynamicValue{Msgpack: mp}, nil
}

func (c *Client5) providerMeta() (*tfplugin5.DynamicValue, error) {
	c.mu.Lock()
	meta := c.meta
	c.mu.Unlock()
	if meta == cty.NilType {
		return nil, nil
	}
	return dv5(cty.NullVal(meta), meta)
}

// PrepareConfig calls PrepareProviderConfig.
func (c *Client5) PrepareConfig(ctx context.Context, cfg cty.Value) (cty.Value, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return cty.NilVal, nil, err
	}
	ty := types.schema.ProviderConfig.ImpliedType()
	dv, err := dv5(cfg, ty)
	if err != nil {
		return cty.NilVal, nil, fmt.Errorf("provider config: %w", err)
	}
	resp, err := c.rpc.PrepareProviderConfig(ctx, &tfplugin5.PrepareProviderConfig_Request{Config: dv})
	if err != nil {
		return cty.NilVal, nil, fmt.Errorf("PrepareProviderConfig: %w", err)
	}
	diags := diagnostics5(resp.GetDiagnostics())
	if diags.HasErrors() {
		return cfg, diags, nil
	}
	prepared, err := values.FromDynamic(resp.GetPreparedConfig().GetMsgpack(), resp.GetPreparedConfig().GetJson(), ty)
	if err != nil {
		return cty.NilVal, diags, fmt.Errorf("PrepareProviderConfig: prepared config: %w", err)
	}
	if prepared.IsNull() {
		prepared = cfg
	}
	return prepared, diags, nil
}

// Configure calls Configure.
func (c *Client5) Configure(ctx context.Context, cfg cty.Value, terraformVersion string) (Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return nil, err
	}
	dv, err := dv5(cfg, types.schema.ProviderConfig.ImpliedType())
	if err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	resp, err := c.rpc.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion:   terraformVersion,
		Config:             dv,
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	})
	if err != nil {
		return nil, fmt.Errorf("Configure: %w", err)
	}
	return diagnostics5(resp.GetDiagnostics()), nil
}

// ImportResourceState calls ImportResourceState.
func (c *Client5) ImportResourceState(ctx context.Context, typeName, id string) ([]ImportedResource, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := types.resource(typeName); err != nil {
		return nil, nil, err
	}
	resp, err := c.rpc.ImportResourceState(ctx, &tfplugin5.ImportResourceState_Request{
		TypeName:           typeName,
		Id:                 id,
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ImportResourceState: %w", err)
	}
	diags := diagnostics5(resp.GetDiagnostics())
	if diags.HasErrors() {
		return nil, diags, nil
	}
	out := make([]ImportedResource, 0, len(resp.GetImportedResources()))
	for _, ir := range resp.GetImportedResources() {
		r, err := types.resource(ir.GetTypeName())
		if err != nil {
			return nil, diags, fmt.Errorf("ImportResourceState: %w", err)
		}
		state, err := values.FromDynamic(ir.GetState().GetMsgpack(), ir.GetState().GetJson(), r.Block.ImpliedType())
		if err != nil {
			return nil, diags, fmt.Errorf("ImportResourceState: %s state: %w", ir.GetTypeName(), err)
		}
		out = append(out, ImportedResource{TypeName: ir.GetTypeName(), State: state, Private: ir.GetPrivate()})
	}
	return out, diags, nil
}

// ReadResource calls ReadResource.
func (c *Client5) ReadResource(ctx context.Context, typeName string, state cty.Value, private []byte) (cty.Value, []byte, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	r, err := types.resource(typeName)
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	ty := r.Block.ImpliedType()
	cur, err := dv5(state, ty)
	if err != nil {
		return cty.NilVal, nil, nil, fmt.Errorf("%s state: %w", typeName, err)
	}
	meta, err := c.providerMeta()
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	resp, err := c.rpc.ReadResource(ctx, &tfplugin5.ReadResource_Request{
		TypeName:           typeName,
		CurrentState:       cur,
		Private:            private,
		ProviderMeta:       meta,
		ClientCapabilities: &tfplugin5.ClientCapabilities{},
	})
	if err != nil {
		return cty.NilVal, nil, nil, fmt.Errorf("ReadResource: %w", err)
	}
	diags := diagnostics5(resp.GetDiagnostics())
	if diags.HasErrors() {
		return cty.NilVal, nil, diags, nil
	}
	newState, err := values.FromDynamic(resp.GetNewState().GetMsgpack(), resp.GetNewState().GetJson(), ty)
	if err != nil {
		return cty.NilVal, nil, diags, fmt.Errorf("ReadResource: %s new state: %w", typeName, err)
	}
	return newState, resp.GetPrivate(), diags, nil
}

// Stop calls Stop.
func (c *Client5) Stop(ctx context.Context) error {
	resp, err := c.rpc.Stop(ctx, &tfplugin5.Stop_Request{})
	if err != nil {
		return fmt.Errorf("Stop: %w", err)
	}
	if e := resp.GetError(); e != "" {
		return fmt.Errorf("Stop: %s", e)
	}
	return nil
}
