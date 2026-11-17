// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package redfish

import "errors"

// Diagnostic contains only bounded public request metadata, never headers or bodies.
type Diagnostic struct {
	Code   string
	Method string
	URI    string
	Status int
	Stage  string
}

// ObservationError retains independent observation and exact-session cleanup errors.
// SessionURI is a validated resource receipt, not a session token.
type ObservationError struct {
	Primary    error
	Cleanup    error
	SessionURI string
}

func (e *ObservationError) Error() string {
	if e.Primary != nil {
		return e.Primary.Error() + "; BMC session cleanup incomplete"
	}
	return "BMC session cleanup incomplete: " + e.Cleanup.Error()
}
func (e *ObservationError) Unwrap() error {
	if e.Primary != nil {
		return e.Primary
	}
	return e.Cleanup
}

// Diagnostics preserves a failed observation even when cleanup also fails.
func Diagnostics(err error) []Diagnostic {
	if err == nil {
		return nil
	}
	var result []Diagnostic
	appendError := func(err error, stage string) {
		if err == nil {
			return
		}
		d := Diagnostic{Code: Code(err), Stage: stage}
		var remote *Error
		if errors.As(err, &remote) {
			d.Method, d.URI, d.Status = remote.Method, remote.URI, remote.Status
		}
		result = append(result, d)
	}
	var observation *ObservationError
	if errors.As(err, &observation) {
		appendError(observation.Primary, "observation")
		appendError(observation.Cleanup, "cleanup")
	} else {
		appendError(err, "observation")
	}
	return result
}

// CleanupReceipt returns only the exact validated receipt for a failed cleanup.
// An empty result means that no usable receipt is available.
func CleanupReceipt(err error) string {
	var observation *ObservationError
	if errors.As(err, &observation) {
		return observation.SessionURI
	}
	return ""
}
