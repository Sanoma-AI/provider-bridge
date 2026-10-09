// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package values

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

var objTy = cty.Object(map[string]cty.Type{
	"id":      cty.String,
	"count":   cty.Number,
	"enabled": cty.Bool,
	"members": cty.Set(cty.String),
	"tags":    cty.Map(cty.String),
	"extra":   cty.DynamicPseudoType,
	"rules":   cty.List(cty.Object(map[string]cty.Type{"name": cty.String})),
})

func TestJSONRoundTrip(t *testing.T) {
	in := `{"count":12345678901234567890123,"enabled":null,"extra":{"value":{"a":1},"type":["object",{"a":"number"}]},"id":"x","members":["b","a"],"rules":[{"name":"r1"}],"tags":{"k":"v"}}`
	v, err := FromJSON([]byte(in), objTy)
	if err != nil {
		t.Fatal(err)
	}
	if !v.GetAttr("enabled").IsNull() {
		t.Error("null lost")
	}
	if got := v.GetAttr("count").AsBigFloat().Text('f', -1); got != "12345678901234567890123" {
		t.Errorf("big number = %s", got)
	}
	if !v.GetAttr("members").Type().IsSetType() {
		t.Error("set decoded as something else")
	}
	if !v.GetAttr("extra").Type().IsObjectType() {
		t.Errorf("dynamic value type = %#v", v.GetAttr("extra").Type())
	}

	out, err := ToJSON(v, objTy)
	if err != nil {
		t.Fatal(err)
	}
	// Sets come back sorted, numbers keep their digits, nulls stay.
	want := `{"count":12345678901234567890123,"enabled":null,"extra":{"value":{"a":1},"type":["object",{"a":"number"}]},"id":"x","members":["a","b"],"rules":[{"name":"r1"}],"tags":{"k":"v"}}`
	if string(out) != want {
		t.Errorf("ToJSON =\n%s\nwant\n%s", out, want)
	}

	// Through msgpack and back is lossless.
	mp, err := ToMsgpack(v, objTy)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromDynamic(mp, nil, objTy)
	if err != nil {
		t.Fatal(err)
	}
	if !back.RawEquals(v) {
		t.Errorf("msgpack round trip changed the value:\n%#v\n%#v", back, v)
	}
}

func TestOmittedAttributesAreNull(t *testing.T) {
	v, err := FromJSON([]byte(`{"id":"x"}`), objTy)
	if err != nil {
		t.Fatal(err)
	}
	for name := range objTy.AttributeTypes() {
		if name != "id" && !v.GetAttr(name).IsNull() {
			t.Errorf("%s = %#v, want null", name, v.GetAttr(name))
		}
	}
}

func TestFromJSONErrors(t *testing.T) {
	for in, want := range map[string]string{
		`{"nope":1}`:       "nope",
		`[1]`:              "",
		`{"count":"many"}`: "count",
	} {
		_, err := FromJSON([]byte(in), objTy)
		if err == nil {
			t.Errorf("FromJSON(%s) succeeded", in)
			continue
		}
		if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("FromJSON(%s) = %v, want mention of %q", in, err, want)
		}
	}
}

func TestEmptyIsNull(t *testing.T) {
	v, err := FromJSON(nil, objTy)
	if err != nil || !v.IsNull() {
		t.Fatalf("FromJSON(nil) = %#v, %v", v, err)
	}
	v, err = FromDynamic(nil, nil, objTy)
	if err != nil || !v.IsNull() {
		t.Fatalf("FromDynamic(nil, nil) = %#v, %v", v, err)
	}
	v, err = FromDynamic(nil, []byte(`{"id":"j"}`), objTy)
	if err != nil || v.GetAttr("id").AsString() != "j" {
		t.Fatalf("FromDynamic(json) = %#v, %v", v, err)
	}
}

func TestToJSONRejectsUnknown(t *testing.T) {
	v := cty.ObjectVal(map[string]cty.Value{"a": cty.UnknownVal(cty.String)})
	if _, err := ToJSON(v, v.Type()); err == nil {
		t.Fatal("unknown encoded")
	}
}

func TestFormatPath(t *testing.T) {
	p := cty.GetAttrPath("rules").Index(cty.NumberIntVal(0)).GetAttr("name")
	if got := FormatPath(p); got != "rules[0].name" {
		t.Errorf("FormatPath = %q", got)
	}
	p = cty.GetAttrPath("tags").Index(cty.StringVal("env"))
	if got := FormatPath(p); got != `tags["env"]` {
		t.Errorf("FormatPath = %q", got)
	}
}
