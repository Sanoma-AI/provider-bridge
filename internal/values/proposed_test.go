// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package values

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

func proposedBlock() *schema.Block {
	return &schema.Block{
		Attributes: map[string]*schema.Attribute{
			"id":    {Type: cty.String, Computed: true},
			"name":  {Type: cty.String, Required: true},
			"mode":  {Type: cty.String, Optional: true, Computed: true},
			"token": {Type: cty.String, Optional: true, WriteOnly: true},
			"settings": {Optional: true, Computed: true, NestedType: &schema.Object{
				Nesting: schema.NestingSingle,
				Attributes: map[string]*schema.Attribute{
					"level":   {Type: cty.Number, Optional: true},
					"version": {Type: cty.String, Computed: true},
				},
			}},
		},
		Blocks: map[string]*schema.NestedBlock{
			"rule": {Nesting: schema.NestingList, Block: &schema.Block{Attributes: map[string]*schema.Attribute{
				"name":   {Type: cty.String, Required: true},
				"port":   {Type: cty.Number, Optional: true, Computed: true},
				"secret": {Type: cty.String, Optional: true, WriteOnly: true},
			}}},
			"member": {Nesting: schema.NestingSet, Block: &schema.Block{Attributes: map[string]*schema.Attribute{
				"user": {Type: cty.String, Required: true},
				"role": {Type: cty.String, Computed: true},
			}}},
		},
	}
}

func decode(t *testing.T, b *schema.Block, js string) cty.Value {
	t.Helper()
	v, err := FromJSON([]byte(js), b.ImpliedType())
	if err != nil {
		t.Fatal(err)
	}
	v, err = schema.CoerceConfig(b, v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestProposedNew(t *testing.T) {
	b := proposedBlock()
	ty := b.ImpliedType()
	prior := `{"id":"1","name":"a","mode":"fast","settings":{"level":1,"version":"v1"},` +
		`"rule":[{"name":"x","port":80}],"member":[{"user":"u","role":"admin"},{"user":"v","role":"dev"}]}`
	for name, tc := range map[string]struct{ prior, config, want string }{
		"create keeps computed null and drops write-only": {
			config: `{"name":"a","token":"t","rule":[{"name":"x","secret":"s"}],"member":[{"user":"u"}]}`,
			want:   `{"id":null,"member":[{"role":null,"user":"u"}],"mode":null,"name":"a","rule":[{"name":"x","port":null,"secret":null}],"settings":null,"token":null}`,
		},
		"computed values absent from config keep their prior values": {
			prior:  prior,
			config: `{"name":"b","settings":{"level":2},"rule":[{"name":"y"},{"name":"z","secret":"s"}],"member":[{"user":"v"},{"user":"w"}]}`,
			want:   `{"id":"1","member":[{"role":"dev","user":"v"},{"role":null,"user":"w"}],"mode":"fast","name":"b","rule":[{"name":"y","port":80,"secret":null},{"name":"z","port":null,"secret":null}],"settings":{"level":2,"version":"v1"},"token":null}`,
		},
		"config wins for an optional computed value": {
			prior:  prior,
			config: `{"name":"a","mode":"slow","settings":{"level":1},"rule":[{"name":"x","port":81}]}`,
			want:   `{"id":"1","member":[],"mode":"slow","name":"a","rule":[{"name":"x","port":81,"secret":null}],"settings":{"level":1,"version":"v1"},"token":null}`,
		},
		"removed nested attribute with configured values is null": {
			prior:  prior,
			config: `{"name":"a"}`,
			want:   `{"id":"1","member":[],"mode":"fast","name":"a","rule":[],"settings":null,"token":null}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := cty.NullVal(ty)
			if tc.prior != "" {
				p = decode(t, b, tc.prior)
			}
			got, err := ToJSON(ProposedNew(b, p, decode(t, b, tc.config)), ty)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("ProposedNew =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	if v := ProposedNew(b, decode(t, b, prior), cty.NullVal(ty)); !v.IsNull() {
		t.Errorf("ProposedNew of a null config = %#v", v)
	}
}

func TestToJSONUnknown(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{
		"id":    cty.String,
		"tags":  cty.Map(cty.String),
		"rules": cty.List(cty.Object(map[string]cty.Type{"port": cty.Number})),
		"ips":   cty.Set(cty.String),
		"name":  cty.String,
	})
	v := cty.ObjectVal(map[string]cty.Value{
		"id":    cty.UnknownVal(cty.String),
		"tags":  cty.MapVal(map[string]cty.Value{"env": cty.UnknownVal(cty.String), "k": cty.StringVal("v")}),
		"rules": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"port": cty.UnknownVal(cty.Number)})}),
		"ips":   cty.SetVal([]cty.Value{cty.StringVal("a"), cty.UnknownVal(cty.String)}),
		"name":  cty.StringVal("n"),
	})
	js, paths, err := ToJSONUnknown(v, ty)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":null,"ips":null,"name":"n","rules":[{"port":null}],"tags":{"env":null,"k":"v"}}`; string(js) != want {
		t.Errorf("json =\n%s\nwant\n%s", js, want)
	}
	if want := []string{"id", "ips", "rules[0].port", `tags["env"]`}; len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] || paths[2] != want[2] || paths[3] != want[3] {
		t.Errorf("paths = %q, want %q", paths, want)
	}

	// Through msgpack, as plans travel, unknowns survive.
	mp, err := ToMsgpack(v, ty)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromDynamic(mp, nil, ty)
	if err != nil || back.GetAttr("id").IsKnown() || !back.RawEquals(v) {
		t.Errorf("msgpack round trip = %#v, %v", back, err)
	}
}
