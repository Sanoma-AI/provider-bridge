// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// Scrubbed is the placeholder written over secret strings.
const Scrubbed = "<scrubbed>"

// scrubber replaces secrets in resource state while keeping its type, so a
// scrubbed fixture still parses against the schema's implied type.
type scrubber struct {
	// extra holds "<type>.<attr path>" entries from --scrub, e.g.
	// "stripe_webhook_endpoint.secret"; collection levels are not indexed
	// ("rules.name" covers every rule).
	extra map[string]bool
	hits  map[string]bool
}

func newScrubber(paths []string) *scrubber {
	s := &scrubber{extra: map[string]bool{}, hits: map[string]bool{}}
	for _, p := range paths {
		s.extra[p] = true
	}
	return s
}

// Reset forgets the hits so far; the recorder resets per resource.
func (s *scrubber) Reset() { s.hits = map[string]bool{} }

// Hits lists the paths that held a value and were scrubbed.
func (s *scrubber) Hits() []string {
	out := make([]string, 0, len(s.hits))
	for p := range s.hits {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// State scrubs every sensitive attribute of block b and every --scrub path
// in v; prefix is the resource type, or the path of a nested block.
func (s *scrubber) State(prefix string, b *schema.Block, v cty.Value) cty.Value {
	if v.IsNull() || !v.IsKnown() || !v.Type().IsObjectType() {
		return v
	}
	attrs := v.AsValueMap()
	if len(attrs) == 0 {
		return v
	}
	for name, a := range b.Attributes {
		p := prefix + "." + name
		av, ok := attrs[name]
		if !ok {
			continue
		}
		if a.Sensitive || s.extra[p] {
			if !av.IsNull() {
				attrs[name] = scrubLeaves(av)
				s.hits[p] = true
			}
			continue
		}
		if a.NestedType != nil {
			nested := &schema.Block{Attributes: a.NestedType.Attributes}
			attrs[name] = schema.MapObjects(av, func(o cty.Value) cty.Value { return s.State(p, nested, o) })
		}
	}
	for name, nb := range b.Blocks {
		p := prefix + "." + name
		bv, ok := attrs[name]
		if !ok {
			continue
		}
		if s.extra[p] {
			if !bv.IsNull() {
				attrs[name] = scrubLeaves(bv)
				s.hits[p] = true
			}
			continue
		}
		attrs[name] = schema.MapObjects(bv, func(o cty.Value) cty.Value { return s.State(p, nb.Block, o) })
	}
	return cty.ObjectVal(attrs)
}

// scrubLeaves replaces every known string inside v with Scrubbed.
func scrubLeaves(v cty.Value) cty.Value {
	out, err := cty.Transform(v, func(_ cty.Path, x cty.Value) (cty.Value, error) {
		if x.Type() == cty.String && x.IsKnown() && !x.IsNull() {
			return cty.StringVal(Scrubbed), nil
		}
		return x, nil
	})
	if err != nil {
		return cty.NullVal(v.Type())
	}
	return out
}

// replaceSecrets replaces each secret, raw and JSON-escaped, with a
// placeholder naming its environment variable.
func replaceSecrets(b []byte, secrets map[string]string) []byte {
	s := string(b)
	for env, secret := range secrets {
		for _, form := range secretForms(secret) {
			s = strings.ReplaceAll(s, form, "<scrubbed:"+env+">")
		}
	}
	return []byte(s)
}
