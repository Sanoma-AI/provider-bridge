// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"context"
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// dynamicValue is a protocol DynamicValue: msgpack, or JSON, or neither for
// a null value.
type dynamicValue struct{ msgpack, json []byte }

func (d dynamicValue) decode(ty cty.Type) (cty.Value, error) {
	return values.FromDynamic(d.msgpack, d.json, ty)
}

type importedRaw struct {
	typeName string
	state    dynamicValue
	private  []byte
}

// wire moves one protocol version's messages. Values cross it msgpack
// encoded; it knows nothing about types. Errors name the RPC.
type wire interface {
	protocol() int
	// getSchema returns the raw response and, unless the diagnostics hold
	// an error, its conversion.
	getSchema(ctx context.Context) (proto.Message, *schema.ProviderSchema, Diagnostics, error)
	// prepareConfig validates cfg; a protocol without a prepared config
	// returns an empty dynamicValue.
	prepareConfig(ctx context.Context, cfg []byte) (dynamicValue, Diagnostics, error)
	configure(ctx context.Context, terraformVersion string, cfg []byte) (Diagnostics, error)
	importResourceState(ctx context.Context, typeName, id string) ([]importedRaw, Diagnostics, error)
	upgradeResourceState(ctx context.Context, typeName string, version int64, rawJSON []byte) (dynamicValue, Diagnostics, error)
	// readResource reads state; meta is the provider_meta value, nil when
	// the provider has no provider_meta schema.
	readResource(ctx context.Context, typeName string, state, private, meta []byte) (dynamicValue, []byte, Diagnostics, error)
}

// client implements Provider over a wire.
type client struct {
	w wire

	mu     sync.Mutex
	schema *schema.ProviderSchema
	meta   []byte // msgpack of a null provider_meta value, or nil
}

var _ Provider = (*client)(nil)

// NewClient5 returns a Provider speaking plugin protocol 5 (SDKv2
// providers such as GitHub's).
func NewClient5(rpc tfplugin5.ProviderClient) Provider { return &client{w: wire5{rpc}} }

// NewClient6 returns a Provider speaking plugin protocol 6 (Plugin
// Framework providers such as Stripe's).
func NewClient6(rpc tfplugin6.ProviderClient) Provider { return &client{w: wire6{rpc}} }

func (c *client) Protocol() int { return c.w.protocol() }

func (c *client) GetSchema(ctx context.Context) (*schema.ProviderSchema, Diagnostics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schema != nil {
		return c.schema, nil, nil
	}
	_, s, diags, err := c.w.getSchema(ctx)
	if err != nil || diags.HasErrors() {
		return nil, diags, err
	}
	if err := c.setSchema(s); err != nil {
		return nil, diags, err
	}
	return s, diags, nil
}

func (c *client) SetSchema(s *schema.ProviderSchema) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setSchema(s)
}

func (c *client) setSchema(s *schema.ProviderSchema) error {
	c.meta = nil
	if s.ProviderMeta != nil {
		ty := s.ProviderMeta.ImpliedType()
		mp, err := values.ToMsgpack(cty.NullVal(ty), ty)
		if err != nil {
			return fmt.Errorf("provider_meta: %w", err)
		}
		c.meta = mp
	}
	c.schema = s
	return nil
}

func (c *client) RawSchema(ctx context.Context) (proto.Message, error) {
	raw, _, _, err := c.w.getSchema(ctx)
	if raw != nil {
		return raw, nil
	}
	return nil, err
}

