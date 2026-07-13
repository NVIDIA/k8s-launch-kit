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

package connectivity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestShellWithTimeoutPrefersNativeTimeout(t *testing.T) {
	cmd := shellWithTimeout(`ping -c 1 -W 1 -I "192.168.0.10" "192.168.0.11"`, 5*time.Second)

	assert.Contains(t, cmd, `command -v timeout`)
	assert.Contains(t, cmd, `timeout -s TERM -k 2 5 sh -c`)
	assert.Contains(t, cmd, `ping -c 1 -W 1 -I \"192.168.0.10\" \"192.168.0.11\"`)
	assert.Contains(t, cmd, `else ping -c 1 -W 1 -I "192.168.0.10" "192.168.0.11" & pid=$!`)
}

func TestShellWithTimeoutUsesMinimumOneSecond(t *testing.T) {
	cmd := shellWithTimeout(`true`, 0)

	assert.Contains(t, cmd, `timeout -s TERM -k 2 1 sh -c "true"`)
	assert.Contains(t, cmd, `sleep 1;`)
}
