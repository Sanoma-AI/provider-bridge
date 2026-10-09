// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package values converts resource values between the JSON the bridge's API
// carries and the cty values and msgpack/JSON DynamicValue encodings the
// plugin protocol uses.
//
// JSON out is ctyjson: nulls are kept, sets become arrays, numbers keep full
// precision, and DynamicPseudoType attributes become {"value":..,"type":..}.
// JSON in is decoded against the implied type, so omitted attributes are
// null; unknown values cannot be expressed and are rejected.
package values

import (
	"errors"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"
)

// FromJSON decodes a JSON document of type ty. Empty input is a null value.
func FromJSON(b []byte, ty cty.Type) (cty.Value, error) {
	if len(b) == 0 {
		return cty.NullVal(ty), nil
	}
	v, err := ctyjson.Unmarshal(b, ty)
	if err != nil {
		return cty.NilVal, describe(err)
	}
	return v, nil
}

// ToJSON encodes v as JSON of type ty. Unknown values are an error.
func ToJSON(v cty.Value, ty cty.Type) ([]byte, error) {
	if !v.IsWhollyKnown() {
		return nil, errors.New("value contains unknowns")
	}
	b, err := ctyjson.Marshal(v, ty)
	if err != nil {
		return nil, describe(err)
	}
	return b, nil
}

// ToMsgpack encodes v for a DynamicValue's msgpack field.
func ToMsgpack(v cty.Value, ty cty.Type) ([]byte, error) {
	b, err := msgpack.Marshal(v, ty)
	if err != nil {
		return nil, describe(err)
	}
	return b, nil
}

// FromDynamic decodes a DynamicValue: msgpack when present, else JSON, else
// null (a provider returns an empty DynamicValue for "no object").
func FromDynamic(mp, js []byte, ty cty.Type) (cty.Value, error) {
	switch {
	case len(mp) > 0:
		v, err := msgpack.Unmarshal(mp, ty)
		if err != nil {
			return cty.NilVal, describe(err)
		}
		return v, nil
	case len(js) > 0:
		return FromJSON(js, ty)
	default:
		return cty.NullVal(ty), nil
	}
}

// describe adds the cty path, when there is one, to an error message.
func describe(err error) error {
	var pe cty.PathError
	if errors.As(err, &pe) && len(pe.Path) > 0 {
		return fmt.Errorf("%s: %w", FormatPath(pe.Path), err)
	}
	return err
}

// FormatPath renders a cty path as in OpenTofu diagnostics: a.b[0]["k"].
func FormatPath(p cty.Path) string {
	var s string
	for _, step := range p {
		switch st := step.(type) {
		case cty.GetAttrStep:
			if s != "" {
				s += "."
			}
			s += st.Name
		case cty.IndexStep:
			switch {
			case st.Key.IsNull() || !st.Key.IsKnown():
				s += "[?]"
			case st.Key.Type() == cty.String:
				s += fmt.Sprintf("[%q]", st.Key.AsString())
			case st.Key.Type() == cty.Number:
				s += "[" + st.Key.AsBigFloat().Text('f', -1) + "]"
			default:
				s += "[?]"
			}
		}
	}
	return s
}
