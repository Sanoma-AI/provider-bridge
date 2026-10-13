// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package schema is the bridge's protocol-neutral model of an OpenTofu
// provider schema and its JSON document (formatVersion 1).
//
// Plugin protocol 5 and 6 schemas both convert into this model (see
// internal/tfproto). The JSON document is what the bridge hands to the
// Sanoma engine: every attribute carries its cty type (ctyjson encoding), so
// a consumer can derive a value schema from `type` alone, while `nestedType`
// and `blocks` carry the per-child flags (required, computed, sensitive...).
// Object keys are sorted (struct fields are declared alphabetically by JSON
// name; maps are sorted by encoding/json).
package schema

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"
)

// FormatVersion is the version of the JSON document produced by Marshal.
const FormatVersion = 1

// Nesting is how a nested block or nested attribute repeats.
type Nesting string

// Nesting modes, as named by the plugin protocol.
const (
	NestingSingle Nesting = "single"
	NestingGroup  Nesting = "group"
	NestingList   Nesting = "list"
	NestingSet    Nesting = "set"
	NestingMap    Nesting = "map"
)

// ProviderSchema is a provider's full schema.
type ProviderSchema struct {
	DataSources    map[string]*Resource `json:"dataSources"`
	FormatVersion  int                  `json:"formatVersion"`
	Protocol       int                  `json:"protocol"`
	ProviderConfig *Block               `json:"providerConfig"`
	// ProviderMeta is the provider_meta block, for the few providers that
	// have one.
	ProviderMeta *Block               `json:"providerMeta,omitempty"`
	Resources    map[string]*Resource `json:"resources"`
	Source       string               `json:"source"`
	Version      string               `json:"version"`
}

// Resource is the schema of one resource type or data source.
type Resource struct {
	Block         *Block `json:"block"`
	SchemaVersion int64  `json:"schemaVersion"`
}

// Block is a set of attributes and nested blocks. A Block must not change
// once ImpliedType has been called: the type is computed once.
type Block struct {
	Attributes      map[string]*Attribute   `json:"attributes"`
	Blocks          map[string]*NestedBlock `json:"blocks"`
	Deprecated      bool                    `json:"deprecated,omitempty"`
	Description     string                  `json:"description,omitempty"`
	DescriptionKind string                  `json:"descriptionKind,omitempty"`

	implied     cty.Type
	impliedOnce sync.Once
}

// Attribute is one attribute of a block or nested type.
type Attribute struct {
	Computed        bool    `json:"computed,omitempty"`
	Deprecated      bool    `json:"deprecated,omitempty"`
	Description     string  `json:"description,omitempty"`
	DescriptionKind string  `json:"descriptionKind,omitempty"`
	NestedType      *Object `json:"nestedType,omitempty"`
	Optional        bool    `json:"optional,omitempty"`
	Required        bool    `json:"required,omitempty"`
	Sensitive       bool    `json:"sensitive,omitempty"`
	// Type is the attribute's cty type (ctyjson encoding in the document);
	// for a nested attribute it is the type implied by NestedType.
	Type      cty.Type `json:"type"`
	WriteOnly bool     `json:"writeOnly,omitempty"`
}

// Object is a nested attribute type (plugin protocol 6 only).
type Object struct {
	Attributes map[string]*Attribute `json:"attributes"`
	MaxItems   int64                 `json:"maxItems,omitempty"`
	MinItems   int64                 `json:"minItems,omitempty"`
	Nesting    Nesting               `json:"nesting"`
}

// NestedBlock is a block nested in another block.
type NestedBlock struct {
	Block    *Block  `json:"block"`
	MaxItems int64   `json:"maxItems,omitempty"`
	MinItems int64   `json:"minItems,omitempty"`
	Nesting  Nesting `json:"nesting"`
}

