// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
)

func session(number int) provider.Ref {
	ref := fakeRef
	ref.SessionID = fmt.Sprintf("00000000-0000-0000-0000-%012d", number)
	return ref
}

func TestSessionsSeparateProcessesPlansAndTeardown(t *testing.T) {
	m, l := newFakeManager(t)
	ctx := context.Background()
	a, b, rotated := session(1), session(2), session(3)
	for i, ref := range []provider.Ref{a, b, rotated} {
		if _, _, err := m.Configure(ctx, ref, fmt.Sprintf(`{"api_key":"synthetic-%d"}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	if l.Starts.Load() != 3 {
		t.Fatal("sessions shared a process")
	}
	ch := provider.Change{TypeName: "fake_thing", Config: []byte(`{"secret":"synthetic"}`)}
	p, _, err := m.Plan(ctx, a, ch)
	if err != nil {
		t.Fatal(err)
	}
	ch.Planned, ch.PlannedPrivate, ch.PlanToken = p.Planned, p.Private, p.Token
	for _, wrong := range []provider.Ref{b, rotated} {
		if _, _, _, err := m.Apply(ctx, wrong, ch); code(err) != provider.CodeFailedPrecondition {
			t.Fatalf("cross-session apply: %v", err)
		}
	}
	changed := ch
	changed.Config = []byte(`{"secret":"changed"}`)
	if _, _, _, err := m.Apply(ctx, a, changed); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("changed input: %v", err)
	}
	created, _, _, err := m.Apply(ctx, a, ch)
	if err != nil || created == nil {
		t.Fatalf("own-session apply: %v", err)
	}
	deletion := provider.Change{TypeName: "fake_thing", PriorState: created.State, PriorPrivate: created.Private, PriorSchemaVersion: &created.SchemaVersion}
	p, _, err = m.Plan(ctx, a, deletion)
	if err != nil {
		t.Fatal(err)
	}
	deletion.PlanToken = p.Token
	if _, _, _, err := m.Apply(ctx, b, deletion); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("cross-session delete: %v", err)
	}
	m.Close(a)
	if _, _, err := m.Import(ctx, b, "fake_thing", "same"); err != nil {
		t.Fatal("closing another account stopped this session", err)
	}
	if _, _, err := m.Configure(ctx, a, `{"api_key":"synthetic-0"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Apply(ctx, a, ch); code(err) != provider.CodeFailedPrecondition {
		t.Fatalf("restarted session accepted old plan: %v", err)
	}
}

func TestCloseDuringConfigureDoesNotLeakSession(t *testing.T) {
	m, l := newFakeManager(t)
	l.Gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, _, err := m.Configure(context.Background(), session(1), `{"api_key":"slow"}`); done <- err }()
	deadline := time.Now().Add(time.Second)
	for l.Starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if l.Starts.Load() == 0 {
		t.Fatal("configure did not start")
	}
	m.Close(session(1))
	close(l.Gate)
	if err := <-done; err == nil {
		t.Fatal("closed configuration was published")
	}
	if !l.Last().Killed() {
		t.Fatal("closed configuration leaked its process")
	}
}

func TestSessionLimitAndFailedConfigureCleanup(t *testing.T) {
	m, _ := newFakeManager(t)
	ctx := context.Background()
	missing := fakeRef
	missing.SessionID = ""
	if _, _, err := m.Configure(ctx, missing, `{}`); code(err) != provider.CodeInvalidArgument {
		t.Fatal("missing session accepted")
	}
	for i := 1; i <= 64; i++ {
		if _, _, err := m.Configure(ctx, session(i), `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := m.Configure(ctx, session(65), `{}`); code(err) != provider.CodeResourceExhausted {
		t.Fatalf("limit: %v", err)
	}
	m.Close(session(1))
	if _, _, err := m.Configure(ctx, session(65), `{"api_key":"bad"}`); err == nil {
		t.Fatal("expected rejected config")
	}
	if _, _, err := m.Configure(ctx, session(66), `{}`); err != nil {
		t.Fatal("failed config retained capacity", err)
	}
}
