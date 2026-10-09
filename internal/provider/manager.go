// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Package provider manages provider instances for the bridge: it resolves a
// (source, version) to a verified binary, caches schemas, keeps one
// configured instance per (source, version), and implements Import as
// ImportResourceState followed by ReadResource.
//
// Errors carry a Code the server maps onto Connect codes, plus the
// provider's diagnostics.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/registry"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// Ref names a provider release; SHA256, when set, pins its SHA256SUMS.
type Ref struct {
	Source  string
	Version string
	SHA256  string
}

func (r Ref) String() string { return r.Source + " " + r.Version }

// Release is a resolved, verified provider release.
type Release struct {
	// Source is the normalized source address.
	Source  string
	Version string
	// SHA256 is the hex sha256 of the release's SHA256SUMS document; empty
	// for an unverified local binary (OverrideLauncher).
	SHA256 string
	Binary string
	// Dir, when set, is where the schema document is cached.
	Dir string
}

// Instance is a running provider process.
type Instance interface {
	tfproto.Provider
	Exited() bool
	Kill()
}

// Launcher resolves releases and starts provider processes.
type Launcher interface {
	// Resolve installs (when absent) and verifies the release of source (in
	// short form) at version. Pins are the Manager's business.
	Resolve(ctx context.Context, source, version string) (Release, error)
	// Start launches the release's binary and completes the handshake.
	Start(ctx context.Context, rel Release) (Instance, error)
}

// Resource is one resource instance's state, as JSON of the implied type.
type Resource struct {
	TypeName      string
	State         json.RawMessage
	Private       []byte
	SchemaVersion int64
}

// SchemaResult is a provider's schema and its identity.
type SchemaResult struct {
	Schema *schema.ProviderSchema
	JSON   []byte
	SHA256 string
}

// Manager owns every provider process of one bridge.
type Manager struct {
	launcher Launcher
	log      *slog.Logger

	// opMu serializes the slow, state-changing operations (GetSchema on a
	// cold cache, Configure, Close) so two callers never start the same
	// provider twice. Import and Read only take mu briefly.
	opMu sync.Mutex

	mu         sync.Mutex
	releases   map[string]Release       // by source@version, resolved once
	schemas    map[string]*SchemaResult // by source@version
	configured map[string]*configured   // by source@version
	closed     bool
}

type configured struct {
	rel        Release
	inst       Instance
	schema     *schema.ProviderSchema
	configHash string
}

// NewManager returns a Manager that starts providers through launcher.
func NewManager(launcher Launcher, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		launcher:   launcher,
		log:        log,
		releases:   map[string]Release{},
		schemas:    map[string]*SchemaResult{},
		configured: map[string]*configured{},
	}
}

// refKey is the map key of a normalized ref.
func refKey(ref Ref) string { return ref.Source + "@" + ref.Version }

// normalize validates ref and spells its source in short form, so
// "registry.opentofu.org/hashicorp/null" and "hashicorp/null" share state.
func normalize(ref Ref) (Ref, error) {
	if ref.Source == "" || ref.Version == "" {
		return ref, newError(CodeInvalidArgument, "provider source and version are required", nil)
	}
	src, err := registry.ParseSource(ref.Source)
	if err != nil {
		return ref, newError(CodeInvalidArgument, err.Error(), nil)
	}
	if err := registry.CheckVersion(ref.Version); err != nil {
		return ref, newError(CodeInvalidArgument, err.Error(), nil)
	}
	ref.Source = src.String()
	ref.SHA256 = strings.ToLower(ref.SHA256)
	return ref, nil
}

