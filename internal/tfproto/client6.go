// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"context"
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// Client6 speaks plugin protocol 6 (Plugin Framework providers such as Stripe's).
type Client6 struct {
	rpc tfplugin6.ProviderClient

	mu    sync.Mutex
	types *typeCache
	meta  cty.Type
	raw   *tfplugin6.GetProviderSchema_Response
}

var _ Provider = (*Client6)(nil)

// NewClient6 wraps a protocol 6 gRPC client.
func NewClient6(rpc tfplugin6.ProviderClient) *Client6 { return &Client6{rpc: rpc} }

// Protocol returns 6.
func (c *Client6) Protocol() int { return 6 }

// GetSchema calls GetProviderSchema once and caches the result.
func (c *Client6) GetSchema(ctx context.Context) (*schema.ProviderSchema, Diagnostics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.types != nil {
		return c.types.schema, nil, nil
	}
	resp, err := c.rpc.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, nil, fmt.Errorf("GetProviderSchema: %w", err)
	}
	diags := diagnostics6(resp.GetDiagnostics())
	if diags.HasErrors() {
		return nil, diags, nil
	}
	s, err := providerSchema6(resp)
	if err != nil {
		return nil, diags, fmt.Errorf("GetSchema: %w", err)
	}
	c.meta = cty.NilType
	if pm := resp.GetProviderMeta().GetBlock(); pm != nil {
		b, err := block6(pm)
		if err != nil {
			return nil, diags, fmt.Errorf("GetSchema: provider_meta: %w", err)
		}
		c.meta = b.ImpliedType()
	}
	c.types = &typeCache{schema: s}
	c.raw = resp
	return s, diags, nil
}

// RawSchema returns the unconverted GetProviderSchema response (for debugging).
func (c *Client6) RawSchema(ctx context.Context) (*tfplugin6.GetProviderSchema_Response, error) {
	if _, _, err := c.GetSchema(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw, nil
}

func (c *Client6) loaded(ctx context.Context) (*typeCache, error) {
	if _, diags, err := c.GetSchema(ctx); err != nil {
		return nil, err
	} else if err := diags.Err(); err != nil {
		return nil, fmt.Errorf("GetSchema: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.types, nil
}

func dv6(v cty.Value, ty cty.Type) (*tfplugin6.DynamicValue, error) {
	mp, err := values.ToMsgpack(v, ty)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.DynamicValue{Msgpack: mp}, nil
}

func (c *Client6) providerMeta() (*tfplugin6.DynamicValue, error) {
	c.mu.Lock()
	meta := c.meta
	c.mu.Unlock()
	if meta == cty.NilType {
		return nil, nil
	}
	return dv6(cty.NullVal(meta), meta)
}

// PrepareConfig calls ValidateProviderConfig. Protocol 6 has no prepared
// config, so cfg is returned unchanged.
func (c *Client6) PrepareConfig(ctx context.Context, cfg cty.Value) (cty.Value, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return cty.NilVal, nil, err
	}
	dv, err := dv6(cfg, types.schema.ProviderConfig.ImpliedType())
	if err != nil {
		return cty.NilVal, nil, fmt.Errorf("provider config: %w", err)
	}
	resp, err := c.rpc.ValidateProviderConfig(ctx, &tfplugin6.ValidateProviderConfig_Request{Config: dv})
	if err != nil {
		return cty.NilVal, nil, fmt.Errorf("ValidateProviderConfig: %w", err)
	}
	return cfg, diagnostics6(resp.GetDiagnostics()), nil
}

// Configure calls ConfigureProvider.
func (c *Client6) Configure(ctx context.Context, cfg cty.Value, terraformVersion string) (Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return nil, err
	}
	dv, err := dv6(cfg, types.schema.ProviderConfig.ImpliedType())
	if err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	resp, err := c.rpc.ConfigureProvider(ctx, &tfplugin6.ConfigureProvider_Request{
		TerraformVersion:   terraformVersion,
		Config:             dv,
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	})
	if err != nil {
		return nil, fmt.Errorf("ConfigureProvider: %w", err)
	}
	return diagnostics6(resp.GetDiagnostics()), nil
}

// ImportResourceState calls ImportResourceState.
func (c *Client6) ImportResourceState(ctx context.Context, typeName, id string) ([]ImportedResource, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := types.resource(typeName); err != nil {
		return nil, nil, err
	}
	resp, err := c.rpc.ImportResourceState(ctx, &tfplugin6.ImportResourceState_Request{
		TypeName:           typeName,
		Id:                 id,
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ImportResourceState: %w", err)
	}
	diags := diagnostics6(resp.GetDiagnostics())
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
func (c *Client6) ReadResource(ctx context.Context, typeName string, state cty.Value, private []byte) (cty.Value, []byte, Diagnostics, error) {
	types, err := c.loaded(ctx)
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	r, err := types.resource(typeName)
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	ty := r.Block.ImpliedType()
	cur, err := dv6(state, ty)
	if err != nil {
		return cty.NilVal, nil, nil, fmt.Errorf("%s state: %w", typeName, err)
	}
	meta, err := c.providerMeta()
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	resp, err := c.rpc.ReadResource(ctx, &tfplugin6.ReadResource_Request{
		TypeName:           typeName,
		CurrentState:       cur,
		Private:            private,
		ProviderMeta:       meta,
		ClientCapabilities: &tfplugin6.ClientCapabilities{},
	})
	if err != nil {
		return cty.NilVal, nil, nil, fmt.Errorf("ReadResource: %w", err)
	}
	diags := diagnostics6(resp.GetDiagnostics())
	if diags.HasErrors() {
		return cty.NilVal, nil, diags, nil
	}
	newState, err := values.FromDynamic(resp.GetNewState().GetMsgpack(), resp.GetNewState().GetJson(), ty)
	if err != nil {
		return cty.NilVal, nil, diags, fmt.Errorf("ReadResource: %s new state: %w", typeName, err)
	}
	return newState, resp.GetPrivate(), diags, nil
}

// Stop calls StopProvider.
func (c *Client6) Stop(ctx context.Context) error {
	resp, err := c.rpc.StopProvider(ctx, &tfplugin6.StopProvider_Request{})
	if err != nil {
		return fmt.Errorf("StopProvider: %w", err)
	}
	if e := resp.GetError(); e != "" {
		return fmt.Errorf("StopProvider: %s", e)
	}
	return nil
}
