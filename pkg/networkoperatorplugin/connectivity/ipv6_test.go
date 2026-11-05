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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPickNetworkIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		ips  []string
		want string
	}{
		{"IPv4", []string{"192.0.2.1"}, "192.0.2.1"},
		{"IPv6", []string{"fd02::1"}, "fd02::1"},
		{"dual stack keeps IPv4", []string{"fd02::1", "192.0.2.1"}, "192.0.2.1"},
		{"ignores unusable addresses", []string{"garbage", "1.2.3.999", "fe80::1", "::", "ff02::1", "fd02::1"}, "fd02::1"},
		{"empty", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, pickNetworkIP(tc.ips)) })
	}
}

func TestIPv6MultusDiscoveryProducesCompleteRailMatrix(t *testing.T) {
	pods := make([]TestPod, 0, 2)
	for host := 1; host <= 2; host++ {
		nets := []NetworkStatus{{Name: "default", Default: true, IPs: []string{"10.0.0.1"}}}
		for rail := 0; rail < 4; rail++ {
			for nic := 0; nic < 2; nic++ {
				nets = append(nets, NetworkStatus{Name: fmt.Sprintf("default/rail%dnic%d", rail, nic), Interface: fmt.Sprintf("net%d", rail*2+nic+1), IPs: []string{fmt.Sprintf("fd02:0:%x:%x::1", nic<<12|rail<<8, host)}})
			}
		}
		ann, err := json.Marshal(nets)
		require.NoError(t, err)
		pod, ifaces, err := buildTestPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod%d", host), Namespace: "default", Annotations: map[string]string{MultusAnnotation: string(ann)}}, Spec: corev1.PodSpec{NodeName: fmt.Sprintf("worker%d", host)}})
		require.NoError(t, err)
		require.Len(t, pod.IPsByRail, 8)
		require.Len(t, ifaces, 8)
		pod.RDMADevsByRail = map[string]string{}
		for rail := range ifaces {
			pod.RDMADevsByRail[rail] = "rdma_" + rail
		}
		pods = append(pods, pod)
	}
	plan := Plan(pods)
	require.Nil(t, plan.Skip)
	require.Len(t, plan.ICMPSameRail, 16)
	require.Len(t, plan.RDMASameRail, 16)
	require.Len(t, plan.RDMABwSameRail, 16)
	for _, test := range plan.ICMPSameRail {
		assert.Contains(t, test.SrcIP, "fd02:")
		assert.Contains(t, test.DstIP, "fd02:")
		assert.Contains(t, icmpCommand(test), "ping -6 -c")
		assert.Contains(t, routeCommand(test), `"$ipcmd" -6 -o route get`)
	}
	for _, gpu := range []bool{false, true} {
		server := ibWriteBwBatchServerCommandMode(plan.RDMABwSameRail, 65536, gpu)
		client := ibWriteBwBatchClientCommandMode(plan.RDMABwSameRail, 65536, gpu)
		assert.Contains(t, server, "-R --ipv6 --ipv6-addr")
		assert.Contains(t, client, "-R --ipv6 --ipv6-addr")
	}
	assert.Contains(t, rpingBatchClientCommand(plan.RDMASameRail, 5), "fd02:")
}

func TestIPv4CommandsKeepExistingFamilySelection(t *testing.T) {
	test := PingTest{SrcIP: "192.0.2.1", DstIP: "192.0.2.2", SrcRDMADev: "mlx5_0", DstRDMADev: "mlx5_1"}
	assert.NotContains(t, icmpCommand(test), "-6")
	assert.NotContains(t, routeCommand(test), "-6")
	assert.NotContains(t, ibWriteBwBatchServerCommandMode([]PingTest{test}, 65536, false), "--ipv6")
	assert.NotContains(t, ibWriteBwBatchClientCommand([]PingTest{test}, 65536), "--ipv6")
}
