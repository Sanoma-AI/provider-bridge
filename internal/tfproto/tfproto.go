// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package tfproto is the bridge's client side of the OpenTofu plugin
// protocol. Client5 and Client6 wrap the generated gRPC stubs of protocol 5
// and 6 behind one Provider interface carrying the calls the bridge needs,
// with schemas in the protocol-neutral internal/schema model and values as
// cty.
//
// Each client fetches the provider schema once and caches the implied types
// it needs to encode and decode DynamicValues.
package tfproto

import (
	"context"
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// TerraformVersion is the fixed terraform_version sent with Configure.
// Providers use it in user agents and for feature checks.
const TerraformVersion = "1.10.0"

// Provider is one running provider process, protocol 5 or 6.
type Provider interface {
	// Protocol is the negotiated plugin protocol major version.
	Protocol() int
	// GetSchema returns the provider's schema (Source and Version unset).
	GetSchema(ctx context.Context) (*schema.ProviderSchema, Diagnostics, error)
	// PrepareConfig validates provider configuration: PrepareProviderConfig
	// in protocol 5 (which may return an amended config), ValidateProviderConfig
	// in protocol 6 (which returns cfg unchanged).
	PrepareConfig(ctx context.Context, cfg cty.Value) (cty.Value, Diagnostics, error)
	// Configure configures the provider; it must be called once, before
	// ImportResourceState and ReadResource.
	Configure(ctx context.Context, cfg cty.Value, terraformVersion string) (Diagnostics, error)
	ImportResourceState(ctx context.Context, typeName, id string) ([]ImportedResource, Diagnostics, error)
	// UpgradeResourceState converts rawJSON, state written with schema
	// version, into a value of the resource type's current implied type.
	UpgradeResourceState(ctx context.Context, typeName string, version int64, rawJSON []byte) (cty.Value, Diagnostics, error)
	// ReadResource refreshes state; a null new state means the object is gone.
	ReadResource(ctx context.Context, typeName string, state cty.Value, private []byte) (cty.Value, []byte, Diagnostics, error)
	// Stop asks the provider to cancel in-flight calls.
	Stop(ctx context.Context) error
}

// ImportedResource is one object returned by ImportResourceState.
type ImportedResource struct {
	TypeName string
	State    cty.Value
	Private  []byte
}

// Severity of a diagnostic.
type Severity int

// Severities.
const (
	SeverityError Severity = iota + 1
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	}
	return "invalid"
}

// Diagnostic is one provider diagnostic.
type Diagnostic struct {
	Severity Severity
	Summary  string
	Detail   string
	// Attribute is the attribute path, e.g. settings.secret or rules[0].name.
	Attribute string
}

// Diagnostics is a list of diagnostics.
type Diagnostics []Diagnostic

// HasErrors reports whether any diagnostic is an error.
func (d Diagnostics) HasErrors() bool {
	for _, x := range d {
		if x.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Errors returns only the error diagnostics.
func (d Diagnostics) Errors() Diagnostics { return d.filter(SeverityError) }

// Warnings returns only the warning diagnostics.
func (d Diagnostics) Warnings() Diagnostics { return d.filter(SeverityWarning) }

func (d Diagnostics) filter(s Severity) Diagnostics {
	var out Diagnostics
	for _, x := range d {
		if x.Severity == s {
			out = append(out, x)
		}
	}
	return out
}

// Err summarizes the error diagnostics as one error, or returns nil.
func (d Diagnostics) Err() error {
	errs := d.Errors()
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.String()
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

func (d Diagnostic) String() string {
	s := d.Summary
	if d.Detail != "" {
		s += ": " + d.Detail
	}
	if d.Attribute != "" {
		s = d.Attribute + ": " + s
	}
	return s
}

// typeCache holds the schema a client fetched and answers implied types.
type typeCache struct {
	schema *schema.ProviderSchema
}

func (c *typeCache) resource(typeName string) (*schema.Resource, error) {
	r, ok := c.schema.Resources[typeName]
	if !ok {
		return nil, &UnknownTypeError{TypeName: typeName}
	}
	return r, nil
}

// UnknownTypeError is returned for a resource type the provider lacks.
type UnknownTypeError struct{ TypeName string }

func (e *UnknownTypeError) Error() string {
	return fmt.Sprintf("provider has no resource type %q", e.TypeName)
}
