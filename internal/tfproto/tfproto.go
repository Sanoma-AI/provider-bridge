// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package tfproto is the bridge's client side of the OpenTofu plugin
// protocol. One client holds the protocol logic (schema caching, value
// encoding, the provider_meta value, the import loop) above a small wire
// adapter per protocol version (wire5, wire6) that only builds requests and
// unpacks responses of the generated gRPC stubs. Schemas are in the
// protocol-neutral internal/schema model and values are cty.
//
// The client fetches the provider schema once (or is seeded with it, see
// Provider.SetSchema) and derives the implied types it needs to encode and
// decode DynamicValues from it.
package tfproto

import (
	"context"
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/protobuf/proto"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// TerraformVersion is the fixed terraform_version sent with Configure.
// Providers use it in user agents and for feature checks.
const TerraformVersion = "1.10.0"

// Provider is one running provider process, protocol 5 or 6.
type Provider interface {
	// Protocol is the negotiated plugin protocol major version.
	Protocol() int
	// GetSchema returns the provider's schema (Source and Version unset),
	// calling the provider once.
	GetSchema(ctx context.Context) (*schema.ProviderSchema, Diagnostics, error)
	// SetSchema seeds the client with the schema of the same release
	// fetched earlier, so that no GetSchema call reaches the provider.
	SetSchema(s *schema.ProviderSchema) error
	// RawSchema calls the provider's GetSchema (protocol 5) or
	// GetProviderSchema (protocol 6) and returns the unconverted response,
	// for debugging.
	RawSchema(ctx context.Context) (proto.Message, error)
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
	// ValidateResourceConfig validates a resource's configuration
	// (ValidateResourceTypeConfig in protocol 5).
	ValidateResourceConfig(ctx context.Context, typeName string, config cty.Value) (Diagnostics, error)
	// PlanResourceChange plans the change from prior (null for a create) to
	// config, given proposed, the proposed new state (values.ProposedNew).
	PlanResourceChange(ctx context.Context, typeName string, prior, proposed, config cty.Value, priorPrivate []byte) (PlannedChange, Diagnostics, error)
	// ApplyResourceChange applies a planned change; a null planned state
	// (and null config) deletes. The new state is returned even alongside
	// error diagnostics; null means no object.
	ApplyResourceChange(ctx context.Context, typeName string, prior, planned, config cty.Value, plannedPrivate []byte) (cty.Value, []byte, Diagnostics, error)
}

// PlannedChange is a PlanResourceChange result.
type PlannedChange struct {
	// State is the planned new state; it may hold unknown values.
	State cty.Value
	// Msgpack is State as the provider encoded it.
	Msgpack []byte
	// RequiresReplace lists the paths the provider says force a
	// replacement if they change, changed or not.
	RequiresReplace []cty.Path
	Private         []byte
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

// UnknownTypeError is returned for a resource type the provider lacks.
type UnknownTypeError struct{ TypeName string }

func (e *UnknownTypeError) Error() string {
	return fmt.Sprintf("provider has no resource type %q", e.TypeName)
}
