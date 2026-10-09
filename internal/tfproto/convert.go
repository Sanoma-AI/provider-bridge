// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"fmt"
	"strconv"

	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// Protocol 5 and 6 messages are structurally alike but distinct Go types,
// so each conversion exists twice. Protocol 6 adds nested attribute types.

func descriptionKind(markdown bool, description string) string {
	switch {
	case description == "":
		return ""
	case markdown:
		return "markdown"
	default:
		return "plain"
	}
}

func attrType(name string, raw []byte) (schema.Type, error) {
	ty, err := ctyjson.UnmarshalType(raw)
	if err != nil {
		return schema.Type{}, fmt.Errorf("attribute %s: invalid type %q: %w", name, raw, err)
	}
	return schema.Type{Type: ty}, nil
}

// ---- protocol 5 ----

func providerSchema5(resp *tfplugin5.GetProviderSchema_Response) (*schema.ProviderSchema, error) {
	s := &schema.ProviderSchema{
		FormatVersion: schema.FormatVersion,
		Protocol:      5,
		Resources:     map[string]*schema.Resource{},
		DataSources:   map[string]*schema.Resource{},
	}
	var err error
	if s.ProviderConfig, err = block5(resp.GetProvider().GetBlock()); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	for name, rs := range resp.GetResourceSchemas() {
		if s.Resources[name], err = resource5(rs); err != nil {
			return nil, fmt.Errorf("resource %s: %w", name, err)
		}
	}
	for name, rs := range resp.GetDataSourceSchemas() {
		if s.DataSources[name], err = resource5(rs); err != nil {
			return nil, fmt.Errorf("data source %s: %w", name, err)
		}
	}
	s.FillTypes()
	return s, nil
}

func resource5(rs *tfplugin5.Schema) (*schema.Resource, error) {
	b, err := block5(rs.GetBlock())
	if err != nil {
		return nil, err
	}
	return &schema.Resource{SchemaVersion: rs.GetVersion(), Block: b}, nil
}

func block5(pb *tfplugin5.Schema_Block) (*schema.Block, error) {
	b := &schema.Block{Attributes: map[string]*schema.Attribute{}, Blocks: map[string]*schema.NestedBlock{}}
	if pb == nil {
		return b, nil
	}
	b.Deprecated = pb.GetDeprecated()
	b.Description = pb.GetDescription()
	b.DescriptionKind = descriptionKind(pb.GetDescriptionKind() == tfplugin5.StringKind_MARKDOWN, b.Description)
	for _, pa := range pb.GetAttributes() {
		ty, err := attrType(pa.GetName(), pa.GetType())
		if err != nil {
			return nil, err
		}
		b.Attributes[pa.GetName()] = &schema.Attribute{
			Type:            ty,
			Required:        pa.GetRequired(),
			Optional:        pa.GetOptional(),
			Computed:        pa.GetComputed(),
			Sensitive:       pa.GetSensitive(),
			WriteOnly:       pa.GetWriteOnly(),
			Deprecated:      pa.GetDeprecated(),
			Description:     pa.GetDescription(),
			DescriptionKind: descriptionKind(pa.GetDescriptionKind() == tfplugin5.StringKind_MARKDOWN, pa.GetDescription()),
		}
	}
	for _, pn := range pb.GetBlockTypes() {
		nested, err := block5(pn.GetBlock())
		if err != nil {
			return nil, fmt.Errorf("block %s: %w", pn.GetTypeName(), err)
		}
		n, err := blockNesting(int32(pn.GetNesting()))
		if err != nil {
			return nil, fmt.Errorf("block %s: %w", pn.GetTypeName(), err)
		}
		b.Blocks[pn.GetTypeName()] = &schema.NestedBlock{Nesting: n, MinItems: pn.GetMinItems(), MaxItems: pn.GetMaxItems(), Block: nested}
	}
	return b, nil
}

func diagnostics5(in []*tfplugin5.Diagnostic) Diagnostics {
	var out Diagnostics
	for _, d := range in {
		var path string
		for _, st := range d.GetAttribute().GetSteps() {
			switch sel := st.GetSelector().(type) {
			case *tfplugin5.AttributePath_Step_AttributeName:
				path = joinAttr(path, sel.AttributeName)
			case *tfplugin5.AttributePath_Step_ElementKeyString:
				path += "[" + strconv.Quote(sel.ElementKeyString) + "]"
			case *tfplugin5.AttributePath_Step_ElementKeyInt:
				path += "[" + strconv.FormatInt(sel.ElementKeyInt, 10) + "]"
			}
		}
		out = append(out, Diagnostic{
			Severity:  severity(int32(d.GetSeverity())),
			Summary:   d.GetSummary(),
			Detail:    d.GetDetail(),
			Attribute: path,
		})
	}
	return out
}

// ---- protocol 6 ----

func providerSchema6(resp *tfplugin6.GetProviderSchema_Response) (*schema.ProviderSchema, error) {
	s := &schema.ProviderSchema{
		FormatVersion: schema.FormatVersion,
		Protocol:      6,
		Resources:     map[string]*schema.Resource{},
		DataSources:   map[string]*schema.Resource{},
	}
	var err error
	if s.ProviderConfig, err = block6(resp.GetProvider().GetBlock()); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	for name, rs := range resp.GetResourceSchemas() {
		if s.Resources[name], err = resource6(rs); err != nil {
			return nil, fmt.Errorf("resource %s: %w", name, err)
		}
	}
	for name, rs := range resp.GetDataSourceSchemas() {
		if s.DataSources[name], err = resource6(rs); err != nil {
			return nil, fmt.Errorf("data source %s: %w", name, err)
		}
	}
	s.FillTypes()
	return s, nil
}