// Schema returns the provider's schema, from memory, from the schema cached
// in the release's cache directory, or by starting an unconfigured instance.
func (m *Manager) Schema(ctx context.Context, ref Ref) (*SchemaResult, error) {
	ref, err := normalize(ref)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	cached := m.schemas[refKey(ref)]
	m.mu.Unlock()
	if cached != nil {
		if _, err := m.resolve(ctx, ref); err != nil {
			return nil, err
		}
		return cached, nil
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	rel, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	cached = m.schemas[refKey(ref)]
	m.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	res, err := m.loadSchemaFile(rel)
	if err != nil {
		inst, err := m.start(ctx, rel)
		if err != nil {
			return nil, err
		}
		s, diags, err := inst.GetSchema(ctx)
		inst.Kill()
		if err != nil {
			return nil, newError(CodeUnavailable, fmt.Sprintf("%s: get schema", ref), err)
		}
		if diags.HasErrors() {
			return nil, diagError(CodeFailedPrecondition, fmt.Sprintf("%s: get schema", ref), diags)
		}
		if res, err = m.storeSchema(rel, s); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	m.schemas[refKey(ref)] = res
	m.mu.Unlock()
	return res, nil
}

// schemaCacheFile names the on-disk schema cache. Bump the revision
// whenever the conversion into the schema document changes, so caches
// written by an older bridge are ignored rather than served.
const schemaCacheFile = "schema.r1.json"

func (m *Manager) loadSchemaFile(rel Release) (*SchemaResult, error) {
	if rel.Dir == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(rel.Dir, schemaCacheFile))
	if err != nil {
		return nil, err
	}
	s, err := schema.Unmarshal(b)
	if err != nil || s.Source != rel.Source || s.Version != rel.Version {
		m.log.Warn("ignoring invalid cached schema", "dir", rel.Dir, "error", err)
		return nil, os.ErrNotExist
	}
	return &SchemaResult{Schema: s, JSON: b, SHA256: rel.SHA256}, nil
}

// storeSchema stamps the schema with its release and caches it on disk.
func (m *Manager) storeSchema(rel Release, s *schema.ProviderSchema) (*SchemaResult, error) {
	stamped := *s
	stamped.Source, stamped.Version = rel.Source, rel.Version
	b, err := schema.Marshal(&stamped)
	if err != nil {
		return nil, newError(CodeInternal, "marshal schema", err)
	}
	if rel.Dir != "" {
		tmp := filepath.Join(rel.Dir, fmt.Sprintf(".%s.%d", schemaCacheFile, os.Getpid()))
		werr := os.WriteFile(tmp, b, 0o644)
		if werr == nil {
			werr = os.Rename(tmp, filepath.Join(rel.Dir, schemaCacheFile))
		}
		if werr != nil {
			m.log.Warn("cache schema", "dir", rel.Dir, "error", werr)
		}
	}
	return &SchemaResult{Schema: &stamped, JSON: b, SHA256: rel.SHA256}, nil
}

// resolve returns the verified release of ref, resolved (installed and
// hashed) once per Manager, and refuses it when ref pins another
// SHA256SUMS. It is the one place a request's pin is checked.
func (m *Manager) resolve(ctx context.Context, ref Ref) (Release, error) {
	key := refKey(ref)
	m.mu.Lock()
	rel, ok := m.releases[key]
	m.mu.Unlock()
	if !ok {
		var err error
		if rel, err = m.launcher.Resolve(ctx, ref.Source, ref.Version); err != nil {
			return Release{}, newError(CodeFailedPrecondition, fmt.Sprintf("%s: resolve provider", ref), err)
		}
		m.mu.Lock()
		m.releases[key] = rel
		m.mu.Unlock()
	}
	if ref.SHA256 != "" && ref.SHA256 != rel.SHA256 {
		return Release{}, newError(CodeFailedPrecondition, "refusing release",
			&registry.PinMismatchError{Source: rel.Source, Version: rel.Version, Want: ref.SHA256, Got: rel.SHA256})
	}
	return rel, nil
}

func (m *Manager) start(ctx context.Context, rel Release) (Instance, error) {
	inst, err := m.launcher.Start(ctx, rel)
	if err != nil {
		return nil, newError(CodeUnavailable, fmt.Sprintf("%s %s: start provider", rel.Source, rel.Version), err)
	}
	return inst, nil
}

// ConfigHash identifies a provider configuration without revealing it.
func ConfigHash(configJSON string) string {
	sum := sha256.Sum256([]byte(configJSON))
	return hex.EncodeToString(sum[:])
}

// Configure starts and configures the provider. It is idempotent for the
// same config; another config for the same (source, version) fails until
// Close.
func (m *Manager) Configure(ctx context.Context, ref Ref, configJSON string) (tfproto.Diagnostics, string, error) {
	ref, err := normalize(ref)
	if err != nil {
		return nil, "", err
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	hash := ConfigHash(configJSON)
	log := m.log.With("provider", ref.Source, "version", ref.Version, "config_sha256", hash)

	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, "", newError(CodeUnavailable, "bridge is shutting down", nil)
	}
	rel, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	cur := m.configured[refKey(ref)]
	m.mu.Unlock()
	if cur != nil {
		switch {
		case cur.inst.Exited():
			log.Warn("configured provider had exited; starting it again")
			m.remove(refKey(ref), cur)
		case cur.configHash != hash:
			return nil, "", newError(CodeFailedPrecondition, fmt.Sprintf("%s is already configured with a different config; Close it first", ref), nil)
		default:
			return nil, rel.SHA256, nil
		}
	}

	inst, err := m.start(ctx, rel)
	if err != nil {
		return nil, "", err
	}
	c, warnings, err := m.configure(ctx, ref, rel, inst, configJSON)
	if err != nil {
		inst.Kill()
		return nil, "", err
	}
	c.configHash = hash
	m.mu.Lock()
	m.configured[refKey(ref)] = c
	if m.schemas[refKey(ref)] == nil {
		if res, err := m.storeSchema(rel, c.schema); err == nil {
			m.schemas[refKey(ref)] = res
		}
	}
	m.mu.Unlock()
	log.Info("provider configured", "protocol", inst.Protocol())
	return warnings, rel.SHA256, nil
}

func (m *Manager) configure(ctx context.Context, ref Ref, rel Release, inst Instance, configJSON string) (*configured, tfproto.Diagnostics, error) {
	s, diags, err := inst.GetSchema(ctx)
	if err != nil {
		return nil, nil, newError(CodeUnavailable, fmt.Sprintf("%s: get schema", ref), err)
	}
	if diags.HasErrors() {
		return nil, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("%s: get schema", ref), diags)
	}
	warnings := diags.Warnings()
	cfg, err := values.FromJSON([]byte(configJSON), s.ProviderConfig.ImpliedType())
	if err != nil {
		return nil, nil, newError(CodeInvalidArgument, "config_json", err)
	}
	if cfg.IsNull() {
		return nil, nil, newError(CodeInvalidArgument, "config_json must be a JSON object", nil)
	}
	if cfg, err = schema.CoerceConfig(s.ProviderConfig, cfg); err != nil {
		return nil, nil, newError(CodeInvalidArgument, "config_json", err)
	}
	cfg, diags, err = inst.PrepareConfig(ctx, cfg)
	if err != nil {
		return nil, nil, newError(CodeUnavailable, fmt.Sprintf("%s: validate config", ref), err)
	}
	if diags.HasErrors() {
		return nil, nil, diagError(CodeInvalidArgument, fmt.Sprintf("%s: invalid provider config", ref), diags)
	}
	warnings = append(warnings, diags.Warnings()...)
	diags, err = inst.Configure(ctx, cfg, tfproto.TerraformVersion)
	if err != nil {
		return nil, nil, newError(CodeUnavailable, fmt.Sprintf("%s: configure", ref), err)
	}
	if diags.HasErrors() {
		return nil, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("%s: configure failed", ref), diags)
	}
	warnings = append(warnings, diags.Warnings()...)
	return &configured{rel: rel, inst: inst, schema: s}, warnings, nil
}

