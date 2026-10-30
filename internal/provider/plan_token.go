// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
)

// No plan or credential cache: authenticate each exact Apply input with this
// configured process's random key. Restart/rotation invalidates every old plan.
func (c *configured) planToken(ch Change) []byte {
	// Omit the token itself and canonicalize empty byte slices to nil.
	bytes := func(b []byte) []byte {
		if len(b) == 0 {
			return nil
		}
		return b
	}
	payload, _ := json.Marshal(struct {
		Type                            string
		Prior, Private                  []byte
		Version                         *int64
		Config, Planned, PlannedPrivate []byte
	}{ch.TypeName, bytes(ch.PriorState), bytes(ch.PriorPrivate), ch.PriorSchemaVersion, bytes(ch.Config), bytes(ch.Planned), bytes(ch.PlannedPrivate)})
	mac := hmac.New(sha256.New, c.planKey[:])
	mac.Write(payload)
	return mac.Sum(nil)
}
func (c *configured) validPlanToken(ch Change) bool {
	want := c.planToken(ch)
	token := ch.PlanToken
	if len(token) != sha256.Size && len(token) != 2*sha256.Size {
		return false
	}
	// A replace has one token for delete and one for create.
	valid := hmac.Equal(want, token[:sha256.Size])
	if len(token) == 2*sha256.Size {
		valid = hmac.Equal(want, token[sha256.Size:]) || valid
	}
	return valid
}
