// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package values

import (
	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// ProposedNew is the proposed new state PlanResourceChange receives: config
// laid over prior, following OpenTofu's objchange.ProposedNew (internal to
// OpenTofu, so rewritten here). A non-computed attribute takes its config
// value; a computed attribute absent from config keeps its prior value (null
// on create: the provider marks it unknown when it plans); a write-only
// attribute is null. Nested blocks and nested attributes are matched to
// their prior counterparts by index (list), key (map) or by their configured
// attributes (set), and merged the same way.
//
// prior is null for a create; config must be non-null and wholly known, as
// the bridge's configs always are. Both are of b's implied type.
func ProposedNew(b *schema.Block, prior, config cty.Value) cty.Value {
	if config.IsNull() {
		return config
	}
	return proposeBlock(b, prior, config)
}

func proposeBlock(b *schema.Block, prior, config cty.Value) cty.Value {
	if config.IsNull() {
		return prior
	}
	out := proposeAttributes(b.Attributes, prior, config)
	for name, nb := range b.Blocks {
		priorV, configV := attr(prior, name), config.GetAttr(name)
		elem := func(p, c cty.Value) cty.Value { return proposeBlock(nb.Block, p, c) }
		match := func(p, c cty.Value) bool { return blockMatches(nb.Block, p, c) }
		switch nb.Nesting {
		case schema.NestingSingle:
			if !configV.IsNull() {
				configV = proposeBlock(nb.Block, priorV, configV)
			}
			out[name] = configV
		case schema.NestingGroup:
			out[name] = proposeBlock(nb.Block, priorV, configV)
		default:
			out[name] = proposeCollection(nb.Nesting, priorV, configV, elem, match)
		}
	}
	return cty.ObjectVal(out)
}

func proposeAttributes(attrs map[string]*schema.Attribute, prior, config cty.Value) map[string]cty.Value {
	out := make(map[string]cty.Value, len(attrs))
	for name, a := range attrs {
		priorV, configV := attr(prior, name), config.GetAttr(name)
		var v cty.Value
		switch {
		case a.WriteOnly:
			v = cty.NullVal(configV.Type())
		case a.Computed && configV.IsNull():
			v = priorV
			// An optional nested attribute whose prior value holds
			// non-computed values came from config, now removed.
			if a.Optional && a.NestedType != nil && hasConfigured(a.NestedType, priorV) {
				v = configV
			}
		case a.NestedType != nil:
			v = proposeNested(a.NestedType, priorV, configV)
		default:
			v = configV
		}
		out[name] = v
	}
	return out
}

func proposeNested(o *schema.Object, prior, config cty.Value) cty.Value {
	elem := func(p, c cty.Value) cty.Value {
		if c.IsNull() {
			return c
		}
		return cty.ObjectVal(proposeAttributes(o.Attributes, p, c))
	}
	if o.Nesting == schema.NestingSingle {
		return elem(prior, config)
	}
	match := func(p, c cty.Value) bool { return attributesMatch(o.Attributes, p, c) }
	return proposeCollection(o.Nesting, prior, config, elem, match)
}

// proposeCollection merges a list, map or set of nested objects element by
// element; an element without a prior counterpart is taken from config.
// Lists and maps of dynamically typed objects are tuples and objects.
func proposeCollection(n schema.Nesting, prior, config cty.Value, elem func(p, c cty.Value) cty.Value, match func(p, c cty.Value) bool) cty.Value {
	if config.IsNull() || config.LengthInt() == 0 {
		return config
	}
	ty := config.Type()
	switch n {
	case schema.NestingList:
		elems := config.AsValueSlice()
		for i, c := range elems {
			if !prior.IsNull() && i < prior.LengthInt() {
				elems[i] = elem(prior.Index(cty.NumberIntVal(int64(i))), c)
			}
		}
		if ty.IsTupleType() {
			return cty.TupleVal(elems)
		}
		return cty.ListVal(elems)
	case schema.NestingMap:
		elems := config.AsValueMap()
		for k, c := range elems {
			if p, ok := element(prior, k); ok {
				elems[k] = elem(p, c)
			}
		}
		if ty.IsObjectType() {
			return cty.ObjectVal(elems)
		}
		return cty.MapVal(elems)
	case schema.NestingSet:
		var priors []cty.Value
		if !prior.IsNull() {
			priors = prior.AsValueSlice()
		}
		used := make([]bool, len(priors))
		elems := config.AsValueSlice()
		for i, c := range elems {
			p := cty.NullVal(ty.ElementType())
			for j, cand := range priors {
				if !used[j] && match(cand, c) {
					p, used[j] = cand, true
					break
				}
			}
			elems[i] = elem(p, c)
		}
		return cty.SetVal(elems)
	}
	return config
}

// blockMatches reports whether prior can be the prior state of config, a
// set element: every value config decides is equal in both.
func blockMatches(b *schema.Block, prior, config cty.Value) bool {
	if prior.IsNull() || config.IsNull() {
		return prior.IsNull() == config.IsNull()
	}
	if !attributesMatch(b.Attributes, prior, config) {
		return false
	}
	for name, nb := range b.Blocks {
		match := func(p, c cty.Value) bool { return blockMatches(nb.Block, p, c) }
		if !collectionMatches(nb.Nesting, prior.GetAttr(name), config.GetAttr(name), match) {
			return false
		}
	}
	return true
}

func attributesMatch(attrs map[string]*schema.Attribute, prior, config cty.Value) bool {
	if prior.IsNull() || config.IsNull() {
		return prior.IsNull() == config.IsNull()
	}
	for name, a := range attrs {
		p, c := prior.GetAttr(name), config.GetAttr(name)
		switch {
		case a.WriteOnly, a.Computed && c.IsNull():
			// Not decided by config.
		case a.NestedType != nil:
			o := a.NestedType
			match := func(p, c cty.Value) bool { return attributesMatch(o.Attributes, p, c) }
			if !collectionMatches(o.Nesting, p, c, match) {
				return false
			}
		case !p.RawEquals(c):
			return false
		}
	}
	return true
}

// collectionMatches applies match to a nested value: the object itself
// (single, group), or the elements of a list (by index), map (by key) or set
// (each config element matches a distinct prior one).
func collectionMatches(n schema.Nesting, prior, config cty.Value, match func(p, c cty.Value) bool) bool {
	if n == schema.NestingSingle || n == schema.NestingGroup {
		return match(prior, config)
	}
	if prior.IsNull() || config.IsNull() {
		return prior.IsNull() == config.IsNull()
	}
	if prior.LengthInt() != config.LengthInt() {
		return false
	}
	switch n {
	case schema.NestingList:
		ps, cs := prior.AsValueSlice(), config.AsValueSlice()
		for i := range cs {
			if !match(ps[i], cs[i]) {
				return false
			}
		}
	case schema.NestingMap:
		for k, c := range config.AsValueMap() {
			p, ok := element(prior, k)
			if !ok || !match(p, c) {
				return false
			}
		}
	case schema.NestingSet:
		ps := prior.AsValueSlice()
		used := make([]bool, len(ps))
	next:
		for _, c := range config.AsValueSlice() {
			for j, p := range ps {
				if !used[j] && match(p, c) {
					used[j] = true
					continue next
				}
			}
			return false
		}
	}
	return true
}

// hasConfigured reports whether v, a value of o's type, holds a non-null
// value for a non-computed attribute at any depth.
func hasConfigured(o *schema.Object, v cty.Value) bool {
	found := false
	for _, e := range objects(o.Nesting, v) {
		for name, a := range o.Attributes {
			c := e.GetAttr(name)
			switch {
			case c.IsNull():
			case !a.Computed:
				found = true
			case a.NestedType != nil:
				found = hasConfigured(a.NestedType, c)
			}
			if found {
				return true
			}
		}
	}
	return false
}

// objects returns the non-null objects in a nested attribute's value.
func objects(n schema.Nesting, v cty.Value) []cty.Value {
	if v.IsNull() {
		return nil
	}
	if n == schema.NestingSingle {
		return []cty.Value{v}
	}
	var out []cty.Value
	for it := v.ElementIterator(); it.Next(); {
		if _, e := it.Element(); !e.IsNull() {
			out = append(out, e)
		}
	}
	return out
}

// attr returns v's attribute name, null when v is null.
func attr(v cty.Value, name string) cty.Value {
	if v.IsNull() {
		return cty.NullVal(v.Type().AttributeType(name))
	}
	return v.GetAttr(name)
}

// element returns the element of a map (or, when dynamically typed, an
// object) at key.
func element(v cty.Value, key string) (cty.Value, bool) {
	switch {
	case v.IsNull():
		return cty.NilVal, false
	case v.Type().IsObjectType():
		if !v.Type().HasAttribute(key) {
			return cty.NilVal, false
		}
		return v.GetAttr(key), true
	}
	k := cty.StringVal(key)
	if !v.HasIndex(k).True() {
		return cty.NilVal, false
	}
	return v.Index(k), true
}
