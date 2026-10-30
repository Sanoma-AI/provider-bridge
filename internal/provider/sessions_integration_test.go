// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/Sanoma-AI/provider-bridge/internal/provider"
	"github.com/Sanoma-AI/provider-bridge/internal/testutil"
)

func TestIntegrationConcurrentProviderSessions(t *testing.T) {
	testutil.SkipShort(t)
	log := testutil.Logger(t)
	m := provider.NewManager(testutil.Launcher(t, log, true), log)
	t.Cleanup(m.Shutdown)
	refs := []provider.Ref{}
	for i, key := range []string{"session-a-v1", "session-b", "session-a-v2"} {
		ref := session(i + 1)
		ref.Source, ref.Version = testutil.TestProviderSource, testutil.TestProviderVersion
		if _, _, err := m.Configure(ctxTimeout(t), ref, fmt.Sprintf(`{"api_key":%q}`, key)); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Go(func() {
			got, _, err := m.Import(context.Background(), ref, "testprov_item", "session-account")
			if err != nil || len(got) != 1 {
				t.Errorf("session %d import: %v", i, err)
				return
			}
			var state map[string]any
			if err := json.Unmarshal(got[0].State, &state); err != nil {
				t.Error(err)
				return
			}
			want := "account-a"
			if i == 1 {
				want = "account-b"
			}
			if state["name"] != want {
				t.Errorf("session %d used another account", i)
			}
		})
	}
	wg.Wait()
	m.Close(refs[0])
	if _, _, err := m.Import(ctxTimeout(t), refs[1], "testprov_item", "session-account"); err != nil {
		t.Fatal("closing A stopped B", err)
	}
}
