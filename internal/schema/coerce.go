// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"errors"
	"fmt"

	"github.com/zclconf/go-cty/cty"
)

// CoerceConfig normalizes v, a value of b's implied type, the way OpenTofu
// core does before handing configuration or state to a provider: a null
// list, set or map of nested blocks becomes an empty collection, and a null
// group block becomes an object of nulls. SDKv2 providers reject the null
// collections otherwise. v must be wholly known and conform to b's implied
// type: a DynamicPseudoType attribute holds a value of a concrete type.
func CoerceConfig(b *Block, v cty.Value) (cty.Value, error) {
	if !v.IsWhollyKnown() {
		return cty.NilVal, fmt.Errorf("value contains unknowns")
	}
	if errs := v.Type().TestConformance(b.ImpliedType()); len(errs) > 0 {
		return cty.NilVal, fmt.Errorf("value does not conform to the schema: %w", errors.Join(errs...))
	}
	return b.coerce(v), nil
}

func (b *Block) coerce(v cty.Value) cty.Value {
	if v.IsNull() || len(b.Blocks) == 0 {
		return v
	}
	attrs := v.AsValueMap()
	for name, nb := range b.Blocks {
		attrs[name] = nb.coerce(attrs[name])
	}
	return cty.ObjectVal(attrs)
}

func (nb *NestedBlock) coerce(v cty.Value) cty.Value {
	ty := nb.ImpliedType()
	switch nb.Nesting {
	case NestingSingle:
		return nb.Block.coerce(v)
	case NestingGroup:
		if v.IsNull() {
			v = nullObject(nb.Block)
		}
		return nb.Block.coerce(v)
	}
	if ty == cty.DynamicPseudoType {
		return v
	}
	if v.IsNull() {
		switch {
		case ty.IsListType():
			return cty.ListValEmpty(ty.ElementType())
		case ty.IsSetType():
			return cty.SetValEmpty(ty.ElementType())
		case ty.IsMapType():
			return cty.MapValEmpty(ty.ElementType())
		}
		return v
	}
	if v.LengthInt() == 0 || len(nb.Block.Blocks) == 0 {
		return v
	}
	switch {
	case ty.IsListType():
		elems := v.AsValueSlice()
		for i, e := range elems {
			elems[i] = nb.Block.coerce(e)
		}
		return cty.ListVal(elems)
	case ty.IsSetType():
		elems := v.AsValueSlice()
		for i, e := range elems {
			elems[i] = nb.Block.coerce(e)
		}
		return cty.SetVal(elems)
	case ty.IsMapType():
		elems := v.AsValueMap()
		for k, e := range elems {
			elems[k] = nb.Block.coerce(e)
		}
		return cty.MapVal(elems)
	}
	return v
}

func nullObject(b *Block) cty.Value {
	ty := b.ImpliedType()
	if len(ty.AttributeTypes()) == 0 {
		return cty.EmptyObjectVal
	}
	attrs := make(map[string]cty.Value, len(ty.AttributeTypes()))
	for name, aty := range ty.AttributeTypes() {
		attrs[name] = cty.NullVal(aty)
	}
	return cty.ObjectVal(attrs)
}
