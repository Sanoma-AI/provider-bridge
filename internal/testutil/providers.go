// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
)

// TestProviderSource is the source address the in-tree test provider is
// served under (via a dev override; it is not on any registry).
const TestProviderSource = "sanoma/testprov"

// TestProviderVersion is the version tests request for the test provider.
const TestProviderVersion = "0.0.1"

// Pin is one entry of testdata/pins.json.
type Pin struct {
	Version string `json:"version"`
	// SHA256 is the hex sha256 of the release's SHA256SUMS document.
	SHA256 string `json:"sha256"`
	Note   string `json:"note,omitempty"`
}

// RepoRoot returns the main module's root directory.
func RepoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Pins reads testdata/pins.json.
func Pins(t testing.TB) map[string]Pin {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(RepoRoot(t), "testdata", "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pins map[string]Pin
	if err := json.Unmarshal(b, &pins); err != nil {
		t.Fatal(err)
	}
	return pins
}

// Ref returns the pinned ref for source.
func Ref(t testing.TB, source string) provider.Ref {
	t.Helper()
	p, ok := Pins(t)[source]
	if !ok {
		t.Fatalf("testdata/pins.json has no %s", source)
	}
	return provider.Ref{Source: source, Version: p.Version, SHA256: p.SHA256}
}

// CacheDir is the provider cache shared by test runs:
// $XDG_CACHE_HOME/provider-bridge-test, else ~/.cache/provider-bridge-test.
func CacheDir(t testing.TB) string {
	t.Helper()
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		base = filepath.Join(home, ".cache")
	}
	dir := filepath.Join(base, "provider-bridge-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// SkipShort skips tests that need the network or build external code.
func SkipShort(t testing.TB) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs network or a provider process; skipped under -short")
	}
}

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// TestProviderBinary builds testprovider/ once per test binary and returns
// the executable's path. The build needs the testprovider module's
// dependencies in the module cache (downloaded on first use).
func TestProviderBinary(t testing.TB) string {
	t.Helper()
	SkipShort(t)
	buildOnce.Do(func() {
		root := filepath.Join(CacheDir(t), "testprov")
		removeStaleBuilds(root)
		dir := filepath.Join(root, fmt.Sprintf("build-%d", os.Getpid()))
		if buildErr = os.MkdirAll(dir, 0o755); buildErr != nil {
			return
		}
		buildPath = filepath.Join(dir, "terraform-provider-testprov")
		cmd := exec.Command(goTool(), "build", "-mod=readonly", "-o", buildPath, ".")
		cmd.Dir = filepath.Join(RepoRoot(t), "testprovider")
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build testprovider: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

// removeStaleBuilds deletes test provider builds older than an hour; each
// test binary builds into its own directory so parallel packages never
// race on one file.
func removeStaleBuilds(root string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// goTool is the go command; `go test` puts its own GOROOT/bin first on PATH.
func goTool() string { return "go" }

// Launcher returns a registry launcher over the shared test cache, with the
// test provider registered as a dev override when withTestProvider is set.
func Launcher(t testing.TB, log *slog.Logger, withTestProvider bool) *provider.RegistryLauncher {
	t.Helper()
	l := &provider.RegistryLauncher{
		Cache:  registry.NewCache(CacheDir(t), log, nil),
		Logger: log,
	}
	if withTestProvider {
		l.Dev = map[string]string{TestProviderSource: TestProviderBinary(t)}
	}
	return l
}

// Logger returns a slog logger writing to the test log. Lines written after
// the test finished (late provider output) are dropped.
func Logger(t testing.TB) *slog.Logger {
	w := &testWriter{t: t}
	t.Cleanup(func() { w.done.Store(true) })
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct {
	t    testing.TB
	done atomic.Bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	if !w.done.Load() {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
