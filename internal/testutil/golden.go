// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package testutil holds test helpers: golden files, the pinned provider
// downloads integration tests use, and building the in-tree protocol 6 test
// provider.
package testutil

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Update rewrites golden files instead of comparing: go test ./... -update
var Update = flag.Bool("update", false, "rewrite golden files")

// Golden compares got with the file at path, or writes it under -update.
func Golden(t testing.TB, path string, got []byte) {
	t.Helper()
	if *Update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the output (run with -update to accept):\n%s", path, diffHint(want, got))
	}
}

// diffHint shows the first differing line.
func diffHint(want, got []byte) string {
	wl := bytes.Split(want, []byte("\n"))
	gl := bytes.Split(got, []byte("\n"))
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g []byte
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if !bytes.Equal(w, g) {
			return fmt.Sprintf("line %d:\n  want: %s\n  got:  %s", i+1, w, g)
		}
	}
	return "(no line differs)"
}
