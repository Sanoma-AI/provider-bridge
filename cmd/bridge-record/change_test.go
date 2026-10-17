// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

func TestParseChanges(t *testing.T) {
	got, err := parseChanges([]string{`a_x=one={"n":1}`, `a_x=two={}`, `a_x=one={"n":2}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].slug != "one" || len(got[0].configs) != 2 || got[0].configs[1] != `{"n":2}` || got[1].slug != "two" {
		t.Fatalf("parseChanges = %+v", got)
	}
	for _, bad := range []string{`a_x={}`, `a_x=one=`, `a_x=one={`, `a_x=o/ne={}`, `=one={}`} {
		if _, err := parseChanges([]string{bad}); err == nil {
			t.Errorf("parseChanges(%q) succeeded", bad)
		}
	}
}

// A plan is scrubbed like state, unknown values kept.
func TestScrubPlanned(t *testing.T) {
	s := testutil.FakeSchema()
	r := &recorder{schema: s, scrubber: newScrubber(nil)}
	ty := s.Resources["fake_thing"].Block.ImpliedType()
	plan := cty.ObjectVal(map[string]cty.Value{"id": cty.UnknownVal(cty.String), "name": cty.StringVal("n"), "secret": cty.StringVal("hunter2")})
	mp, err := values.ToMsgpack(plan, ty)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.scrubPlanned("fake_thing", mp)
	if err != nil {
		t.Fatal(err)
	}
	v, err := values.FromDynamic(out, nil, ty)
	if err != nil || v.GetAttr("id").IsKnown() || v.GetAttr("secret").AsString() != Scrubbed || v.GetAttr("name").AsString() != "n" {
		t.Fatalf("scrubbed plan = %#v, %v", v, err)
	}
}
