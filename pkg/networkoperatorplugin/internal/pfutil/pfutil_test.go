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

package pfutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestIsDualPortModel(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"cx7 2-port", "Nvidia ConnectX-7 NDR200/HDR QSFP112 2-port PCIe Gen5 x16 InfiniBand Adapter", true},
		{"cx8 dual-port", "NVIDIA ConnectX-8 C8240 HHHL SuperNIC, 400GbE / 400Gb/s IB, Dual-port QSFP112", true},
		{"spaced 2 port", "Some Adapter 2 port QSFP", true},
		{"spaced dual port", "Some Adapter Dual port QSFP", true},
		{"bf3 single 1P", "Nvidia BlueField-3 VPI QSFP112 1P 400G PCIe Gen5 x16", false},
		{"single-port", "ConnectX-7 Single-port QSFP112", false},
		{"qsfp112 no false positive", "Nvidia ConnectX-7 QSFP112 PCIe Gen5 x16 Adapter", false},
		{"digit-adjacent token no false positive", "Some NIC Gen2 port adapter", false},
		{"qsfp112 followed by 2-port still matches", "Nvidia ConnectX-7 QSFP112 2-port Adapter", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsDualPortModel(tc.model))
		})
	}
}

func TestCollapsePFsToOnePerNIC(t *testing.T) {
	t.Run("multi-plane NIC collapses to master", func(t *testing.T) {
		// Two PFs on one NIC (same bus:device), no port-count keyword in model
		// → planes of one rail → keep the master (.0) only.
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.0", Model: "ConnectX-8 SuperNIC"},
			{PciAddress: "0000:18:00.1", Model: "ConnectX-8 SuperNIC"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		require.Len(t, kept, 1)
		assert.Equal(t, "0000:18:00.0", kept[0].PciAddress)
		assert.Equal(t, 1, dropped)
	})

	t.Run("dual-port NIC keeps every port", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.0", Model: "ConnectX-7 QSFP112 2-port Adapter"},
			{PciAddress: "0000:18:00.1", Model: "ConnectX-7 QSFP112 2-port Adapter"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		assert.Len(t, kept, 2)
		assert.Equal(t, 0, dropped)
	})

	t.Run("master is lowest function even when listed out of order", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.1", Model: "multi-plane"},
			{PciAddress: "0000:18:00.0", Model: "multi-plane"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		require.Len(t, kept, 1)
		assert.Equal(t, "0000:18:00.0", kept[0].PciAddress)
		assert.Equal(t, 1, dropped)
	})

	t.Run("mixed NICs: one dual-port, one multi-plane", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.0", Model: "ConnectX-7 2-port"},
			{PciAddress: "0000:18:00.1", Model: "ConnectX-7 2-port"},
			{PciAddress: "0000:51:00.0", Model: "ConnectX-8 SuperNIC"},
			{PciAddress: "0000:51:00.1", Model: "ConnectX-8 SuperNIC"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		require.Len(t, kept, 3)
		got := []string{kept[0].PciAddress, kept[1].PciAddress, kept[2].PciAddress}
		assert.Equal(t, []string{"0000:18:00.0", "0000:18:00.1", "0000:51:00.0"}, got)
		assert.Equal(t, 1, dropped)
	})

	t.Run("three or more sibling PFs collapse to a single master", func(t *testing.T) {
		// A 4-plane NIC (e.g. ConnectX-9 hwplb) presents four function-split PFs
		// on one bus:device → all but the master are dropped.
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.0", Model: "ConnectX-9 SuperNIC"},
			{PciAddress: "0000:18:00.1", Model: "ConnectX-9 SuperNIC"},
			{PciAddress: "0000:18:00.2", Model: "ConnectX-9 SuperNIC"},
			{PciAddress: "0000:18:00.3", Model: "ConnectX-9 SuperNIC"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		require.Len(t, kept, 1)
		assert.Equal(t, "0000:18:00.0", kept[0].PciAddress)
		assert.Equal(t, 3, dropped)
	})

	t.Run("single-PF NICs are unchanged", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:18:00.0", Model: "ConnectX-7 1P"},
			{PciAddress: "0000:51:00.0", Model: "ConnectX-7 1P"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		assert.Len(t, kept, 2)
		assert.Equal(t, 0, dropped)
	})

	t.Run("malformed PCI address is kept, never dropped", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "bogus", Model: "multi-plane"},
			{PciAddress: "0000:18:00.0", Model: "multi-plane"},
			{PciAddress: "0000:18:00.1", Model: "multi-plane"},
		}
		kept, dropped := CollapsePFsToOnePerNIC(pfs)
		require.Len(t, kept, 2)
		assert.Equal(t, "bogus", kept[0].PciAddress)
		assert.Equal(t, "0000:18:00.0", kept[1].PciAddress)
		assert.Equal(t, 1, dropped)
	})
}