// Marshal encodes the schema document with two-space indentation. Every
// attribute must have a Type (see FillTypes).
func Marshal(s *ProviderSchema) ([]byte, error) {
	if err := s.checkTypes(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Unmarshal decodes a schema document produced by Marshal.
func Unmarshal(b []byte) (*ProviderSchema, error) {
	var s ProviderSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("schema formatVersion %d, want %d", s.FormatVersion, FormatVersion)
	}
	return &s, nil
}

// Resource returns the schema of a managed resource type.
func (s *ProviderSchema) Resource(typeName string) (*Resource, bool) {
	r, ok := s.Resources[typeName]
	return r, ok
}

// ImpliedType is the cty object type of a value conforming to b, computed
// once per Block.
//
// It follows OpenTofu's configschema: single and group blocks are objects,
// list blocks are lists (or DynamicPseudoType if the element type has
// dynamic parts, since list elements must share one type), set blocks are
// sets, and map blocks are maps (or DynamicPseudoType when dynamic).
func (b *Block) ImpliedType() cty.Type {
	if b == nil {
		return cty.EmptyObject
	}
	b.impliedOnce.Do(func() { b.implied = b.impliedType() })
	return b.implied
}

func (b *Block) impliedType() cty.Type {
	atys := make(map[string]cty.Type, len(b.Attributes)+len(b.Blocks))
	for name, a := range b.Attributes {
		atys[name] = a.ImpliedType()
	}
	for name, nb := range b.Blocks {
		atys[name] = nb.ImpliedType()
	}
	return cty.Object(atys)
}

// ImpliedType is the attribute's type: Type when set (FillTypes sets it on
// nested attributes too), else the nested type's.
func (a *Attribute) ImpliedType() cty.Type {
	if a.Type == cty.NilType && a.NestedType != nil {
		return a.NestedType.ImpliedType()
	}
	return a.Type
}

// ImpliedType of a nested attribute type.
func (o *Object) ImpliedType() cty.Type {
	atys := make(map[string]cty.Type, len(o.Attributes))
	for name, a := range o.Attributes {
		atys[name] = a.ImpliedType()
	}
	return wrap(o.Nesting, cty.Object(atys))
}

// ImpliedType of a nested block.
func (nb *NestedBlock) ImpliedType() cty.Type {
	return wrap(nb.Nesting, nb.Block.ImpliedType())
}

func wrap(n Nesting, ety cty.Type) cty.Type {
	switch n {
	case NestingList:
		if ety.HasDynamicTypes() {
			return cty.DynamicPseudoType
		}
		return cty.List(ety)
	case NestingSet:
		return cty.Set(ety)
	case NestingMap:
		if ety.HasDynamicTypes() {
			return cty.DynamicPseudoType
		}
		return cty.Map(ety)
	default: // single, group
		return ety
	}
}

// FillTypes sets Type on every nested attribute to its implied type, so the
// JSON document always carries `type`.
func (b *Block) FillTypes() {
	if b == nil {
		return
	}
	for _, a := range b.Attributes {
		a.fillTypes()
	}
	for _, nb := range b.Blocks {
		nb.Block.FillTypes()
	}
}

func (a *Attribute) fillTypes() {
	if a.NestedType == nil {
		return
	}
	for _, c := range a.NestedType.Attributes {
		c.fillTypes()
	}
	a.Type = a.NestedType.ImpliedType()
}

// checkTypes reports the first attribute without a type.
func (s *ProviderSchema) checkTypes() error {
	var check func(path string, attrs map[string]*Attribute, blocks map[string]*NestedBlock) error
	check = func(path string, attrs map[string]*Attribute, blocks map[string]*NestedBlock) error {
		for name, a := range attrs {
			if a.Type == cty.NilType {
				return fmt.Errorf("schema: attribute %s%s has no type", path, name)
			}
			if a.NestedType != nil {
				if err := check(path+name+".", a.NestedType.Attributes, nil); err != nil {
					return err
				}
			}
		}
		for name, nb := range blocks {
			if nb.Block != nil {
				if err := check(path+name+".", nb.Block.Attributes, nb.Block.Blocks); err != nil {
					return err
				}
			}
		}
		return nil
	}
	top := map[string]*Block{"provider.": s.ProviderConfig, "provider_meta.": s.ProviderMeta}
	for name, r := range s.Resources {
		top[name+"."] = r.Block
	}
	for name, r := range s.DataSources {
		top["data."+name+"."] = r.Block
	}
	for path, b := range top {
		if b != nil {
			if err := check(path, b.Attributes, b.Blocks); err != nil {
				return err
			}
		}
	}
	return nil
}

// FillTypes fills nested attribute types throughout the schema.
func (s *ProviderSchema) FillTypes() {
	s.ProviderConfig.FillTypes()
	s.ProviderMeta.FillTypes()
	for _, r := range s.Resources {
		r.Block.FillTypes()
	}
	for _, r := range s.DataSources {
		r.Block.FillTypes()
	}
}
