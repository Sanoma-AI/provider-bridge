// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package schema_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

func attr(ty cty.Type, flags ...string) *schema.Attribute {
	a := &schema.Attribute{Type: ty}
	for _, f := range flags {
		switch f {
		case "required":
			a.Required = true
		case "optional":
			a.Optional = true
		case "computed":
			a.Computed = true
		case "sensitive":
			a.Sensitive = true
		}
	}
	return a
}

// sample covers every shape the document can take.
func sample() *schema.ProviderSchema {
	rules := &schema.Block{
		Attributes: map[string]*schema.Attribute{"name": attr(cty.String, "required"), "port": attr(cty.Number, "optional")},
		Blocks: map[string]*schema.NestedBlock{
			"match": {Nesting: schema.NestingSet, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"path": attr(cty.String, "optional")}, Blocks: map[string]*schema.NestedBlock{}}},
		},
	}
	s := &schema.ProviderSchema{
		FormatVersion: schema.FormatVersion,
		Source:        "example/sample",
		Version:       "1.0.0",
		Protocol:      6,
		ProviderConfig: &schema.Block{
			Attributes: map[string]*schema.Attribute{"api_key": attr(cty.String, "optional", "sensitive")},
			Blocks:     map[string]*schema.NestedBlock{},
		},
		Resources: map[string]*schema.Resource{
			"sample_item": {SchemaVersion: 2, Block: &schema.Block{
				Description:     "An item.",
				DescriptionKind: "markdown",
				Attributes: map[string]*schema.Attribute{
					"id":      attr(cty.String, "computed"),
					"name":    attr(cty.String, "required"),
					"tags":    attr(cty.Map(cty.String), "optional"),
					"extra":   attr(cty.DynamicPseudoType, "optional"),
					"members": attr(cty.Set(cty.String), "optional"),
					"settings": {Optional: true, NestedType: &schema.Object{Nesting: schema.NestingSingle, Attributes: map[string]*schema.Attribute{
						"secret": attr(cty.String, "optional", "sensitive"),
						"level":  attr(cty.Number, "optional"),
					}}},
					"ports": {Optional: true, NestedType: &schema.Object{Nesting: schema.NestingList, Attributes: map[string]*schema.Attribute{
						"number": attr(cty.Number, "required"),
					}}},
				},
				Blocks: map[string]*schema.NestedBlock{
					"rule":     {Nesting: schema.NestingList, Block: rules},
					"owner":    {Nesting: schema.NestingList, MaxItems: 1, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"login": attr(cty.String, "required")}, Blocks: map[string]*schema.NestedBlock{}}},
					"timeouts": {Nesting: schema.NestingSingle, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"read": attr(cty.String, "optional")}, Blocks: map[string]*schema.NestedBlock{}}},
					"labels":   {Nesting: schema.NestingMap, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"color": attr(cty.String, "optional")}, Blocks: map[string]*schema.NestedBlock{}}},
					"meta":     {Nesting: schema.NestingGroup, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"note": attr(cty.String, "optional")}, Blocks: map[string]*schema.NestedBlock{}}},
				},
			}},
		},
		DataSources: map[string]*schema.Resource{},
	}
	s.FillTypes()
	return s
}

func TestDocumentGoldenAndRoundTrip(t *testing.T) {
	b, err := schema.Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, "testdata/sample.json", b)

	back, err := schema.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := schema.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatal("document changed after a round trip")
	}
	if !back.Resources["sample_item"].Block.ImpliedType().Equals(sample().Resources["sample_item"].Block.ImpliedType()) {
		t.Fatal("implied type changed after a round trip")
	}
}

func TestMarshalRefusesUntypedAttribute(t *testing.T) {
	s := sample()
	s.Resources["sample_item"].Block.Blocks["rule"].Block.Attributes["bad"] = &schema.Attribute{Optional: true}
	if _, err := schema.Marshal(s); err == nil || !strings.Contains(err.Error(), "sample_item.rule.bad") {
		t.Fatalf("err = %v", err)
	}
}

// ImpliedType is computed once and safe to call concurrently.
func TestImpliedTypeConcurrent(t *testing.T) {
	b := sample().Resources["sample_item"].Block
	var wg sync.WaitGroup
	types := make([]cty.Type, 8)
	for i := range types {
		wg.Go(func() { types[i] = b.ImpliedType() })
	}
	wg.Wait()
	for _, ty := range types[1:] {
		if !ty.Equals(types[0]) {
			t.Fatal("implied types differ")
		}
	}
}

func TestUnmarshalRejectsOtherFormat(t *testing.T) {
	if _, err := schema.Unmarshal([]byte(`{"formatVersion":2}`)); err == nil {
		t.Fatal("accepted formatVersion 2")
	}
}

