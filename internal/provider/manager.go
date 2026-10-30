// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package provider manages provider instances for the bridge: it resolves a
// (source, version) to a verified binary, caches schemas, keeps one
// configured instance per (source, version, session), implements Import as
// ImportResourceState followed by ReadResource, and Plan and Apply as
// OpenTofu does (change.go).
//
// Errors carry a Code the server maps onto Connect codes, plus the
// provider's diagnostics.
package provider

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
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
	Source    string
	Version   string
	SHA256    string
	SessionID string
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
	// JSON is the schema document (schema.Marshal of Schema).
	JSON   string
	SHA256 string
}

// Manager owns every provider process of one bridge.
type Manager struct {
	launcher Launcher
	log      *slog.Logger

	// mu guards the maps and closed; it is never held across a download,
	// a process start or a provider call. The slow operations on one
	// (source, version), a cold GetSchema and Configure, serialize on that
	// key's lock instead, so one slow provider never blocks another.
	mu         sync.Mutex
	locks      map[string]chan struct{} // by source@version
	releases   map[string]Release       // by source@version, resolved once
	schemas    map[string]*SchemaResult // by source@version
	configured map[string]*configured   // by source@version/session
	pending    int                      // starts reserved against the process bound
	starting   map[string]bool
	closed     bool
}

type configured struct {
	rel        Release
	inst       Instance
	schema     *schema.ProviderSchema
	configHash string
	planKey    [32]byte
}

// NewManager returns a Manager that starts providers through launcher.
func NewManager(launcher Launcher, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		launcher:   launcher,
		log:        log,
		locks:      map[string]chan struct{}{},
		releases:   map[string]Release{},
		schemas:    map[string]*SchemaResult{},
		configured: map[string]*configured{},
		starting:   map[string]bool{},
	}
}

// refKey is the map key of a normalized ref.
func refKey(ref Ref) string { return ref.Source + "@" + ref.Version }

const maxSessions = 64

var sessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func sessionKey(ref Ref) string { return refKey(ref) + "/" + ref.SessionID }
func checkSession(ref Ref) error {
	if !sessionID.MatchString(ref.SessionID) {
		return newError(CodeInvalidArgument, "a random session_id UUID is required; upgrade the bridge client", nil)
	}
	return nil
}

