// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Package pins reads a pins file (testdata/pins.json): provider releases
// tests and the fixture recorder may download, each pinned by the sha256 of
// its SHA256SUMS document, keyed by source address.
package pins

import (
	"encoding/json"
	"fmt"
	"os"
)

// Pin is one release.
type Pin struct {
	Version string `json:"version"`
	// SHA256 is the hex sha256 of the release's SHA256SUMS document.
	SHA256 string `json:"sha256"`
	Note   string `json:"note,omitempty"`
}

// Read reads the pins file at path.
func Read(path string) (map[string]Pin, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pins map[string]Pin
	if err := json.Unmarshal(b, &pins); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pins, nil
}
