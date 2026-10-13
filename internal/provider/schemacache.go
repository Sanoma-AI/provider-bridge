// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/Sanoma-AI/provider-bridge/internal/registry"
	"github.com/Sanoma-AI/provider-bridge/internal/schema"
)

// schemaCacheFile is the schema document cached beside a release's binary,
// tagged with the bridge build that wrote it: another build may convert
// schemas differently, so it ignores the file and fetches the schema again.
const schemaCacheFile = "schema.json"

type schemaCache struct {
	Bridge string          `json:"bridge"`
	Schema json.RawMessage `json:"schema"`
}

// buildID identifies the running bridge build: its module version when it
// was built from a tagged or pseudo-versioned clean tree, else its VCS
// revision when clean, else the sha256 of its executable. Empty when none
// is known; the on-disk schema cache is then not used.
var buildID = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" && !strings.HasSuffix(v, "+dirty") {
			return bi.Main.Path + "@" + v
		}
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" && modified == "false" {
			return "vcs:" + rev
		}
	}
	if exe, err := os.Executable(); err == nil {
		if sum, err := registry.FileSHA256(exe); err == nil {
			return "sha256:" + sum
		}
	}
	return ""
})

// knownSchema returns rel's schema from memory or, when this bridge build
// wrote it, from the release's cache directory; nil when neither has it.
func (m *Manager) knownSchema(rel Release) *SchemaResult {
	key := refKey(Ref{Source: rel.Source, Version: rel.Version})
	m.mu.Lock()
	res := m.schemas[key]
	m.mu.Unlock()
	if res != nil {
		return res
	}
	if res = m.loadSchemaFile(rel); res == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.schemas[key]; cur != nil {
		return cur
	}
	m.schemas[key] = res
	return res
}

func (m *Manager) loadSchemaFile(rel Release) *SchemaResult {
	id := buildID()
	if rel.Dir == "" || id == "" {
		return nil
	}
	path := filepath.Join(rel.Dir, schemaCacheFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Warn("cannot read cached schema", "path", path, "error", err)
		}
		return nil
	}
	var c schemaCache
	if err := json.Unmarshal(b, &c); err != nil {
		m.log.Warn("ignoring invalid cached schema", "path", path, "error", err)
		return nil
	}
	if c.Bridge != id {
		m.log.Debug("ignoring schema cached by another bridge build", "path", path, "cached_by", c.Bridge, "bridge", id)
		return nil
	}
	s, err := schema.Unmarshal(c.Schema)
	if err != nil {
		m.log.Warn("ignoring invalid cached schema", "path", path, "error", err)
		return nil
	}
	if s.Source != rel.Source || s.Version != rel.Version {
		m.log.Warn("ignoring cached schema of another release", "path", path,
			"want_source", rel.Source, "want_version", rel.Version, "cached_source", s.Source, "cached_version", s.Version)
		return nil
	}
	doc, err := schema.Marshal(s)
	if err != nil {
		m.log.Warn("ignoring invalid cached schema", "path", path, "error", err)
		return nil
	}
	return &SchemaResult{Schema: s, JSON: string(doc), SHA256: rel.SHA256}
}

// storeSchema stamps s with its release, keeps it in memory (unless a
// schema is already known) and caches it on disk. Callers hold rel's lock.
func (m *Manager) storeSchema(rel Release, s *schema.ProviderSchema) (*SchemaResult, error) {
	stamped := *s
	stamped.Source, stamped.Version = rel.Source, rel.Version
	doc, err := schema.Marshal(&stamped)
	if err != nil {
		return nil, newError(CodeInternal, "marshal schema", err)
	}
	if id := buildID(); rel.Dir != "" && id != "" {
		if err := writeSchemaFile(rel.Dir, id, doc); err != nil {
			m.log.Warn("cannot cache schema on disk", "dir", rel.Dir, "error", err)
		}
	}
	res := &SchemaResult{Schema: &stamped, JSON: string(doc), SHA256: rel.SHA256}
	key := refKey(Ref{Source: rel.Source, Version: rel.Version})
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.schemas[key]; cur != nil {
		return cur, nil
	}
	m.schemas[key] = res
	return res, nil
}

func writeSchemaFile(dir, id string, doc []byte) error {
	b, err := json.Marshal(schemaCache{Bridge: id, Schema: doc})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d", schemaCacheFile, os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, schemaCacheFile))
}