func resource6(rs *tfplugin6.Schema) (*schema.Resource, error) {
	b, err := block6(rs.GetBlock())
	if err != nil {
		return nil, err
	}
	return &schema.Resource{SchemaVersion: rs.GetVersion(), Block: b}, nil
}

func block6(pb *tfplugin6.Schema_Block) (*schema.Block, error) {
	b := &schema.Block{Attributes: map[string]*schema.Attribute{}, Blocks: map[string]*schema.NestedBlock{}}
	if pb == nil {
		return b, nil
	}
	b.Deprecated = pb.GetDeprecated()
	b.Description = pb.GetDescription()
	b.DescriptionKind = descriptionKind(pb.GetDescriptionKind() == tfplugin6.StringKind_MARKDOWN, b.Description)
	attrs, err := attributes6(pb.GetAttributes())
	if err != nil {
		return nil, err
	}
	b.Attributes = attrs
	for _, pn := range pb.GetBlockTypes() {
		nested, err := block6(pn.GetBlock())
		if err != nil {
			return nil, fmt.Errorf("block %s: %w", pn.GetTypeName(), err)
		}
		n, err := blockNesting(int32(pn.GetNesting()))
		if err != nil {
			return nil, fmt.Errorf("block %s: %w", pn.GetTypeName(), err)
		}
		b.Blocks[pn.GetTypeName()] = &schema.NestedBlock{Nesting: n, MinItems: pn.GetMinItems(), MaxItems: pn.GetMaxItems(), Block: nested}
	}
	return b, nil
}

func attributes6(in []*tfplugin6.Schema_Attribute) (map[string]*schema.Attribute, error) {
	out := make(map[string]*schema.Attribute, len(in))
	for _, pa := range in {
		a := &schema.Attribute{
			Required:        pa.GetRequired(),
			Optional:        pa.GetOptional(),
			Computed:        pa.GetComputed(),
			Sensitive:       pa.GetSensitive(),
			WriteOnly:       pa.GetWriteOnly(),
			Deprecated:      pa.GetDeprecated(),
			Description:     pa.GetDescription(),
			DescriptionKind: descriptionKind(pa.GetDescriptionKind() == tfplugin6.StringKind_MARKDOWN, pa.GetDescription()),
		}
		if nt := pa.GetNestedType(); nt != nil {
			children, err := attributes6(nt.GetAttributes())
			if err != nil {
				return nil, fmt.Errorf("attribute %s: %w", pa.GetName(), err)
			}
			n, err := objectNesting(nt.GetNesting())
			if err != nil {
				return nil, fmt.Errorf("attribute %s: %w", pa.GetName(), err)
			}
			a.NestedType = &schema.Object{Nesting: n, Attributes: children, MinItems: nt.GetMinItems(), MaxItems: nt.GetMaxItems()}
		} else {
			ty, err := attrType(pa.GetName(), pa.GetType())
			if err != nil {
				return nil, err
			}
			a.Type = ty
		}
		out[pa.GetName()] = a
	}
	return out, nil
}

func objectNesting(n tfplugin6.Schema_Object_NestingMode) (schema.Nesting, error) {
	switch n {
	case tfplugin6.Schema_Object_SINGLE:
		return schema.NestingSingle, nil
	case tfplugin6.Schema_Object_LIST:
		return schema.NestingList, nil
	case tfplugin6.Schema_Object_SET:
		return schema.NestingSet, nil
	case tfplugin6.Schema_Object_MAP:
		return schema.NestingMap, nil
	}
	return "", fmt.Errorf("invalid nested type nesting %v", n)
}

func diagnostics6(in []*tfplugin6.Diagnostic) Diagnostics {
	var out Diagnostics
	for _, d := range in {
		var path string
		for _, st := range d.GetAttribute().GetSteps() {
			switch sel := st.GetSelector().(type) {
			case *tfplugin6.AttributePath_Step_AttributeName:
				path = joinAttr(path, sel.AttributeName)
			case *tfplugin6.AttributePath_Step_ElementKeyString:
				path += "[" + strconv.Quote(sel.ElementKeyString) + "]"
			case *tfplugin6.AttributePath_Step_ElementKeyInt:
				path += "[" + strconv.FormatInt(sel.ElementKeyInt, 10) + "]"
			}
		}
		out = append(out, Diagnostic{
			Severity:  severity(int32(d.GetSeverity())),
			Summary:   d.GetSummary(),
			Detail:    d.GetDetail(),
			Attribute: path,
		})
	}
	return out
}

// ---- shared ----

// blockNesting maps the NestedBlock nesting enum, numbered alike in 5 and 6.
func blockNesting(n int32) (schema.Nesting, error) {
	switch n {
	case int32(tfplugin5.Schema_NestedBlock_SINGLE):
		return schema.NestingSingle, nil
	case int32(tfplugin5.Schema_NestedBlock_LIST):
		return schema.NestingList, nil
	case int32(tfplugin5.Schema_NestedBlock_SET):
		return schema.NestingSet, nil
	case int32(tfplugin5.Schema_NestedBlock_MAP):
		return schema.NestingMap, nil
	case int32(tfplugin5.Schema_NestedBlock_GROUP):
		return schema.NestingGroup, nil
	}
	return "", fmt.Errorf("invalid block nesting %d", n)
}

// severity maps the Diagnostic severity enum, numbered alike in 5 and 6.
func severity(s int32) Severity {
	if s == int32(tfplugin5.Diagnostic_WARNING) {
		return SeverityWarning
	}
	// INVALID is treated as an error, as OpenTofu core does.
	return SeverityError
}

func joinAttr(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