// lock takes ref's key lock, or gives up when ctx is done.
func (m *Manager) lock(ctx context.Context, ref Ref) (unlock func(), err error) {
	m.mu.Lock()
	l, ok := m.locks[refKey(ref)]
	if !ok {
		l = make(chan struct{}, 1)
		m.locks[refKey(ref)] = l
	}
	m.mu.Unlock()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, newError(CodeUnavailable, fmt.Sprintf("%s: waiting for another call", ref), ctx.Err())
	}
}

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
	res := m.schemas[refKey(ref)]
	m.mu.Unlock()
	if res != nil {
		if _, err := m.resolve(ctx, ref); err != nil {
			return nil, err
		}
		return res, nil
	}

	unlock, err := m.lock(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rel, err := m.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Another caller may have fetched it while this one waited.
	if res := m.knownSchema(rel); res != nil {
		return res, nil
	}
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
	return m.storeSchema(rel, s)
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
func ConfigHash(configJSON string) string { return registry.SHA256Hex([]byte(configJSON)) }

// Configure starts and configures the provider. It is idempotent for the
// same config; another config for the same (source, version, session) fails until
// Close.
func (m *Manager) Configure(ctx context.Context, ref Ref, configJSON string) (tfproto.Diagnostics, string, error) {
	ref, err := normalize(ref)
	if err != nil {
		return nil, "", err
	}
	if err := checkSession(ref); err != nil {
		return nil, "", err
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	hash := ConfigHash(configJSON)
	log := m.log.With("provider", ref.Source, "version", ref.Version)

	unlock, err := m.lock(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	defer unlock()
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
	cur := m.configured[sessionKey(ref)]
	m.mu.Unlock()
	if cur != nil {
		switch {
		case cur.inst.Exited():
			log.Warn("configured provider had exited; starting it again")
			m.remove(sessionKey(ref), cur)
		case cur.configHash != hash:
			return nil, "", newError(CodeFailedPrecondition, fmt.Sprintf("%s is already configured with a different config; Close it first", ref), nil)
		default:
			return nil, rel.SHA256, nil
		}
	}

	m.mu.Lock()
	if len(m.configured)+m.pending >= maxSessions {
		m.mu.Unlock()
		return nil, "", newError(CodeResourceExhausted, "provider session limit reached; close completed sessions", nil)
	}
	m.pending++
	m.starting[sessionKey(ref)] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.pending--; delete(m.starting, sessionKey(ref)); m.mu.Unlock() }()
	inst, err := m.start(ctx, rel)
	if err != nil {
		return nil, "", err
	}
	known := m.knownSchema(rel)
	if known != nil {
		if err := inst.SetSchema(known.Schema); err != nil {
			inst.Kill()
			return nil, "", newError(CodeInternal, fmt.Sprintf("%s: seed schema", ref), err)
		}
	}
	c, warnings, err := m.configure(ctx, ref, rel, inst, configJSON)
	if err != nil {
		inst.Kill()
		return nil, "", err
	}
	c.configHash = hash
	if _, err := rand.Read(c.planKey[:]); err != nil {
		inst.Kill()
		return nil, "", newError(CodeInternal, "session initialization failed", nil)
	}
	m.mu.Lock()
	if m.closed || !m.starting[sessionKey(ref)] {
		m.mu.Unlock()
		inst.Kill()
		return nil, "", newError(CodeUnavailable, "bridge is shutting down", nil)
	}
	m.configured[sessionKey(ref)] = c
	m.mu.Unlock()
	if known == nil {
		if _, err := m.storeSchema(rel, c.schema); err != nil {
			log.Warn("cannot cache the schema", "error", err)
		}
	}
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
	if err := checkSession(ref); err != nil {
		return ref, nil, err
	}
	m.mu.Lock()
	c := m.configured[sessionKey(ref)]
	m.mu.Unlock()
	if c == nil {
		return ref, nil, newError(CodeFailedPrecondition, fmt.Sprintf("%s is not configured; call Configure first", ref), nil)
	}
	// Configure resolved the release, so this only checks the pin.
	if _, err := m.resolve(ctx, ref); err != nil {
		return ref, nil, err
	}
	if c.inst.Exited() {
		m.remove(sessionKey(ref), c)
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
		return newError(CodeInvalidArgument, "", err)
	}
	if c.inst.Exited() {
		m.remove(sessionKey(ref), c)
		return newError(CodeUnavailable, fmt.Sprintf("%s: provider exited; configure again", ref), err)
	}
	return newError(CodeUnavailable, fmt.Sprintf("%s: %s", ref, what), err)
}

// Import imports the object with the given ID and reads it. A provider may
// return several resources for one ID; they are read concurrently and
// returned in the provider's order.
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
	type readResult struct {
		res   Resource
		gone  bool
		diags tfproto.Diagnostics
		err   error
	}
	results := make([]readResult, len(imported))
	var wg sync.WaitGroup
	for i, ir := range imported {
		r, ok := c.schema.Resource(ir.TypeName)
		if !ok { // the client checks imported types; this is a bug guard
			return nil, nil, newError(CodeInternal, "import", &tfproto.UnknownTypeError{TypeName: ir.TypeName})
		}
		wg.Go(func() {
			res, gone, diags, err := m.read(ctx, ref, c, r, ir.TypeName, ir.State, ir.Private)
			results[i] = readResult{res, gone, diags, err}
		})
	}
	wg.Wait()
	out := make([]Resource, 0, len(imported))
	for _, rr := range results {
		warnings = append(warnings, rr.diags...)
		if rr.err != nil {
			return nil, nil, rr.err
		}
		if rr.gone {
			return nil, nil, newError(CodeNotFound, fmt.Sprintf("import %s %q: cannot import non-existent remote object", typeName, id), nil)
		}
		out = append(out, rr.res)
	}
	return out, warnings, nil
}

// Read refreshes one resource. gone reports that the object no longer
// exists. When schemaVersion is set, stateJSON was written with that schema
// version and the provider upgrades it (UpgradeResourceState) before the
// read, as OpenTofu does on every refresh; otherwise stateJSON must be of
// the current implied type.
func (m *Manager) Read(ctx context.Context, ref Ref, typeName string, stateJSON, private []byte, schemaVersion *int64) (Resource, bool, tfproto.Diagnostics, error) {
	ref, c, r, err := m.resourceInstance(ctx, ref, typeName)
	if err != nil {
		return Resource{}, false, nil, err
	}
	state, warnings, err := m.state(ctx, ref, c, r, typeName, "state_json", stateJSON, schemaVersion)
	if err != nil {
		return Resource{}, false, nil, err
	}
	res, gone, diags, err := m.read(ctx, ref, c, r, typeName, state, private)
	return res, gone, append(warnings, diags...), err
}

// state decodes stateJSON, a resource's state, into a value of its current
// implied type: through UpgradeResourceState when schemaVersion is set, else
// directly. field names the request field in errors.
func (m *Manager) state(ctx context.Context, ref Ref, c *configured, r *schema.Resource, typeName, field string, stateJSON []byte, schemaVersion *int64) (cty.Value, tfproto.Diagnostics, error) {
	if schemaVersion == nil {
		v, err := object(r.Block, field, stateJSON)
		return v, nil, err
	}
	if err := isObject(field, stateJSON); err != nil {
		return cty.NilVal, nil, err
	}
	state, diags, err := c.inst.UpgradeResourceState(ctx, typeName, *schemaVersion, stateJSON)
	if err != nil {
		return cty.NilVal, nil, m.callErr(ref, c, "upgrade state", err)
	}
	if diags.HasErrors() {
		return cty.NilVal, nil, diagError(CodeFailedPrecondition, fmt.Sprintf("upgrade %s state from schema version %d", typeName, *schemaVersion), diags)
	}
	if state.IsNull() {
		return cty.NilVal, nil, newError(CodeFailedPrecondition, fmt.Sprintf("upgrade %s state: provider returned no state", typeName), nil)
	}
	return state, diags.Warnings(), nil
}

// object decodes a JSON object of b's implied type, normalized as OpenTofu
// core does (schema.CoerceConfig). field names the request field in errors.
func object(b *schema.Block, field string, js []byte) (cty.Value, error) {
	if err := isObject(field, js); err != nil {
		return cty.NilVal, err
	}
	v, err := values.FromJSON(js, b.ImpliedType())
	if err == nil {
		v, err = schema.CoerceConfig(b, v)
	}
	if err != nil {
		return cty.NilVal, newError(CodeInvalidArgument, field, err)
	}
	return v, nil
}

func isObject(field string, js []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(js, &obj); err != nil || obj == nil {
		return newError(CodeInvalidArgument, field+" must be a JSON object", err)
	}
	return nil
}

// read calls ReadResource; r is typeName's schema.
func (m *Manager) read(ctx context.Context, ref Ref, c *configured, r *schema.Resource, typeName string, state cty.Value, private []byte) (Resource, bool, tfproto.Diagnostics, error) {
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
	b, err := values.ToJSON(newState, r.Block.ImpliedType())
	if err != nil {
		return Resource{}, false, nil, newError(CodeInternal, fmt.Sprintf("read %s: encode state", typeName), err)
	}
	return Resource{TypeName: typeName, State: b, Private: newPrivate, SchemaVersion: r.SchemaVersion}, false, diags.Warnings(), nil
}

// Close stops the configured instance of ref, or every instance when ref is
// the zero Ref. Closing an unconfigured provider is not an error.
func (m *Manager) Close(ref Ref) {
	all := ref == Ref{}
	if !all {
		if r, err := normalize(ref); err == nil {
			ref = r
		}
	}
	m.mu.Lock()
	for key := range m.starting {
		if all || key == sessionKey(ref) {
			m.starting[key] = false
		}
	}
	var victims []*configured
	for key, c := range m.configured {
		if all || key == sessionKey(ref) {
			victims = append(victims, c)
			delete(m.configured, key)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range victims {
		wg.Go(func() {
			c.inst.Kill()
			m.log.Info("provider closed", "provider", c.rel.Source, "version", c.rel.Version)
		})
	}
	wg.Wait()
}

// Shutdown closes every instance and refuses new Configure calls; a
// Configure already under way kills its instance instead of keeping it.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.Close(Ref{})
}
