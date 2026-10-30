// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

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
	sink.Accept("terraform-provider-x", hclog.Warn, "secret-in-message", "api_key", "secret-in-attribute", "timestamp", "2026-10-09T00:00:00Z")
	sink.Accept("terraform-provider-x", hclog.Error, "secret-in-stderr")
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
		{"level": "WARN", "msg": "provider log suppressed", "source": "provider"},
		{"level": "ERROR", "msg": "provider log suppressed", "source": "provider"},
		{"level": "DEBUG", "msg": "provider log suppressed", "source": "go-plugin"},
		{"level": "ERROR", "msg": "provider log suppressed", "source": "go-plugin"},
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
