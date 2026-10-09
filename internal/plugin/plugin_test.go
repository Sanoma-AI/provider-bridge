// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func TestSlogSink(t *testing.T) {
	var buf bytes.Buffer
	sink := slogSink{log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug - 4}))}
	sink.Accept("terraform-provider-x", hclog.Warn, "Response contains warning", "@module", "sdk.proto", "timestamp", "2026-10-09T00:00:00Z")
	sink.Accept("terraform-provider-x", hclog.Error, "panic: boom")
	sink.Accept("", hclog.Info, "plugin process exited", "id", 42)
	sink.Accept("", hclog.Error, "plugin failed to exit gracefully")

	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		delete(e, "time")
		got = append(got, e)
	}
	want := []map[string]any{
		{"level": "WARN", "msg": "Response contains warning", "@module": "sdk.proto"},
		{"level": "ERROR", "msg": "provider stderr", "line": "panic: boom"},
		{"level": "DEBUG", "msg": "plugin process exited", "source": "go-plugin", "id": float64(42)},
		{"level": "ERROR", "msg": "plugin failed to exit gracefully", "source": "go-plugin"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines: %s", len(got), buf.String())
	}
	for i := range want {
		gb, _ := json.Marshal(got[i])
		wb, _ := json.Marshal(want[i])
		if !bytes.Equal(gb, wb) {
			t.Errorf("line %d = %s, want %s", i, gb, wb)
		}
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("HTTPS_PROXY", "http://proxy")
	env := strings.Join(Env("debug"), "\n")
	if strings.Contains(env, "GITHUB_TOKEN") || !strings.Contains(env, "HTTPS_PROXY=http://proxy") || !strings.Contains(env, "TF_LOG=debug") {
		t.Fatalf("env = %s", env)
	}
}
