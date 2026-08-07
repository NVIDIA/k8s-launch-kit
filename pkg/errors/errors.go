// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package errors

import (
	"errors"
	"fmt"
)

// Exit codes for structured error handling.
// Agents use these to distinguish error types programmatically.
const (
	ExitSuccess        = 0
	ExitGeneral        = 1
	ExitValidation     = 2 // bad flags, invalid config, no matching profile
	ExitCluster        = 3 // kubeconfig invalid, API unreachable, discovery failed
	ExitDeployment     = 4 // apply failed, timeout
	ExitPartialSuccess = 5 // discovery ok but deploy failed
)

// StructuredError is a typed error with machine-readable fields for AI agents.
type StructuredError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Category   string `json:"category"`
	Transient  bool   `json:"transient"`
	Suggestion string `json:"suggestion,omitempty"`
	ExitCode   int    `json:"-"`
	Cause      error  `json:"-"`
}

func (e *StructuredError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s", e.Message, e.Cause.Error())
	}
	return e.Message
}

func (e *StructuredError) Unwrap() error {
	return e.Cause
}

// ReportedError marks an error whose machine-readable output has already been
// emitted. The outer CLI boundary still owns the process exit code, but must
// not render the error a second time.
type ReportedError struct {
	Err error
}

func (e *ReportedError) Error() string {
	if e == nil || e.Err == nil {
		return "reported error"
	}
	return e.Err.Error()
}

func (e *ReportedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// MarkReported records that err has already been rendered to the user.
func MarkReported(err error) error {
	if err == nil {
		return nil
	}
	return &ReportedError{Err: err}
}

// IsReported returns true when an error has already been rendered.
func IsReported(err error) bool {
	var reported *ReportedError
	return errors.As(err, &reported)
}

// ExitStatusError requests a process exit code without rendering another
// error. It is used when a phase has already emitted its complete verdict.
type ExitStatusError struct {
	Code int
}

func (e *ExitStatusError) Error() string {
	if e == nil {
		return "exit status"
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// NewExitStatus creates a silent terminal status for the outer CLI boundary.
func NewExitStatus(code int) error {
	return &ExitStatusError{Code: code}
}

// IsExitStatus extracts a silent terminal status.
func IsExitStatus(err error) (int, bool) {
	var status *ExitStatusError
	if !errors.As(err, &status) || status == nil {
		return 0, false
	}
	return status.Code, true
}

// NewValidationError creates a validation error (exit code 2).
func NewValidationError(message string, cause error, suggestion string) *StructuredError {
	return &StructuredError{
		Code:       "VALIDATION_ERROR",
		Message:    message,
		Category:   "validation",
		Transient:  false,
		Suggestion: suggestion,
		ExitCode:   ExitValidation,
		Cause:      cause,
	}
}

// NewClusterError creates a cluster/API error (exit code 3).
func NewClusterError(message string, cause error, suggestion string) *StructuredError {
	return &StructuredError{
		Code:       "CLUSTER_ERROR",
		Message:    message,
		Category:   "cluster",
		Transient:  true,
		Suggestion: suggestion,
		ExitCode:   ExitCluster,
		Cause:      cause,
	}
}

// NewDeploymentError creates a deployment error (exit code 4).
func NewDeploymentError(message string, cause error, suggestion string) *StructuredError {
	return &StructuredError{
		Code:       "DEPLOYMENT_ERROR",
		Message:    message,
		Category:   "deployment",
		Transient:  true,
		Suggestion: suggestion,
		ExitCode:   ExitDeployment,
		Cause:      cause,
	}
}

// NewGeneralError creates a general error (exit code 1).
func NewGeneralError(message string, cause error) *StructuredError {
	return &StructuredError{
		Code:      "GENERAL_ERROR",
		Message:   message,
		Category:  "general",
		Transient: false,
		ExitCode:  ExitGeneral,
		Cause:     cause,
	}
}

// ExitCodeFromError extracts the exit code from an error.
// Returns ExitGeneral (1) if the error is not a StructuredError.
func ExitCodeFromError(err error) int {
	if err == nil {
		return ExitSuccess
	}
	if code, ok := IsExitStatus(err); ok {
		return code
	}
	var se *StructuredError
	if errors.As(err, &se) {
		return se.ExitCode
	}
	return ExitGeneral
}

// StructuredFromError converts any error to a StructuredError.
// If the error is already a StructuredError, it is returned as-is.
func StructuredFromError(err error) *StructuredError {
	if err == nil {
		return nil
	}
	var se *StructuredError
	if errors.As(err, &se) {
		return se
	}
	return NewGeneralError(err.Error(), err)
}
