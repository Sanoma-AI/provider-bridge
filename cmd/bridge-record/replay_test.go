// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/Sanoma-AI/provider-bridge/internal/schema"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
	"github.com/Sanoma-AI/provider-bridge/internal/values"
)

// TestReplayFixtures parses every recorded state against its schema's
// implied type, so the state shape clients rely on is checked in CI without
// credentials, and checks that no sensitive value escaped scrubbing.
func TestReplayFixtures(t *testing.T) {
	root := filepath.Join(testutil.RepoRoot(t), "testdata")
	dirs, err := os.ReadDir(filepath.Join(root, "replies"))
	if err != nil {
		t.Skipf("no recorded replies: %v", err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		t.Run(d.Name(), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, "schemas", d.Name()+".json"))
			if err != nil {
				t.Fatalf("replies without a schema: %v", err)
			}
			s, err := schema.Unmarshal(b)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			err = filepath.WalkDir(filepath.Join(root, "replies", d.Name()), func(path string, e os.DirEntry, err error) error {
				if err != nil || e.IsDir() || !strings.HasSuffix(path, ".json") {
					return err
				}
				n++
				checkFixture(t, s, path)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("no fixtures")
			}
		})
	}
}

type stateHolder struct {
	TypeName  string `json:"typeName"`
	StateJSON string `json:"stateJson"`
}

// changeHolder holds the states of a Plan or Apply fixture.
type changeHolder struct {
	TypeName         string `json:"typeName"`
	PriorStateJSON   string `json:"priorStateJson"`
	ConfigJSON       string `json:"configJson"`
	PlannedStateJSON string `json:"plannedStateJson"`
	Planned          []byte `json:"planned"`
}

func checkFixture(t *testing.T, s *schema.ProviderSchema, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Provider providerInfo `json:"provider"`
		Request  stateHolder  `json:"request"`
		Response struct {
			Resources []stateHolder `json:"resources"`
			Resource  *stateHolder  `json:"resource"`
			Error     *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var change struct {
		Request  changeHolder `json:"request"`
		Response changeHolder `json:"response"`
	}
	if err := json.Unmarshal(b, &change); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if f.Provider.Source != s.Source || f.Provider.Version != s.Version || f.Provider.SHA256 == "" {
		t.Errorf("%s: provider %+v does not match schema %s %s", path, f.Provider, s.Source, s.Version)
	}
	states := append([]stateHolder{}, f.Response.Resources...)
	if f.Response.Resource != nil {
		states = append(states, *f.Response.Resource)
	}
	if f.Request.StateJSON != "" {
		states = append(states, f.Request)
	}
	// Plan and Apply: prior state, config, planned state (unknowns null)
	// and the plan itself, scrubbed alike.
	typeName := change.Request.TypeName
	for _, js := range []string{change.Request.PriorStateJSON, change.Request.ConfigJSON, change.Response.PlannedStateJSON} {
		if js != "" {
			states = append(states, stateHolder{TypeName: typeName, StateJSON: js})
		}
	}
	for _, mp := range [][]byte{change.Request.Planned, change.Response.Planned} {
		if len(mp) == 0 {
			continue
		}
		r, ok := s.Resources[typeName]
		if !ok {
			t.Errorf("%s: unknown type %s", path, typeName)
			continue
		}
		v, err := values.FromDynamic(mp, nil, r.Block.ImpliedType())
		if err != nil || v.IsNull() {
			t.Errorf("%s: planned is not a plan of %s: %v", path, typeName, err)
		} else if again := newScrubber(nil).State(typeName, r.Block, v); !again.RawEquals(v) {
			t.Errorf("%s: a sensitive attribute of the plan is not scrubbed", path)
		}
	}
	// A read of a gone object and a delete leave no state.
	deletes := filepath.Base(path) == "apply-delete.json" || strings.HasSuffix(filepath.Dir(path), "-delete")
	if len(states) == 0 && f.Response.Error == nil && !strings.HasSuffix(path, "read.json") && !deletes {
		t.Errorf("%s: neither state nor error", path)
	}
	for _, st := range states {
		res, ok := s.Resources[st.TypeName]
		if !ok {
			t.Errorf("%s: unknown type %s", path, st.TypeName)
			continue
		}
		ty := res.Block.ImpliedType()
		v, err := values.FromJSON([]byte(st.StateJSON), ty)
		if err != nil {
			t.Errorf("%s: state does not parse as %s: %v", path, st.TypeName, err)
			continue
		}
		out, err := values.ToJSON(v, ty)
		if err != nil || string(out) != st.StateJSON {
			t.Errorf("%s: state is not canonical: %v", path, err)
		}
		// Scrubbing again must change nothing: every sensitive value is
		// already the placeholder.
		again, _ := values.ToJSON(newScrubber(nil).State(st.TypeName, res.Block, v), ty)
		if string(again) != st.StateJSON {
			t.Errorf("%s: a sensitive attribute of %s is not scrubbed", path, st.TypeName)
		}
	}
}

func TestScrubber(t *testing.T) {
	s := testutil.FakeSchema()
	block := s.Resources["fake_thing"].Block
	block.Attributes["nested"] = &schema.Attribute{Optional: true, NestedType: &schema.Object{
		Nesting:    schema.NestingList,
		Attributes: map[string]*schema.Attribute{"token": {Type: cty.String, Sensitive: true}, "label": {Type: cty.String}},
	}}
	block.FillTypes()
	ty := block.ImpliedType()
	v, err := values.FromJSON([]byte(`{"id":"1","name":"n","secret":"s3cret","nested":[{"token":"t","label":"keep"}]}`), ty)
	if err != nil {
		t.Fatal(err)
	}
	sc := newScrubber([]string{"fake_thing.name"})
	out, err := values.ToJSON(sc.State("fake_thing", block, v), ty)
	if err != nil {
		t.Fatal(err)
	}
	// ctyjson escapes < and > as \u003c and \u003e; the decoded value is "<scrubbed>".
	want := `{"id":"1","name":"\u003cscrubbed\u003e","nested":[{"label":"keep","token":"\u003cscrubbed\u003e"}],"secret":"\u003cscrubbed\u003e"}`
	if string(out) != want {
		t.Fatalf("scrubbed =\n%s\nwant\n%s", out, want)
	}
	if got := strings.Join(sc.Hits(), ","); got != "fake_thing.name,fake_thing.nested.token,fake_thing.secret" {
		t.Fatalf("hits = %s", got)
	}

	secrets := map[string]string{"TOKEN": "gho_abc/def\"x"}
	b := replaceSecrets([]byte(`{"a":"gho_abc/def\"x","b":"Bearer gho_abc/def\"x"}`), secrets)
	r := &recorder{secrets: secrets}
	if err := r.assertClean(b); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if err := r.assertClean([]byte(`gho_abc/def"x`)); err == nil {
		t.Fatal("raw secret passed assertClean")
	}
}

// TestSchemaFixtures checks every recorded schema document parses and is in
// the canonical form Marshal produces.
func TestSchemaFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testutil.RepoRoot(t), "testdata", "schemas", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no schemas: %v", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s, err := schema.Unmarshal(b)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		again, err := schema.Marshal(s)
		if err != nil || string(again) != string(b) {
			t.Errorf("%s: not canonical (%v)", p, err)
		}
		for name, r := range s.Resources {
			if ty := r.Block.ImpliedType(); !ty.IsObjectType() {
				t.Errorf("%s: %s implied type %#v", p, name, ty)
			}
		}
	}
}
