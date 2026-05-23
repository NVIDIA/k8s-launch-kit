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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNetworkStatus(t *testing.T) {
	t.Run("empty annotation returns nil", func(t *testing.T) {
		nets, err := ParseNetworkStatus("")
		require.NoError(t, err)
		assert.Nil(t, nets)
	})

	t.Run("single secondary network", func(t *testing.T) {
		ann := `[{"name":"network-operator/rail-0","interface":"net1","ips":["10.0.0.1"],"mac":"aa:bb:cc:00:00:01"}]`
		nets, err := ParseNetworkStatus(ann)
		require.NoError(t, err)
		require.Len(t, nets, 1)
		assert.Equal(t, "network-operator/rail-0", nets[0].Name)
		assert.Equal(t, "net1", nets[0].Interface)
		assert.Equal(t, []string{"10.0.0.1"}, nets[0].IPs)
	})

	t.Run("default + secondary mix", func(t *testing.T) {
		// k8s.v1.cni.cncf.io/network-status as multus actually
		// emits it on a pod: default CNI first, then secondaries.
		ann := `[
		{"name":"k8s-pod-network","interface":"eth0","ips":["192.168.1.10"],"default":true},
		{"name":"network-operator/rail-0","interface":"net1","ips":["10.10.0.5"]},
		{"name":"network-operator/rail-1","interface":"net2","ips":["10.10.1.5"]}
	]`
		all, err := ParseNetworkStatus(ann)
		require.NoError(t, err)
		require.Len(t, all, 3)
		sec := SecondaryNetworks(all)
		require.Len(t, sec, 2)
		assert.Equal(t, "network-operator/rail-0", sec[0].Name)
		assert.Equal(t, "network-operator/rail-1", sec[1].Name)
	})

	t.Run("malformed JSON errors", func(t *testing.T) {
		_, err := ParseNetworkStatus(`not json`)
		require.Error(t, err)
	})

	t.Run("multiple IPs per attachment kept verbatim", func(t *testing.T) {
		ann := `[{"name":"rail","ips":["10.0.0.1","2001:db8::1"]}]`
		nets, err := ParseNetworkStatus(ann)
		require.NoError(t, err)
		require.Len(t, nets, 1)
		assert.Equal(t, []string{"10.0.0.1", "2001:db8::1"}, nets[0].IPs)
	})
}
