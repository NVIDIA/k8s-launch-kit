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
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewValidationError(t *testing.T) {
	cause := fmt.Errorf("bad flag")
	se := NewValidationError("invalid config", cause, "check flags")

	assert.Equal(t, "VALIDATION_ERROR", se.Code)
	assert.Equal(t, "validation", se.Category)
	assert.Equal(t, ExitValidation, se.ExitCode)
	assert.False(t, se.Transient)
	assert.Equal(t, "check flags", se.Suggestion)
	assert.Contains(t, se.Error(), "invalid config")
	assert.Contains(t, se.Error(), "bad flag")
}

func TestNewClusterError(t *testing.T) {
	se := NewClusterError("cluster down", nil, "check kubeconfig")

	assert.Equal(t, "CLUSTER_ERROR", se.Code)
	assert.Equal(t, "cluster", se.Category)
	assert.Equal(t, ExitCluster, se.ExitCode)
	assert.True(t, se.Transient)
	assert.Equal(t, "cluster down", se.Error())
}

func TestNewDeploymentError(t *testing.T) {
	se := NewDeploymentError("apply failed", nil, "retry")

	assert.Equal(t, "DEPLOYMENT_ERROR", se.Code)
	assert.Equal(t, ExitDeployment, se.ExitCode)
	assert.True(t, se.Transient)
}

func TestNewGeneralError(t *testing.T) {
	se := NewGeneralError("something broke", nil)

	assert.Equal(t, "GENERAL_ERROR", se.Code)
	assert.Equal(t, ExitGeneral, se.ExitCode)
	assert.False(t, se.Transient)
}

func TestStructuredError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	se := NewValidationError("wrapper", cause, "")

	assert.True(t, errors.Is(se, cause))
}

func TestExitCodeFromError(t *testing.T) {
	assert.Equal(t, ExitSuccess, ExitCodeFromError(nil))
	assert.Equal(t, ExitGeneral, ExitCodeFromError(fmt.Errorf("plain error")))
	assert.Equal(t, ExitValidation, ExitCodeFromError(NewValidationError("bad", nil, "")))
	assert.Equal(t, ExitCluster, ExitCodeFromError(NewClusterError("down", nil, "")))
	assert.Equal(t, ExitDeployment, ExitCodeFromError(NewDeploymentError("fail", nil, "")))
}

func TestStructuredFromError(t *testing.T) {
	assert.Nil(t, StructuredFromError(nil))

	se := NewValidationError("test", nil, "")
	assert.Equal(t, se, StructuredFromError(se))

	plain := fmt.Errorf("plain")
	result := StructuredFromError(plain)
	assert.Equal(t, "GENERAL_ERROR", result.Code)
	assert.Contains(t, result.Error(), "plain")
}

func TestStructuredError_JSON(t *testing.T) {
	se := NewValidationError("bad input", nil, "use --help")

	data, err := json.Marshal(se)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	assert.Equal(t, "VALIDATION_ERROR", parsed["code"])
	assert.Equal(t, "bad input", parsed["message"])
	assert.Equal(t, "validation", parsed["category"])
	assert.Equal(t, false, parsed["transient"])
	assert.Equal(t, "use --help", parsed["suggestion"])
}
