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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveNetworkNamespaces locks the back-compat contract for the
// deprecated --pod-namespace flag: it folds into a single-entry
// --network-namespaces list, but --network-namespaces always wins when both
// are supplied, and neither set leaves the result empty (the "default"
// default is applied downstream in ApplyOptionsToConfig).
func TestResolveNetworkNamespaces(t *testing.T) {
	tests := []struct {
		name              string
		networkNamespaces []string
		podNamespace      string
		want              []string
	}{
		{"neither set", nil, "", nil},
		{"only --network-namespaces", []string{"ns1", "ns2"}, "", []string{"ns1", "ns2"}},
		{"only deprecated --pod-namespace", nil, "legacy-ns", []string{"legacy-ns"}},
		{"--network-namespaces wins over --pod-namespace", []string{"ns1", "ns2"}, "legacy-ns", []string{"ns1", "ns2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveNetworkNamespaces(tt.networkNamespaces, tt.podNamespace))
		})
	}
}
