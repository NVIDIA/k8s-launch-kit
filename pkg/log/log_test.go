// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  zapcore.Level
	}{
		{name: "trace", input: "trace", want: TraceLevel},
		{name: "trace is case insensitive", input: " TRACE ", want: TraceLevel},
		{name: "debug", input: "debug", want: zapcore.DebugLevel},
		{name: "info", input: "info", want: zapcore.InfoLevel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLevel(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseLevelRejectsUnknownValue(t *testing.T) {
	_, err := parseLevel("verbose")
	require.Error(t, err)
}

func TestLevelName(t *testing.T) {
	assert.Equal(t, "trace", levelName(TraceLevel))
	assert.Equal(t, "debug", levelName(zapcore.DebugLevel))
}

func TestEncodeLevelNamesTrace(t *testing.T) {
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		LevelKey:    "level",
		EncodeLevel: encodeLevel,
	})
	buffer, err := encoder.EncodeEntry(zapcore.Entry{Level: TraceLevel}, nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"level":"TRACE"}`, buffer.String())
}
