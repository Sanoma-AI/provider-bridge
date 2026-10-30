// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

package provider

import (
	"strings"

	"github.com/Sanoma-AI/provider-bridge/internal/tfproto"
)

// Code classifies a Manager error; the server maps it to a Connect code.
type Code int

// Codes.
const (
	CodeInternal Code = iota
	// CodeInvalidArgument: bad request (unknown type, malformed JSON,
	// invalid provider config).
	CodeInvalidArgument
	// CodeFailedPrecondition: not configured, configured differently, pin
	// mismatch, or the provider reported error diagnostics.
	CodeFailedPrecondition
	// CodeUnavailable: the provider process failed or exited.
	CodeUnavailable
	// CodeNotFound: an import found no remote object.
	CodeNotFound
	// CodeResourceExhausted: callers must close sessions before starting more.
	CodeResourceExhausted
)

// Error is a Manager error with the provider's error diagnostics, if any.
type Error struct {
	Code        Code
	Message     string
	Diagnostics tfproto.Diagnostics
	Err         error
}

func (e *Error) Error() string {
	var parts []string
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if derr := e.Diagnostics.Err(); derr != nil {
		parts = append(parts, derr.Error())
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error { return e.Err }

func newError(code Code, msg string, err error) *Error {
	return &Error{Code: code, Message: msg, Err: err}
}

func diagError(code Code, msg string, diags tfproto.Diagnostics) *Error {
	return &Error{Code: code, Message: msg, Diagnostics: diags.Errors()}
}