// loaded returns the schema, fetching it first if needed.
func (c *client) loaded(ctx context.Context) (*schema.ProviderSchema, []byte, error) {
	s, diags, err := c.GetSchema(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := diags.Err(); err != nil {
		return nil, nil, fmt.Errorf("get schema: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return s, c.meta, nil
}

func (c *client) resource(ctx context.Context, typeName string) (*schema.Resource, []byte, error) {
	s, meta, err := c.loaded(ctx)
	if err != nil {
		return nil, nil, err
	}
	r, ok := s.Resource(typeName)
	if !ok {
		return nil, nil, &UnknownTypeError{TypeName: typeName}
	}
	return r, meta, nil
}

func (c *client) PrepareConfig(ctx context.Context, cfg cty.Value) (cty.Value, Diagnostics, error) {
	s, _, err := c.loaded(ctx)
	if err != nil {
		return cty.NilVal, nil, err
	}
	ty := s.ProviderConfig.ImpliedType()
	mp, err := values.ToMsgpack(cfg, ty)
	if err != nil {
		return cty.NilVal, nil, fmt.Errorf("provider config: %w", err)
	}
	prepared, diags, err := c.w.prepareConfig(ctx, mp)
	if err != nil || diags.HasErrors() {
		return cfg, diags, err
	}
	v, err := prepared.decode(ty)
	if err != nil {
		return cty.NilVal, diags, fmt.Errorf("prepared config: %w", err)
	}
	if v.IsNull() {
		v = cfg
	}
	return v, diags, nil
}

func (c *client) Configure(ctx context.Context, cfg cty.Value, terraformVersion string) (Diagnostics, error) {
	s, _, err := c.loaded(ctx)
	if err != nil {
		return nil, err
	}
	mp, err := values.ToMsgpack(cfg, s.ProviderConfig.ImpliedType())
	if err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	return c.w.configure(ctx, terraformVersion, mp)
}

func (c *client) ImportResourceState(ctx context.Context, typeName, id string) ([]ImportedResource, Diagnostics, error) {
	if _, _, err := c.resource(ctx, typeName); err != nil {
		return nil, nil, err
	}
	imported, diags, err := c.w.importResourceState(ctx, typeName, id)
	if err != nil || diags.HasErrors() {
		return nil, diags, err
	}
	out := make([]ImportedResource, 0, len(imported))
	for _, ir := range imported {
		r, _, err := c.resource(ctx, ir.typeName)
		if err != nil {
			return nil, diags, fmt.Errorf("imported: %w", err)
		}
		state, err := ir.state.decode(r.Block.ImpliedType())
		if err != nil {
			return nil, diags, fmt.Errorf("imported %s state: %w", ir.typeName, err)
		}
		out = append(out, ImportedResource{TypeName: ir.typeName, State: state, Private: ir.private})
	}
	return out, diags, nil
}

func (c *client) UpgradeResourceState(ctx context.Context, typeName string, version int64, rawJSON []byte) (cty.Value, Diagnostics, error) {
	r, _, err := c.resource(ctx, typeName)
	if err != nil {
		return cty.NilVal, nil, err
	}
	upgraded, diags, err := c.w.upgradeResourceState(ctx, typeName, version, rawJSON)
	if err != nil || diags.HasErrors() {
		return cty.NilVal, diags, err
	}
	state, err := upgraded.decode(r.Block.ImpliedType())
	if err != nil {
		return cty.NilVal, diags, fmt.Errorf("%s upgraded state: %w", typeName, err)
	}
	return state, diags, nil
}

func (c *client) ReadResource(ctx context.Context, typeName string, state cty.Value, private []byte) (cty.Value, []byte, Diagnostics, error) {
	r, meta, err := c.resource(ctx, typeName)
	if err != nil {
		return cty.NilVal, nil, nil, err
	}
	ty := r.Block.ImpliedType()
	cur, err := values.ToMsgpack(state, ty)
	if err != nil {
		return cty.NilVal, nil, nil, fmt.Errorf("%s state: %w", typeName, err)
	}
	newState, newPrivate, diags, err := c.w.readResource(ctx, typeName, cur, private, meta)
	if err != nil || diags.HasErrors() {
		return cty.NilVal, nil, diags, err
	}
	v, err := newState.decode(ty)
	if err != nil {
		return cty.NilVal, nil, diags, fmt.Errorf("%s new state: %w", typeName, err)
	}
	return v, newPrivate, diags, nil
}
