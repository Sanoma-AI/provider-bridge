// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package values

import (
	"slices"

	"github.com/zclconf/go-cty/cty"
)

// ToJSONUnknown encodes v, which may hold unknown values (a planned
// state), as JSON of type ty with every unknown value as null, and returns
// the paths of those values in FormatPath form, sorted. A set holding an
// unknown is null and listed as a whole: its elements have no stable
// position to name.
func ToJSONUnknown(v cty.Value, ty cty.Type) ([]byte, []string, error) {
	var paths []string
	known := nullUnknowns(v, nil, &paths)
	b, err := ToJSON(known, ty)
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(paths)
	return b, paths, nil
}

func nullUnknowns(v cty.Value, path cty.Path, paths *[]string) cty.Value {
	if v.IsWhollyKnown() {
		return v
	}
	ty := v.Type()
	if !v.IsKnown() || ty.IsSetType() {
		*paths = append(*paths, FormatPath(path))
		return cty.NullVal(ty)
	}
	switch {
	case ty.IsObjectType():
		m := v.AsValueMap()
		for k, e := range m {
			m[k] = nullUnknowns(e, path.GetAttr(k), paths)
		}
		return cty.ObjectVal(m)
	case ty.IsMapType():
		m := v.AsValueMap()
		for k, e := range m {
			m[k] = nullUnknowns(e, path.Index(cty.StringVal(k)), paths)
		}
		return cty.MapVal(m)
	case ty.IsListType(), ty.IsTupleType():
		s := v.AsValueSlice()
		for i, e := range s {
			s[i] = nullUnknowns(e, path.Index(cty.NumberIntVal(int64(i))), paths)
		}
		if ty.IsTupleType() {
			return cty.TupleVal(s)
		}
		return cty.ListVal(s)
	}
	return v
}
