// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package tfproto

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	"github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// Protocol 5 and 6 messages are structurally alike but distinct Go types.
// Diagnostics are wire-identical, so protocol 6 ones are re-read as protocol
// 5 and converted once; schemas differ (protocol 6 adds nested attribute
// types), so blocks convert per protocol under one generic driver.

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

// schemaMsg is a protocol's Schema message (*tfplugin5.Schema,
// *tfplugin6.Schema) with its block type B.
type schemaMsg[B any] interface {
	GetVersion() int64
	GetBlock() B
}

// providerSchema converts a GetProviderSchema response's parts; meta is
// the provider_meta block, the zero B when there is none.
func providerSchema[S schemaMsg[B], B comparable](protocol int, provider, meta B, resources, dataSources map[string]S, block func(B) (*schema.Block, error)) (*schema.ProviderSchema, error) {
	s := &schema.ProviderSchema{FormatVersion: schema.FormatVersion, Protocol: protocol}
	var err error
	if s.ProviderConfig, err = block(provider); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	var none B
	if meta != none {
		if s.ProviderMeta, err = block(meta); err != nil {
			return nil, fmt.Errorf("provider_meta: %w", err)
		}
	}
	if s.Resources, err = convertResources("resource", resources, block); err != nil {
		return nil, err
	}
	if s.DataSources, err = convertResources("data source", dataSources, block); err != nil {
		return nil, err
	}
	s.FillTypes()
	return s, nil
}

func convertResources[S schemaMsg[B], B any](kind string, in map[string]S, block func(B) (*schema.Block, error)) (map[string]*schema.Resource, error) {
	out := make(map[string]*schema.Resource, len(in))
	for name, rs := range in {
		b, err := block(rs.GetBlock())
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", kind, name, err)
		}
		out[name] = &schema.Resource{SchemaVersion: rs.GetVersion(), Block: b}
	}
	return out, nil
}

// ---- protocol 5 ----

func providerSchema5(resp *tfplugin5.GetProviderSchema_Response) (*schema.ProviderSchema, error) {
	return providerSchema(5, resp.GetProvider().GetBlock(), resp.GetProviderMeta().GetBlock(),
		resp.GetResourceSchemas(), resp.GetDataSourceSchemas(), block5)
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

// ---- protocol 6 ----

func providerSchema6(resp *tfplugin6.GetProviderSchema_Response) (*schema.ProviderSchema, error) {
	return providerSchema(6, resp.GetProvider().GetBlock(), resp.GetProviderMeta().GetBlock(),
		resp.GetResourceSchemas(), resp.GetDataSourceSchemas(), block6)
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
			a.NestedType = &schema.Object{
				Nesting:    n,
				Attributes: children,
				// Deprecated in tfplugin6 (OpenTofu ignores them) but still
				// sent by some providers; kept for the schema document.
				MinItems: nt.GetMinItems(), //nolint:staticcheck // see above
				MaxItems: nt.GetMaxItems(), //nolint:staticcheck // see above
			}
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

// diagnostics6 converts protocol 6 diagnostics by re-reading each as its
// wire-identical protocol 5 twin.
func diagnostics6(in []*tfplugin6.Diagnostic) Diagnostics {
	twins := make([]*tfplugin5.Diagnostic, len(in))
	for i, d := range in {
		twins[i] = &tfplugin5.Diagnostic{}
		b, err := proto.Marshal(d)
		if err == nil {
			err = proto.Unmarshal(b, twins[i])
		}
		if err != nil {
			twins[i] = &tfplugin5.Diagnostic{Severity: tfplugin5.Diagnostic_ERROR, Summary: d.GetSummary(), Detail: d.GetDetail()}
		}
	}
	return diagnostics(twins)
}

// ---- shared ----

// diagnostics converts protocol 5 diagnostics.
func diagnostics(in []*tfplugin5.Diagnostic) Diagnostics {
	var out Diagnostics
	for _, d := range in {
		out = append(out, Diagnostic{
			Severity:  severity(d.GetSeverity()),
			Summary:   d.GetSummary(),
			Detail:    d.GetDetail(),
			Attribute: attributePath(d.GetAttribute()),
		})
	}
	return out
}

// attributePath renders a diagnostic's path as OpenTofu does, e.g.
// rules[0].name or tags["env"].
func attributePath(p *tfplugin5.AttributePath) string {
	var path cty.Path
	for _, st := range p.GetSteps() {
		switch sel := st.GetSelector().(type) {
		case *tfplugin5.AttributePath_Step_AttributeName:
			path = path.GetAttr(sel.AttributeName)
		case *tfplugin5.AttributePath_Step_ElementKeyString:
			path = path.Index(cty.StringVal(sel.ElementKeyString))
		case *tfplugin5.AttributePath_Step_ElementKeyInt:
			path = path.Index(cty.NumberIntVal(sel.ElementKeyInt))
		}
	}
	return values.FormatPath(path)
}

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

func severity(s tfplugin5.Diagnostic_Severity) Severity {
	if s == tfplugin5.Diagnostic_WARNING {
		return SeverityWarning
	}
	// INVALID is treated as an error, as OpenTofu core does.
	return SeverityError
}