// instance returns the configured, live instance for ref.
func (m *Manager) instance(ctx context.Context, ref Ref) (Ref, *configured, error) {
	ref, err := normalize(ref)
	if err != nil {
		return ref, nil, err
	}
	if _, err := m.resolve(ctx, ref); err != nil {
		return ref, nil, err
	}
	m.mu.Lock()
	c := m.configured[refKey(ref)]
	m.mu.Unlock()
	if c == nil {
		return ref, nil, newError(CodeFailedPrecondition, fmt.Sprintf("%s is not configured; call Configure first", ref), nil)
	}
	if c.inst.Exited() {
		m.remove(refKey(ref), c)
		return ref, nil, newError(CodeUnavailable, fmt.Sprintf("%s: provider exited; configure again", ref), nil)
	}
	return ref, c, nil
}

func (m *Manager) remove(key string, c *configured) {
	m.mu.Lock()
	if m.configured[key] == c {
		delete(m.configured, key)
	}
	m.mu.Unlock()
	c.inst.Kill()
}

// callErr classifies an error from a provider call.
func (m *Manager) callErr(ref Ref, c *configured, what string, err error) error {
	var unknown *tfproto.UnknownTypeError
	if errors.As(err, &unknown) {
		return newError(CodeInvalidArgument, err.Error(), nil)
	}
	if c.inst.Exited() {
		m.remove(refKey(ref), c)
		return newError(CodeUnavailable, fmt.Sprintf("%s: provider exited; configure again", ref), err)
	}
	return newError(CodeUnavailable, fmt.Sprintf("%s: %s", ref, what), err)
}

