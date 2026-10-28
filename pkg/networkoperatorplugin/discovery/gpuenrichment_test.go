// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"testing"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPUCollapsedSiblingCandidatesAreComparedBeforeBalancing(t *testing.T) {
	for _, mode := range []string{"identical-ties", "overlapping-ties", "disjoint-nearest", "different-proximity"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, _ := fabricFixtures(2, true)
			observations := map[string]nodeGPUObservation{}
			for _, node := range groups[0].WorkerNodes {
				observation := gpuParsedFixture(t)
				observation.data.matrix["mlx5_local0"] = map[int]proximity{0: proxPHB, 1: proxPHB}
				observation.data.matrix["mlx5_local1"] = map[int]proximity{0: proxPHB, 1: proxPHB}
				switch mode {
				case "overlapping-ties":
					observation.data.matrix["mlx5_local1"][0] = proxNODE
				case "disjoint-nearest":
					observation.data.matrix["mlx5_local0"][1] = proxNODE
					observation.data.matrix["mlx5_local1"][0] = proxNODE
				case "different-proximity":
					observation.data.matrix["mlx5_local1"] = map[int]proximity{0: proxPIX, 1: proxPIX}
				}
				observations[node] = observation
			}
			applyGPUObservations(context.Background(), groups, buildPFIdentities(groups, devices, members, true), observations)
			switch mode {
			case "identical-ties":
				assert.Equal(t, "GPU0", groups[0].PFs[0].ConnectedGPU)
				assert.Equal(t, "PHB", groups[0].PFs[0].GPUProximity)
			case "overlapping-ties":
				assert.Equal(t, "GPU1", groups[0].PFs[0].ConnectedGPU, "one common nearest GPU supports the retained rail")
			default:
				assert.Empty(t, groups[0].PFs[0].ConnectedGPU, "incompatible physical evidence must remain unknown")
			}
		})
	}
}

func TestGPUCollapsedRailsConsumeOneBalancingAssignmentEach(t *testing.T) {
	_, devices, _, _ := fabricFixtures(2, true)
	for i := range 2 {
		devices = append(devices, nicop.NicDevice{Status: nicop.NicDeviceStatus{
			Node: fmt.Sprintf("worker-%d", i), Type: "1023", PartNumber: "test",
			Ports: []nicop.NicDevicePortSpec{{PCI: "0000:20:00.0"}, {PCI: "0000:20:00.1"}},
		}})
	}
	groups, _, members := buildClusterConfigWithMembers(devices, nil, nil, true)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].PFs, 2)
	observations := map[string]nodeGPUObservation{}
	for _, node := range groups[0].WorkerNodes {
		observation := gpuParsedFixture(t)
		observation.host.rdma["0000:20:00.0"] = "mlx5_local2"
		observation.host.rdma["0000:20:00.1"] = "mlx5_local3"
		for i := range 4 {
			observation.data.matrix[fmt.Sprintf("mlx5_local%d", i)] = map[int]proximity{0: proxPHB, 1: proxPHB}
		}
		observations[node] = observation
	}
	identities := buildPFIdentities(groups, devices, members, true)
	applyGPUObservations(context.Background(), groups, identities, observations)
	assert.Equal(t, "GPU0", groups[0].PFs[0].ConnectedGPU)
	assert.Equal(t, "GPU1", groups[0].PFs[1].ConnectedGPU, "the second rail, not the first rail's sibling, consumes the second assignment")
}