func TestImpliedType(t *testing.T) {
	got := sample().Resources["sample_item"].Block.ImpliedType()
	rule := cty.Object(map[string]cty.Type{
		"name": cty.String, "port": cty.Number,
		"match": cty.Set(cty.Object(map[string]cty.Type{"path": cty.String})),
	})
	want := cty.Object(map[string]cty.Type{
		"id":       cty.String,
		"name":     cty.String,
		"tags":     cty.Map(cty.String),
		"extra":    cty.DynamicPseudoType,
		"members":  cty.Set(cty.String),
		"settings": cty.Object(map[string]cty.Type{"secret": cty.String, "level": cty.Number}),
		"ports":    cty.List(cty.Object(map[string]cty.Type{"number": cty.Number})),
		"rule":     cty.List(rule),
		"owner":    cty.List(cty.Object(map[string]cty.Type{"login": cty.String})),
		"timeouts": cty.Object(map[string]cty.Type{"read": cty.String}),
		"labels":   cty.Map(cty.Object(map[string]cty.Type{"color": cty.String})),
		"meta":     cty.Object(map[string]cty.Type{"note": cty.String}),
	})
	if !got.Equals(want) {
		t.Fatalf("ImpliedType =\n%#v\nwant\n%#v", got, want)
	}
	// FillTypes gave nested attributes their implied type.
	if s := sample().Resources["sample_item"].Block.Attributes["settings"]; !s.Type.Equals(want.AttributeType("settings")) {
		t.Fatalf("settings type = %#v", s.Type)
	}
}

func TestImpliedTypeDynamicListBlock(t *testing.T) {
	b := &schema.Block{Blocks: map[string]*schema.NestedBlock{
		"dyn": {Nesting: schema.NestingList, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"v": attr(cty.DynamicPseudoType, "optional")}}},
		"set": {Nesting: schema.NestingSet, Block: &schema.Block{Attributes: map[string]*schema.Attribute{"v": attr(cty.DynamicPseudoType, "optional")}}},
	}}
	ty := b.ImpliedType()
	if ty.AttributeType("dyn") != cty.DynamicPseudoType {
		t.Fatalf("list block with dynamic content = %#v, want DynamicPseudoType", ty.AttributeType("dyn"))
	}
	if !ty.AttributeType("set").IsSetType() {
		t.Fatalf("set block = %#v", ty.AttributeType("set"))
	}
}

func TestCoerceConfig(t *testing.T) {
	b := sample().Resources["sample_item"].Block
	ty := b.ImpliedType()
	nulls := map[string]cty.Value{}
	for name, aty := range ty.AttributeTypes() {
		nulls[name] = cty.NullVal(aty)
	}
	nulls["name"] = cty.StringVal("x")
	nulls["rule"] = cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("r"), "port": cty.NullVal(cty.Number),
		"match": cty.NullVal(ty.AttributeType("rule").ElementType().AttributeType("match")),
	})})

	got, err := schema.CoerceConfig(b, cty.ObjectVal(nulls))
	if err != nil {
		t.Fatal(err)
	}
	if v := got.GetAttr("owner"); v.IsNull() || v.LengthInt() != 0 {
		t.Errorf("null list block not made empty: %#v", v)
	}
	if v := got.GetAttr("labels"); v.IsNull() || v.LengthInt() != 0 {
		t.Errorf("null map block not made empty: %#v", v)
	}
	if v := got.GetAttr("rule").Index(cty.NumberIntVal(0)).GetAttr("match"); v.IsNull() || !v.Type().IsSetType() {
		t.Errorf("nested null set block not made empty: %#v", v)
	}
	if v := got.GetAttr("timeouts"); !v.IsNull() {
		t.Errorf("single block should stay null: %#v", v)
	}
	if v := got.GetAttr("meta"); v.IsNull() || !v.GetAttr("note").IsNull() {
		t.Errorf("group block should be an object of nulls: %#v", v)
	}
	if v := got.GetAttr("tags"); !v.IsNull() {
		t.Errorf("attributes must not be touched: %#v", v)
	}

	// A dynamic attribute decoded from JSON has a concrete type.
	dyn, err := values.FromJSON([]byte(`{"name":"x","extra":{"value":["a",1],"type":["tuple",["string","number"]]}}`), ty)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := schema.CoerceConfig(b, dyn); err != nil || !got.GetAttr("extra").Type().IsTupleType() {
		t.Fatalf("dynamic attribute: %v %#v", err, got)
	}

	unknown := cty.ObjectVal(map[string]cty.Value{"api_key": cty.UnknownVal(cty.String)})
	if _, err := schema.CoerceConfig(sample().ProviderConfig, unknown); err == nil {
		t.Error("unknown value accepted")
	}
	if _, err := schema.CoerceConfig(sample().ProviderConfig, cty.ObjectVal(map[string]cty.Value{"x": cty.True})); err == nil {
		t.Error("wrong type accepted")
	}
}
