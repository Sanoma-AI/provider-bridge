// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"context"
	"errors"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// stubWire answers from a protocol 5 schema response and echoes state.
type stubWire struct {
	resp       *tfplugin5.GetProviderSchema_Response
	getSchemas int
	gotMeta    []byte
}

func (*stubWire) protocol() int { return 5 }

func (w *stubWire) getSchema(context.Context) (proto.Message, *schema.ProviderSchema, Diagnostics, error) {
	w.getSchemas++
	s, err := providerSchema5(w.resp)
	return w.resp, s, nil, err
}

func (*stubWire) prepareConfig(context.Context, []byte) (dynamicValue, Diagnostics, error) {
	return dynamicValue{}, nil, nil
}

func (*stubWire) configure(context.Context, string, []byte) (Diagnostics, error) { return nil, nil }

func (*stubWire) importResourceState(_ context.Context, typeName, _ string) ([]importedRaw, Diagnostics, error) {
	return []importedRaw{{typeName: "x_other", state: dynamicValue{json: []byte(`{}`)}}}, nil, nil
}

func (*stubWire) upgradeResourceState(_ context.Context, _ string, _ int64, rawJSON []byte) (dynamicValue, Diagnostics, error) {
	return dynamicValue{json: rawJSON}, nil, nil
}

func (w *stubWire) readResource(_ context.Context, _ string, state, private, meta []byte) (dynamicValue, []byte, Diagnostics, error) {
	w.gotMeta = meta
	return dynamicValue{msgpack: state}, private, nil, nil
}

func stubResponse(withMeta bool) *tfplugin5.GetProviderSchema_Response {
	r := &tfplugin5.GetProviderSchema_Response{
		Provider: &tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
			Attributes: []*tfplugin5.Schema_Attribute{{Name: "token", Type: []byte(`"string"`), Optional: true}},
		}},
		ResourceSchemas: map[string]*tfplugin5.Schema{"x_item": {Block: &tfplugin5.Schema_Block{
			Attributes: []*tfplugin5.Schema_Attribute{{Name: "id", Type: []byte(`"string"`), Required: true}},
		}}},
	}
	if withMeta {
		r.ProviderMeta = &tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
			Attributes: []*tfplugin5.Schema_Attribute{{Name: "module", Type: []byte(`"string"`), Optional: true}},
		}}
	}
	return r
}

func TestClient(t *testing.T) {
	ctx := context.Background()
	w := &stubWire{resp: stubResponse(true)}
	c := &client{w: w}

	cfg := cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("t")})
	got, _, err := c.PrepareConfig(ctx, cfg)
	if err != nil || !got.RawEquals(cfg) {
		t.Fatalf("PrepareConfig = %#v, %v (an empty prepared config keeps cfg)", got, err)
	}
	state := cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("1")})
	read, priv, _, err := c.ReadResource(ctx, "x_item", state, []byte("p"))
	if err != nil || !read.RawEquals(state) || string(priv) != "p" {
		t.Fatalf("ReadResource = %#v %q %v", read, priv, err)
	}
	metaTy := cty.Object(map[string]cty.Type{"module": cty.String})
	if v, err := values.FromDynamic(w.gotMeta, nil, metaTy); err != nil || !v.IsNull() {
		t.Fatalf("provider_meta sent = %#v, %v; want a null object", v, err)
	}
	up, _, err := c.UpgradeResourceState(ctx, "x_item", 0, []byte(`{"id":"2"}`))
	if err != nil || up.GetAttr("id").AsString() != "2" {
		t.Fatalf("UpgradeResourceState = %#v, %v", up, err)
	}
	var unknown *UnknownTypeError
	if _, _, err := c.ImportResourceState(ctx, "x_nope", "1"); !errors.As(err, &unknown) {
		t.Fatalf("import of unknown type: %v", err)
	}
	if _, _, err := c.ImportResourceState(ctx, "x_item", "1"); !errors.As(err, &unknown) {
		t.Fatalf("import returning an unknown type: %v", err)
	}
	if w.getSchemas != 1 {
		t.Fatalf("GetSchema called %d times", w.getSchemas)
	}
	if raw, err := c.RawSchema(ctx); err != nil || raw != w.resp || w.getSchemas != 2 {
		t.Fatalf("RawSchema = %v, %v (calls %d)", raw, err, w.getSchemas)
	}
}

// A client seeded with SetSchema never asks the provider for its schema,
// and a provider without provider_meta gets none.
func TestClientSetSchema(t *testing.T) {
	ctx := context.Background()
	w := &stubWire{resp: stubResponse(false)}
	s, err := providerSchema5(w.resp)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{w: w}
	if err := c.SetSchema(s); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ReadResource(ctx, "x_item", cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("1")}), nil); err != nil {
		t.Fatal(err)
	}
	if w.getSchemas != 0 || w.gotMeta != nil {
		t.Fatalf("getSchema calls %d, meta %v", w.getSchemas, w.gotMeta)
	}
}
