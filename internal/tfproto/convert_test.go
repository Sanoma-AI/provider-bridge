// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package tfproto

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
)

// A protocol 5 list block and a protocol 6 list nested attribute describe
// the same state shape; both must give the same implied type.
func TestBlockAndNestedAttributeAgree(t *testing.T) {
	r5 := &tfplugin5.GetProviderSchema_Response{
		Provider: &tfplugin5.Schema{Block: &tfplugin5.Schema_Block{}},
		ResourceSchemas: map[string]*tfplugin5.Schema{"x_item": {Version: 1, Block: &tfplugin5.Schema_Block{
			Attributes: []*tfplugin5.Schema_Attribute{{Name: "id", Type: []byte(`"string"`), Computed: true}},
			BlockTypes: []*tfplugin5.Schema_NestedBlock{{
				TypeName: "rule", Nesting: tfplugin5.Schema_NestedBlock_LIST,
				Block: &tfplugin5.Schema_Block{Attributes: []*tfplugin5.Schema_Attribute{
					{Name: "name", Type: []byte(`"string"`), Required: true},
					{Name: "secret", Type: []byte(`"string"`), Optional: true, Sensitive: true},
				}},
			}},
		}}},
	}
	r6 := &tfplugin6.GetProviderSchema_Response{
		Provider: &tfplugin6.Schema{Block: &tfplugin6.Schema_Block{}},
		ResourceSchemas: map[string]*tfplugin6.Schema{"x_item": {Version: 1, Block: &tfplugin6.Schema_Block{
			Attributes: []*tfplugin6.Schema_Attribute{
				{Name: "id", Type: []byte(`"string"`), Computed: true},
				{Name: "rule", Optional: true, NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_LIST,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "name", Type: []byte(`"string"`), Required: true},
						{Name: "secret", Type: []byte(`"string"`), Optional: true, Sensitive: true},
					},
				}},
			},
		}}},
	}
	s5, err := providerSchema5(r5)
	if err != nil {
		t.Fatal(err)
	}
	s6, err := providerSchema6(r6)
	if err != nil {
		t.Fatal(err)
	}
	t5 := s5.Resources["x_item"].Block.ImpliedType()
	t6 := s6.Resources["x_item"].Block.ImpliedType()
	if !t5.Equals(t6) {
		t.Fatalf("protocol 5 %#v != protocol 6 %#v", t5, t6)
	}
	want := cty.List(cty.Object(map[string]cty.Type{"name": cty.String, "secret": cty.String}))
	if got := s6.Resources["x_item"].Block.Attributes["rule"].Type; !got.Equals(want) {
		t.Fatalf("nested attribute type = %#v", got)
	}
	if !s6.Resources["x_item"].Block.Attributes["rule"].NestedType.Attributes["secret"].Sensitive {
		t.Fatal("nested sensitive flag lost")
	}
	if s5.Resources["x_item"].SchemaVersion != 1 || s5.Protocol != 5 || s6.Protocol != 6 {
		t.Fatal("version or protocol lost")
	}
}

func TestInvalidType(t *testing.T) {
	r5 := &tfplugin5.GetProviderSchema_Response{Provider: &tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
		Attributes: []*tfplugin5.Schema_Attribute{{Name: "bad", Type: []byte(`"strin"`)}},
	}}}
	if _, err := providerSchema5(r5); err == nil {
		t.Fatal("invalid type accepted")
	}
}

func TestDiagnostics(t *testing.T) {
	d5 := diagnostics([]*tfplugin5.Diagnostic{{
		Severity: tfplugin5.Diagnostic_ERROR, Summary: "bad", Detail: "very",
		Attribute: &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
			{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "rules"}},
			{Selector: &tfplugin5.AttributePath_Step_ElementKeyInt{ElementKeyInt: 0}},
			{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "name"}},
		}},
	}, {Severity: tfplugin5.Diagnostic_WARNING, Summary: "meh"}})
	if !d5.HasErrors() || d5[0].Attribute != "rules[0].name" || len(d5.Warnings()) != 1 {
		t.Fatalf("diagnostics = %+v", d5)
	}
	if got := d5.Err().Error(); got != "rules[0].name: bad: very" {
		t.Fatalf("Err = %q", got)
	}
	d6 := diagnostics6([]*tfplugin6.Diagnostic{{
		Severity: tfplugin6.Diagnostic_INVALID, Summary: "odd",
		Attribute: &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
			{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "tags"}},
			{Selector: &tfplugin6.AttributePath_Step_ElementKeyString{ElementKeyString: "env"}},
		}},
	}})
	if !d6.HasErrors() || d6[0].Attribute != `tags["env"]` {
		t.Fatalf("diagnostics6 = %+v", d6)
	}
}
