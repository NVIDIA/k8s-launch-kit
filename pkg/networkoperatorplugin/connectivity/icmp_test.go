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
	"fmt"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedICMPCommandsPinSourceInterfaceInEveryMode(t *testing.T) {
	pods := []TestPod{
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-b", "rail-0", "rail-1"),
	}
	tests := []struct {
		name    string
		mode    Mode
		routing string
	}{
		{name: "quick", mode: ModeQuick, routing: config.RoutingDestinationBased},
		{name: "full", mode: ModeFull, routing: config.RoutingDestinationBased},
		{name: "strict source based", mode: ModeStrict, routing: config.RoutingSourceBased},
		{name: "strict destination based", mode: ModeStrict, routing: config.RoutingDestinationBased},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanWithOptions(pods, tc.mode, tc.routing)
			require.NotEmpty(t, plan.ICMPSameRail)
			require.NotEmpty(t, plan.ICMPCrossRail)
			icmpTests := testsForCheck(plan, CheckICMP)

			for _, test := range icmpTests {
				expected := fmt.Sprintf("ping -c 1 -W 1 -I %q %q", test.SrcIface, test.DstIP)
				assert.Equal(t, expected, icmpCommand(test), "%+v", test)
				assert.NotContains(t, icmpCommand(test), test.SrcIP, "%+v", test)
			}
		})
	}
}

func TestWrappedICMPCommandPinsInterfaceInTimeoutAndFallbackPaths(t *testing.T) {
	plan := PlanWithOptions([]TestPod{
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-b", "rail-0", "rail-1"),
	}, ModeStrict, config.RoutingSourceBased)
	require.NotEmpty(t, plan.ICMPCrossRail)
	test := plan.ICMPCrossRail[0]

	cmd := shellWithTimeout(icmpCommand(test), 5*time.Second)

	assert.Contains(t, cmd, "timeout -s TERM -k 2 5 sh -c "+shellArg(icmpCommand(test)))
	assert.Contains(t, cmd, "else "+icmpCommand(test)+" & pid=$!")
	assert.NotContains(t, cmd, test.SrcIP)
}