// Import imports the object with the given ID and reads it. A provider may
// return several resources for one ID; each is read.
func (m *Manager) Import(ctx context.Context, ref Ref, typeName, id string) ([]Resource, tfproto.Diagnostics, error) {
	ref, c, err := m.instance(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if typeName == "" || id == "" {
		return nil, nil, newError(CodeInvalidArgument, "type_name and id are required", nil)
	}
	imported, diags, err := c.inst.ImportResourceState(ctx, typeName, id)
	if err != nil {
		return nil, nil, m.callErr(ref, c, "import", err)
	}
	if diags.HasErrors() {
		return nil, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("import %s %q", typeName, id), diags)
	}
	warnings := diags.Warnings()
	if len(imported) == 0 {
		return nil, nil, newError(CodeNotFound, fmt.Sprintf("import %s %q: provider returned no object", typeName, id), nil)
	}
	out := make([]Resource, 0, len(imported))
	for _, ir := range imported {
		res, gone, rdiags, err := m.read(ctx, ref, c, ir.TypeName, ir.State, ir.Private)
		warnings = append(warnings, rdiags...)
		if err != nil {
			return nil, nil, err
		}
		if gone {
			return nil, nil, newError(CodeNotFound, fmt.Sprintf("import %s %q: cannot import non-existent remote object", typeName, id), nil)
		}
		out = append(out, res)
	}
	return out, warnings, nil
}

// Read refreshes one resource. gone reports that the object no longer
// exists. When schemaVersion is set, stateJSON was written with that schema
// version and the provider upgrades it (UpgradeResourceState) before the
// read, as OpenTofu does on every refresh; otherwise stateJSON must be of
// the current implied type.
func (m *Manager) Read(ctx context.Context, ref Ref, typeName string, stateJSON, private []byte, schemaVersion *int64) (Resource, bool, tfproto.Diagnostics, error) {
	ref, c, err := m.instance(ctx, ref)
	if err != nil {
		return Resource{}, false, nil, err
	}
	r, ok := c.schema.Resource(typeName)
	if !ok {
		return Resource{}, false, nil, newError(CodeInvalidArgument, fmt.Sprintf("provider has no resource type %q", typeName), nil)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(stateJSON, &obj); err != nil || obj == nil {
		return Resource{}, false, nil, newError(CodeInvalidArgument, "state_json must be a JSON object", err)
	}
	var state cty.Value
	var warnings tfproto.Diagnostics
	if schemaVersion != nil {
		var diags tfproto.Diagnostics
		state, diags, err = c.inst.UpgradeResourceState(ctx, typeName, *schemaVersion, stateJSON)
		if err != nil {
			return Resource{}, false, nil, m.callErr(ref, c, "upgrade state", err)
		}
		if diags.HasErrors() {
			return Resource{}, false, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("upgrade %s state from schema version %d", typeName, *schemaVersion), diags)
		}
		warnings = diags.Warnings()
		if state.IsNull() {
			return Resource{}, false, nil, newError(CodeFailedPrecondition, fmt.Sprintf("upgrade %s state: provider returned no state", typeName), nil)
		}
	} else {
		if state, err = values.FromJSON(stateJSON, r.Block.ImpliedType()); err != nil {
			return Resource{}, false, nil, newError(CodeInvalidArgument, "state_json", err)
		}
		if state, err = schema.CoerceConfig(r.Block, state); err != nil {
			return Resource{}, false, nil, newError(CodeInvalidArgument, "state_json", err)
		}
	}
	res, gone, diags, err := m.read(ctx, ref, c, typeName, state, private)
	return res, gone, append(warnings, diags...), err
}

func (m *Manager) read(ctx context.Context, ref Ref, c *configured, typeName string, state cty.Value, private []byte) (Resource, bool, tfproto.Diagnostics, error) {
	newState, newPrivate, diags, err := c.inst.ReadResource(ctx, typeName, state, private)
	if err != nil {
		return Resource{}, false, nil, m.callErr(ref, c, "read", err)
	}
	if diags.HasErrors() {
		return Resource{}, false, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("read %s", typeName), diags)
	}
	if newState.IsNull() {
		return Resource{}, true, diags.Warnings(), nil
	}
	r, _ := c.schema.Resource(typeName)
	b, err := values.ToJSON(newState, r.Block.ImpliedType())
	if err != nil {
		return Resource{}, false, nil, newError(CodeInternal, fmt.Sprintf("read %s: encode state", typeName), err)
	}
	return Resource{TypeName: typeName, State: b, Private: newPrivate, SchemaVersion: r.SchemaVersion}, false, diags.Warnings(), nil
}

// Close stops the configured instance of ref, or every instance when ref is
// nil. Closing an unconfigured provider is not an error.
func (m *Manager) Close(ref *Ref) {
	if ref != nil {
		if r, err := normalize(*ref); err == nil {
			ref = &r
		}
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	var victims []*configured
	for key, c := range m.configured {
		if ref == nil || key == refKey(*ref) {
			victims = append(victims, c)
			delete(m.configured, key)
		}
	}
	m.mu.Unlock()
	for _, c := range victims {
		c.inst.Kill()
		m.log.Info("provider closed", "provider", c.rel.Source, "version", c.rel.Version)
	}
}

// Shutdown closes every instance and refuses new Configure calls.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.Close(nil)
}
