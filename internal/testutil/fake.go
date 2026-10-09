// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
)

// FakeSHA256 is the SHA256SUMS hash the fake launcher reports.
const FakeSHA256 = "fa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4efa4e"

// FakeLauncher starts in-process FakeProviders; no binaries, no network.
type FakeLauncher struct {
	Starts   atomic.Int32
	Resolves atomic.Int32
	mu       sync.Mutex
	last     *FakeProvider
}

var _ provider.Launcher = (*FakeLauncher)(nil)

// Resolve returns a release with FakeSHA256.
func (l *FakeLauncher) Resolve(_ context.Context, source, version string) (provider.Release, error) {
	l.Resolves.Add(1)
	return provider.Release{Source: source, Version: version, SHA256: FakeSHA256, Binary: "/fake"}, nil
}

// Start returns a new FakeProvider.
func (l *FakeLauncher) Start(context.Context, provider.Release) (provider.Instance, error) {
	l.Starts.Add(1)
	p := &FakeProvider{}
	l.mu.Lock()
	l.last = p
	l.mu.Unlock()
	return p, nil
}

// Last returns the most recently started provider.
func (l *FakeLauncher) Last() *FakeProvider {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// FakeProvider is a protocol 6 provider with two resource types:
// fake_thing (id computed, name computed, secret sensitive) and fake_dyn
// (id, and value of DynamicPseudoType). Import passes the ID through; Read
// of fake_thing id "gone" returns no object and of id "fail" an error
// diagnostic on name; Read of fake_dyn returns the state unchanged.
// Configure fails for api_key "bad"; its config also has a dynamic
// attribute, extra.
type FakeProvider struct {
	exited     atomic.Bool
	killed     atomic.Bool
	configured atomic.Bool
}

var _ provider.Instance = (*FakeProvider)(nil)

func fakeThing() *schema.Block {
	return &schema.Block{
		Attributes: map[string]*schema.Attribute{
			"id":     {Type: schema.Type{Type: cty.String}, Computed: true},
			"name":   {Type: schema.Type{Type: cty.String}, Computed: true},
			"secret": {Type: schema.Type{Type: cty.String}, Optional: true, Sensitive: true},
		},
		Blocks: map[string]*schema.NestedBlock{},
	}
}

// FakeSchema returns a new copy of the schema FakeProvider serves.
func FakeSchema() *schema.ProviderSchema {
	return &schema.ProviderSchema{
		FormatVersion: schema.FormatVersion,
		Protocol:      6,
		ProviderConfig: &schema.Block{
			Attributes: map[string]*schema.Attribute{
				"api_key": {Type: schema.Type{Type: cty.String}, Optional: true, Sensitive: true},
				"extra":   {Type: schema.Type{Type: cty.DynamicPseudoType}, Optional: true},
			},
			Blocks: map[string]*schema.NestedBlock{},
		},
		Resources: map[string]*schema.Resource{
			"fake_thing": {SchemaVersion: 1, Block: fakeThing()},
			"fake_dyn": {Block: &schema.Block{
				Attributes: map[string]*schema.Attribute{
					"id":    {Type: schema.Type{Type: cty.String}, Required: true},
					"value": {Type: schema.Type{Type: cty.DynamicPseudoType}, Optional: true},
				},
				Blocks: map[string]*schema.NestedBlock{},
			}},
		},
		DataSources: map[string]*schema.Resource{},
	}
}

// Exit simulates the process dying.
func (p *FakeProvider) Exit() { p.exited.Store(true) }

// Killed reports whether Kill was called.
func (p *FakeProvider) Killed() bool { return p.killed.Load() }

// Protocol returns 6.
func (p *FakeProvider) Protocol() int { return 6 }

// Exited reports whether Exit or Kill was called.
func (p *FakeProvider) Exited() bool { return p.exited.Load() || p.killed.Load() }

// Kill marks the provider killed.
func (p *FakeProvider) Kill() { p.killed.Store(true) }

// GetSchema returns FakeSchema.
func (p *FakeProvider) GetSchema(context.Context) (*schema.ProviderSchema, tfproto.Diagnostics, error) {
	return FakeSchema(), nil, nil
}

// PrepareConfig warns once and returns cfg.
func (p *FakeProvider) PrepareConfig(_ context.Context, cfg cty.Value) (cty.Value, tfproto.Diagnostics, error) {
	return cfg, tfproto.Diagnostics{{Severity: tfproto.SeverityWarning, Summary: "fake warning"}}, nil
}

// Configure fails for api_key "bad".
func (p *FakeProvider) Configure(_ context.Context, cfg cty.Value, _ string) (tfproto.Diagnostics, error) {
	if k := cfg.GetAttr("api_key"); !k.IsNull() && k.AsString() == "bad" {
		return tfproto.Diagnostics{{Severity: tfproto.SeverityError, Summary: "Invalid API key", Attribute: "api_key"}}, nil
	}
	p.configured.Store(true)
	return nil, nil
}

func thing(id string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"id":     cty.StringVal(id),
		"name":   cty.StringVal("thing-" + id),
		"secret": cty.StringVal("s3cret"),
	})
}

// ImportResourceState passes the ID through.
func (p *FakeProvider) ImportResourceState(_ context.Context, typeName, id string) ([]tfproto.ImportedResource, tfproto.Diagnostics, error) {
	if typeName != "fake_thing" {
		return nil, nil, &tfproto.UnknownTypeError{TypeName: typeName}
	}
	state := cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id), "name": cty.NullVal(cty.String), "secret": cty.NullVal(cty.String)})
	return []tfproto.ImportedResource{{TypeName: typeName, State: state, Private: []byte("priv")}}, nil, nil
}

// ReadResource derives fake_thing state from the id and returns fake_dyn
// state unchanged.
func (p *FakeProvider) ReadResource(_ context.Context, typeName string, state cty.Value, private []byte) (cty.Value, []byte, tfproto.Diagnostics, error) {
	switch typeName {
	case "fake_dyn":
		return state, private, nil, nil
	case "fake_thing":
	default:
		return cty.NilVal, nil, nil, &tfproto.UnknownTypeError{TypeName: typeName}
	}
	switch id := state.GetAttr("id").AsString(); id {
	case "gone":
		return cty.NullVal(state.Type()), nil, nil, nil
	case "fail":
		return cty.NilVal, nil, tfproto.Diagnostics{{Severity: tfproto.SeverityError, Summary: "Read failed", Attribute: "name"}}, nil
	default:
		return thing(id), append(private, '+'), nil, nil
	}
}

// Stop does nothing.
func (p *FakeProvider) Stop(context.Context) error { return nil }
