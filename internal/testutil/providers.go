// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"io"
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

	"github.com/Sanoma-AI/provider-bridge/internal/pins"
	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/registry"
)

// TestProviderSource is the source address the in-tree test provider is
// served under (via a dev override; it is not on any registry).
const TestProviderSource = "sanoma/testprov"

// TestProviderVersion is the version tests request for the test provider.
const TestProviderVersion = "0.0.1"

// RepoRoot returns the main module's root directory.
func RepoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Ref returns the ref testdata/pins.json pins for source.
func Ref(t testing.TB, source string) provider.Ref {
	t.Helper()
	all, err := pins.Read(filepath.Join(RepoRoot(t), "testdata", "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := all[source]
	if !ok {
		t.Fatalf("testdata/pins.json has no %s", source)
	}
	return provider.Ref{Source: source, Version: p.Version, SHA256: p.SHA256}
}

// CacheDir is the provider cache shared by test runs and bridge-record:
// provider-bridge-test in the user's cache directory (registry.CacheDir).
func CacheDir(t testing.TB) string {
	t.Helper()
	dir := registry.CacheDir("provider-bridge-test")
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

// TestProviderEnv names an already built test provider (`make testprov`);
// when it is set, tests use that binary instead of building their own.
const TestProviderEnv = "PROVIDER_BRIDGE_TESTPROV"

// TestProviderBinary returns the test provider executable: $TestProviderEnv,
// or testprovider/ built once per test binary. The build needs the
// testprovider module's dependencies in the module cache (downloaded on
// first use).
func TestProviderBinary(t testing.TB) string {
	t.Helper()
	SkipShort(t)
	if bin := os.Getenv(TestProviderEnv); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("$%s: %v", TestProviderEnv, err)
		}
		return bin
	}
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

// Launcher returns a registry launcher over the shared test cache, composed
// with an override serving the test provider when withTestProvider is set
// (as `serve --dev-provider` does).
func Launcher(t testing.TB, log *slog.Logger, withTestProvider bool) provider.Launcher {
	t.Helper()
	var l provider.Launcher = &provider.RegistryLauncher{
		Cache:  registry.NewCache(CacheDir(t), log, nil),
		Logger: log,
	}
	if withTestProvider {
		l = &provider.OverrideLauncher{Binaries: map[string]string{TestProviderSource: TestProviderBinary(t)}, Next: l}
	}
	return l
}

// Logger returns a slog logger writing to the test log (see Writer).
func Logger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(Writer(t), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Writer returns a writer to the test log, one t.Log per write. Writes
// after the test finished (late provider output) are dropped.
func Writer(t testing.TB) io.Writer {
	w := &testWriter{t: t}
	t.Cleanup(func() { w.done.Store(true) })
	return w
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
