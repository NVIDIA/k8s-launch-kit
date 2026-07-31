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

package profiles

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestProfileValidation(t *testing.T) {
	// Helper to create bool pointers
	boolPtr := func(b bool) *bool { return &b }

	t.Run("validate Spectrum-X profile with matching requirements", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
			NodeCapabilities: NodeCapabilities{
				Sriov: boolPtr(true),
				Rdma:  boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
				Ib:    false,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid, "Profile should be valid with matching Spectrum-X requirements")
		assert.Empty(t, reason, "Should have no validation error")
	})

	t.Run("validate Spectrum-X profile without SpectrumX in requirements", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX:  nil, // No Spectrum-X
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid without Spectrum-X")
		assert.Contains(t, reason, "profile requires Spectrum-X but it is not enabled")
	})

	t.Run("validate Spectrum-X profile with wrong version", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "unsupported", // not the required version
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid with wrong Spectrum-X version")
		assert.Contains(t, reason, "profile requires SPCX version RA2.2 but got unsupported")
	})

	t.Run("validate non-SpectrumX profile without SpectrumX requirements", func(t *testing.T) {
		profile := &Profile{
			Name:   "SR-IOV Ethernet RDMA",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX:  nil, // No Spectrum-X required
			},
			NodeCapabilities: NodeCapabilities{
				Rdma: boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false,
			SpectrumX:  nil, // No Spectrum-X
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid, "Non-Spectrum-X profile should be valid without Spectrum-X requirements")
		assert.Empty(t, reason)
	})

	t.Run("non-SpectrumX profile rejects user requesting Spectrum-X", func(t *testing.T) {
		// Symmetric to the "profile requires Spectrum-X but it is not enabled"
		// check: a user who explicitly opted into Spectrum-X must not be
		// silently routed to a non-Spectrum-X profile (which would render an
		// SR-IOV-only manifest set without the SpectrumXRailPoolConfig).
		profile := &Profile{
			Name:   "SR-IOV Ethernet RDMA",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX:  nil, // No Spectrum-X required
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Non-Spectrum-X profile must be rejected when user enabled Spectrum-X")
		assert.Contains(t, reason, "user requested Spectrum-X but this profile is not a Spectrum-X profile")
	})

	t.Run("validate profile with mismatched fabric", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "infiniband", // Wrong fabric
			Deployment: "sriov",
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid with mismatched fabric")
		assert.Contains(t, reason, "selected fabric type does not match profile requirements")
	})

	t.Run("validate profile with mismatched deployment type", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "rdma_shared", // Wrong deployment type
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid with mismatched deployment type")
		assert.Contains(t, reason, "selected deployment type does not match profile requirements")
	})

	t.Run("validate profile with mismatched multirail", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false, // Wrong multirail setting
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid with mismatched multirail setting")
		assert.Contains(t, reason, "selected multirail setting does not match profile requirements")
	})

	t.Run("validate multiplaneMode matching - hwplb matches profile allowing hwplb", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: []string{"hwplb", "none"},
				},
			},
			NodeCapabilities: NodeCapabilities{
				Sriov: boolPtr(true),
				Rdma:  boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid, "Profile should match hwplb mode")
		assert.Empty(t, reason)
	})

	t.Run("validate multiplaneMode matching - swplb rejected by non-swplb profile", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: []string{"hwplb", "none"},
				},
			},
			NodeCapabilities: NodeCapabilities{
				Sriov: boolPtr(true),
				Rdma:  boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should reject swplb mode when only hwplb/none are allowed")
		assert.Contains(t, reason, "multiplane mode swplb not in profile's allowed modes")
	})

	t.Run("validate multiplaneMode matching - swplb matches swplb-only profile", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail SWPLB",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  boolPtr(true),
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: []string{"swplb"},
				},
			},
			NodeCapabilities: NodeCapabilities{
				Sriov: boolPtr(true),
				Rdma:  boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid, "SWPLB profile should match swplb mode")
		assert.Empty(t, reason)
	})

	t.Run("validate multiplaneMode matching - no mode constraint accepts any mode", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
					// MultiplaneMode not set - should accept any mode
				},
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: true,
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid, "Profile with no mode constraint should accept any multiplane mode")
		assert.Empty(t, reason)
	})

	t.Run("validate profile with mismatched node capabilities", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X Multi-Rail",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				Fabric:     "ethernet",
				Deployment: "sriov",
				SpectrumX: &ProfileRequirementsSpectrumX{
					SPCXVersion: "RA2.2",
				},
			},
			NodeCapabilities: NodeCapabilities{
				Sriov: boolPtr(true),
				Rdma:  boolPtr(true),
			},
		}

		requirements := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}

		capabilities := &config.ClusterCapabilities{
			Nodes: &config.NodesCapabilities{
				Sriov: false, // Cluster doesn't support SR-IOV
				Rdma:  true,
			},
		}

		valid, reason := profile.Validate(requirements, capabilities, "")
		assert.False(t, valid, "Profile should not be valid with mismatched node capabilities")
		assert.Contains(t, reason, "cluster sriov capability does not match profile requirements")
	})

	t.Run("MinNetworkOperatorRelease rejects older selected release", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MinNetworkOperatorRelease: "26.4",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, reason := profile.Validate(requirements, capabilities, "26.1")
		assert.False(t, valid)
		assert.Contains(t, reason, "Network Operator >= 26.4")
	})

	t.Run("MinNetworkOperatorRelease accepts equal release", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MinNetworkOperatorRelease: "26.4",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, _ := profile.Validate(requirements, capabilities, "26.4")
		assert.True(t, valid)
	})

	t.Run("MinNetworkOperatorRelease ignored when no release pinned", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MinNetworkOperatorRelease: "26.4",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		// Empty selectedRelease = "latest" — gate disabled.
		valid, _ := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid)
	})

	t.Run("MaxNetworkOperatorRelease rejects newer selected release", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X RA2.1",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MaxNetworkOperatorRelease: "26.1",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, reason := profile.Validate(requirements, capabilities, "26.4")
		assert.False(t, valid)
		assert.Contains(t, reason, "Network Operator <= 26.1")
	})

	t.Run("MaxNetworkOperatorRelease accepts equal release", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X RA2.1",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MaxNetworkOperatorRelease: "26.1",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, _ := profile.Validate(requirements, capabilities, "26.1")
		assert.True(t, valid)
	})

	t.Run("MaxNetworkOperatorRelease ignored when no release pinned", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X RA2.1",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MaxNetworkOperatorRelease: "26.1",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, _ := profile.Validate(requirements, capabilities, "")
		assert.True(t, valid)
	})

	t.Run("Min and Max together pin a single release", func(t *testing.T) {
		profile := &Profile{
			Name:   "Spectrum-X RA2.1",
			Plugin: "network-operator",
			ProfileRequirements: ProfileRequirements{
				MinNetworkOperatorRelease: "26.1",
				MaxNetworkOperatorRelease: "26.1",
			},
		}
		requirements := &config.Profile{}
		capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{}}

		valid, _ := profile.Validate(requirements, capabilities, "26.1")
		assert.True(t, valid, "26.1 should match the single-version pin")

		valid, reason := profile.Validate(requirements, capabilities, "25.10")
		assert.False(t, valid, "25.10 below pin should be rejected")
		assert.Contains(t, reason, ">= 26.1")

		valid, reason = profile.Validate(requirements, capabilities, "26.4")
		assert.False(t, valid, "26.4 above pin should be rejected")
		assert.Contains(t, reason, "<= 26.1")
	})
}
