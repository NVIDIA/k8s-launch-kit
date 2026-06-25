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

package discovery

import (
	"testing"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// nicDeviceWithPorts builds a NicDevice CR with two function-split ports on a
// single NIC (bus:device prefix), as the discovery daemon would publish for a
// multi-plane / dual-port adapter.
func nicDeviceWithPorts(node, deviceID, partNumber, model, busDevice string) nicop.NicDevice {
	return nicop.NicDevice{
		Status: nicop.NicDeviceStatus{
			Node:       node,
			Type:       deviceID,
			PartNumber: partNumber,
			PSID:       "psid-test",
			ModelName:  model,
			Ports: []nicop.NicDevicePortSpec{
				{PCI: busDevice + ".0", RdmaInterface: "mlx5_0", NetworkInterface: "eth0"},
				{PCI: busDevice + ".1", RdmaInterface: "mlx5_1", NetworkInterface: "eth1"},
			},
		},
	}
}

func eastWestPFsOf(group config.ClusterConfig) []config.PFConfig {
	var out []config.PFConfig
	for _, pf := range group.PFs {
		if pf.Traffic == "east-west" {
			out = append(out, pf)
		}
	}
	return out
}

func TestBuildClusterConfig_CollapseNicRails(t *testing.T) {
	// One node, two ConnectX-8 NICs, each presenting two function-split PFs
	// (multi-plane). deviceID 1023 is not a DPU and < 5 PFs, so the part-number
	// frequency heuristic doesn't run — all four PFs default to east-west.
	devices := []nicop.NicDevice{
		nicDeviceWithPorts("worker-0", "1023", "pn-test-rdma", "ConnectX-8 SuperNIC", "0000:18:00"),
		nicDeviceWithPorts("worker-0", "1023", "pn-test-rdma", "ConnectX-8 SuperNIC", "0000:51:00"),
	}

	t.Run("collapse on: one rail per NIC", func(t *testing.T) {
		groups, _ := buildClusterConfig(devices, nil, nil, true)
		require.Len(t, groups, 1)
		ew := eastWestPFsOf(groups[0])
		require.Len(t, ew, 2, "two multi-plane NICs collapse to two east-west PFs")
		// Rails numbered 0,1 over the collapsed set; the surviving PFs are the
		// masters (.0) of each NIC, and Model is carried through.
		require.NotNil(t, ew[0].Rail)
		require.NotNil(t, ew[1].Rail)
		assert.Equal(t, 0, *ew[0].Rail)
		assert.Equal(t, 1, *ew[1].Rail)
		assert.Equal(t, "0000:18:00.0", ew[0].PciAddress)
		assert.Equal(t, "0000:51:00.0", ew[1].PciAddress)
		assert.Equal(t, "ConnectX-8 SuperNIC", ew[0].Model)
	})

	t.Run("collapse off: one rail per PF", func(t *testing.T) {
		groups, _ := buildClusterConfig(devices, nil, nil, false)
		require.Len(t, groups, 1)
		ew := eastWestPFsOf(groups[0])
		require.Len(t, ew, 4, "legacy behaviour keeps every PF as its own rail")
		for i, pf := range ew {
			require.NotNil(t, pf.Rail)
			assert.Equal(t, i, *pf.Rail)
		}
	})

	t.Run("dual-port model keeps every port even with collapse on", func(t *testing.T) {
		dualPort := []nicop.NicDevice{
			nicDeviceWithPorts("worker-0", "1021", "pn-test-rdma",
				"Nvidia ConnectX-7 NDR200/HDR QSFP112 2-port PCIe Gen5 x16 InfiniBand Adapter", "0000:18:00"),
		}
		groups, _ := buildClusterConfig(dualPort, nil, nil, true)
		require.Len(t, groups, 1)
		ew := eastWestPFsOf(groups[0])
		require.Len(t, ew, 2, "a genuine dual-port NIC keeps a rail per port")
		assert.Equal(t, 0, *ew[0].Rail)
		assert.Equal(t, 1, *ew[1].Rail)
	})
}

// nsBF4PartNumber is a BlueField-4 DPU product code present in ns-product-ids,
// so its PFs classify as north-south.
const nsBF4PartNumber = "900-9D4B4-00CV-SA0"

func TestBuildClusterConfig_DropsNorthSouthOnlyGroups(t *testing.T) {
	t.Run("north-south-only group dropped, east-west group kept", func(t *testing.T) {
		devices := []nicop.NicDevice{
			// BlueField-4 DPU on its own node → both PFs north-south → group dropped.
			nicDeviceWithPorts("worker-ns", "a2d6", nsBF4PartNumber, "BlueField-4 DPU", "0000:0d:00"),
			// ConnectX-8 SuperNIC on another node → east-west → group kept.
			nicDeviceWithPorts("worker-ew", "1023", "pn-test-rdma", "ConnectX-8 SuperNIC", "0000:18:00"),
		}
		groups, _ := buildClusterConfig(devices, nil, nil, true)
		require.Len(t, groups, 1, "the north-south-only group must be dropped")
		require.NotEmpty(t, eastWestPFsOf(groups[0]))
		// Single remaining group → empty identifier (numbering recomputed after drop).
		assert.Equal(t, "", groups[0].Identifier)
		// No PF from the dropped DPU group leaked into the kept group.
		for _, g := range groups {
			for _, pf := range g.PFs {
				assert.NotEqual(t, nsBF4PartNumber, pf.PartNumber)
			}
		}
	})

	t.Run("cluster with only north-south NICs yields no groups", func(t *testing.T) {
		devices := []nicop.NicDevice{
			nicDeviceWithPorts("worker-ns", "a2d6", nsBF4PartNumber, "BlueField-4 DPU", "0000:0d:00"),
		}
		groups, _ := buildClusterConfig(devices, nil, nil, true)
		assert.Empty(t, groups, "a cluster with only north-south NICs yields no groups")
	})

	t.Run("mixed group is kept and its north-south PFs are recorded", func(t *testing.T) {
		// One node with both an east-west SuperNIC and a north-south DPU. The
		// group has east-west PFs, so it is kept — and the DPU's north-south
		// PFs must still appear in the saved PFs slice.
		devices := []nicop.NicDevice{
			nicDeviceWithPorts("worker-mixed", "1023", "pn-test-rdma", "ConnectX-8 SuperNIC", "0000:18:00"),
			nicDeviceWithPorts("worker-mixed", "a2d6", nsBF4PartNumber, "BlueField-4 DPU", "0000:0d:00"),
		}
		groups, _ := buildClusterConfig(devices, nil, nil, true)
		require.Len(t, groups, 1, "the mixed group must be kept")

		var ew, ns int
		for _, pf := range groups[0].PFs {
			switch pf.Traffic {
			case "east-west":
				ew++
			case "north-south":
				ns++
			}
		}
		assert.Positive(t, ew, "mixed group must keep its east-west PFs")
		assert.Positive(t, ns, "mixed group must still record its north-south PFs")
	})
}
