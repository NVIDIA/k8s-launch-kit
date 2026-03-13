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

package ui

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONOutput_CollectsMessages(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	jo := NewJSON(stdout, stderr)

	jo.Info("hello %s", "world")
	jo.Success("done")
	jo.Warning("watch out")
	jo.Error("oops")

	msgs := jo.Messages()
	require.Len(t, msgs, 4)
	assert.Equal(t, "info", msgs[0].Level)
	assert.Equal(t, "hello world", msgs[0].Message)
	assert.Equal(t, "success", msgs[1].Level)
	assert.Equal(t, "warning", msgs[2].Level)
	assert.Equal(t, "error", msgs[3].Level)

	// stderr should have human-readable output
	assert.Contains(t, stderr.String(), "hello world")
	assert.Contains(t, stderr.String(), "done")
}

func TestJSONOutput_ConfirmAlwaysTrue(t *testing.T) {
	jo := NewJSON(&bytes.Buffer{}, &bytes.Buffer{})

	result, err := jo.Confirm("are you sure?")
	assert.NoError(t, err)
	assert.True(t, result)
}

func TestJSONOutput_HeaderAndSectionAreNoOps(t *testing.T) {
	stderr := &bytes.Buffer{}
	jo := NewJSON(&bytes.Buffer{}, stderr)

	jo.Header("test header") // should not panic or produce output
	jo.Section("test section")

	msgs := jo.Messages()
	// Header is a no-op, Section appends an info message
	assert.Len(t, msgs, 1)
	assert.Equal(t, "test section", msgs[0].Message)
}

func TestJSONOutput_ProgressIsNoOp(t *testing.T) {
	jo := NewJSON(&bytes.Buffer{}, &bytes.Buffer{})

	p := jo.StartProgress("loading")
	p.Update("still loading")
	p.Success("loaded")

	msgs := jo.Messages()
	assert.Len(t, msgs, 3) // start + update + success
}

func TestJSONOutput_Finalize(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	jo := NewJSON(stdout, stderr)

	jo.Info("step 1")
	jo.Success("step 2")

	result := &JSONResult{
		Success:        true,
		Phase:          "generate",
		GeneratedFiles: []string{"/tmp/a.yaml", "/tmp/b.yaml"},
		Deployed:       false,
	}

	err := jo.Finalize(result)
	require.NoError(t, err)

	// Parse the JSON output
	var parsed JSONResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &parsed))

	assert.True(t, parsed.Success)
	assert.Equal(t, "generate", parsed.Phase)
	assert.Len(t, parsed.GeneratedFiles, 2)
	assert.False(t, parsed.Deployed)
	assert.Len(t, parsed.Messages, 2)
	assert.Equal(t, "step 1", parsed.Messages[0].Message)
}

func TestJSONOutput_FinalizeWithError(t *testing.T) {
	stdout := &bytes.Buffer{}
	jo := NewJSON(stdout, &bytes.Buffer{})

	errJSON, _ := json.Marshal(map[string]string{
		"code":    "VALIDATION_ERROR",
		"message": "bad input",
	})

	result := &JSONResult{
		Success: false,
		Phase:   "init",
		Error:   errJSON,
	}

	err := jo.Finalize(result)
	require.NoError(t, err)

	var parsed JSONResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &parsed))

	assert.False(t, parsed.Success)
	assert.NotNil(t, parsed.Error)
}

func TestNewOutputForFormat_Text(t *testing.T) {
	output, jsonOut := NewOutputForFormat("text", false)
	assert.NotNil(t, output)
	assert.Nil(t, jsonOut)
}

func TestNewOutputForFormat_JSON(t *testing.T) {
	output, jsonOut := NewOutputForFormat("json", false)
	assert.NotNil(t, output)
	assert.NotNil(t, jsonOut)
	// output and jsonOut should be the same object
	assert.Equal(t, output, jsonOut)
}

func TestNewOutputForFormat_TextWithAutoConfirm(t *testing.T) {
	output, jsonOut := NewOutputForFormat("text", true)
	assert.NotNil(t, output)
	assert.Nil(t, jsonOut)
	// Should auto-confirm
	result, err := output.Confirm("test?")
	assert.NoError(t, err)
	assert.True(t, result)
}

func TestStandardOutput_AutoConfirm(t *testing.T) {
	output := NewWithOptions(OutputOptions{AutoConfirm: true})
	result, err := output.Confirm("test?")
	assert.NoError(t, err)
	assert.True(t, result)
}
