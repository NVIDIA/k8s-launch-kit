// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

var vrPCIAddress = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f]\.[0-7]$`)

// vrNICAddress identifies an ASIC across its firmware-split PCI functions.
func vrNICAddress(address string) (string, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if !vrPCIAddress.MatchString(address) {
		return "", fmt.Errorf("invalid PCI address %q", address)
	}
	return address[:len(address)-2], nil
}

// assignVRRails runs after traffic classification and preset enrichment. NetworkBay
// identifies physical siblings; the VR wiring convention pairs ASICs across bays.
// Validate every worker before changing any group, and retain PF ordering: this
// function assigns rail membership, not an index within the rail.
func assignVRRails(groups []config.ClusterConfig, devices []nicop.NicDevice) error {
	layouts := make(map[int]map[string]int)
	for i, group := range groups {
		if config.SpectrumXPlatformForGroup(group) != "vr" {
			continue
		}
		if len(group.WorkerNodes) == 0 {
			return fmt.Errorf("VR rail discovery for group %q: no worker nodes", group.Identifier)
		}
		for _, node := range group.WorkerNodes {
			layout, err := vrRailsForNode(group.PFs, devices, node)
			if err != nil {
				return fmt.Errorf("VR rail discovery for group %q, node %q: %w", group.Identifier, node, err)
			}
			if previous, exists := layouts[i]; exists && !maps.Equal(previous, layout) {
				return fmt.Errorf("VR rail discovery for group %q: node %q has a different NetworkBay rail layout from node %q",
					group.Identifier, node, group.WorkerNodes[0])
			}
			layouts[i] = layout
		}
	}
	for i, layout := range layouts {
		for j := range groups[i].PFs {
			pf := &groups[i].PFs[j]
			if pf.Traffic != "east-west" {
				continue
			}
			// Already validated for every worker above.
			nic, _ := vrNICAddress(pf.PciAddress)
			rail := layout[nic]
			pf.Rail = &rail
		}
	}
	return nil
}

func vrRailsForNode(pfs []config.PFConfig, devices []nicop.NicDevice, node string) (map[string]int, error) {
	selected := make(map[string]bool)
	for _, pf := range pfs {
		if pf.Traffic != "east-west" {
			continue
		}
		nic, err := vrNICAddress(pf.PciAddress)
		if err != nil {
			return nil, err
		}
		selected[nic] = true
	}
	if len(selected) != 8 {
		return nil, fmt.Errorf("expected eight east-west ASIC NICs in four NetworkBays, found %d", len(selected))
	}

	bays := make(map[string]nicop.NicDeviceNetworkBayStatus)
	for _, device := range devices {
		if device.Status.Node != node || len(device.Status.Ports) == 0 {
			continue
		}
		nic, err := vrNICAddress(device.Status.Ports[0].PCI)
		if err != nil || !selected[nic] {
			continue
		}
		if _, exists := bays[nic]; exists {
			return nil, fmt.Errorf("duplicate NicDevice for PCI NIC %s", nic)
		}
		if device.Status.NetworkBay == nil {
			return nil, fmt.Errorf("PCI NIC %s has no NetworkBay status; VR discovery requires a NIC Configuration Operator daemon and installed NicDevice CRD that publish networkBay", nic)
		}
		bays[nic] = *device.Status.NetworkBay
	}

	// The first ASIC is MGIR/status ASIC 1; the second is ASIC 0. Number
	// NetworkBays by first-ASIC PCI, independently of the sibling's PCI address.
	firstASICs := make([]string, 0, 4)
	for _, nic := range slices.Sorted(maps.Keys(selected)) {
		bay, exists := bays[nic]
		if !exists {
			return nil, fmt.Errorf("no NicDevice status for east-west PCI NIC %s", nic)
		}
		if bay.Asic != 0 && bay.Asic != 1 {
			return nil, fmt.Errorf("PCI NIC %s has invalid NetworkBay ASIC %d (expected 0 or 1)", nic, bay.Asic)
		}
		peer, err := vrNICAddress(bay.PeerPCI)
		if err != nil {
			return nil, fmt.Errorf("PCI NIC %s has invalid NetworkBay peer: %w", nic, err)
		}
		peerBay, exists := bays[peer]
		if !exists || peer == nic {
			return nil, fmt.Errorf("PCI NIC %s requires a distinct east-west NetworkBay peer %s on the same node", nic, peer)
		}
		back, err := vrNICAddress(peerBay.PeerPCI)
		if err != nil || back != nic || peerBay.Asic != 1-bay.Asic {
			return nil, fmt.Errorf("PCI NIC %s and peer %s must be reciprocal NetworkBay siblings with complementary ASICs 1 and 0", nic, peer)
		}
		if bay.Asic == 1 {
			firstASICs = append(firstASICs, nic)
		}
	}
	if len(firstASICs) != 4 {
		return nil, fmt.Errorf("expected four complete NetworkBays, found %d first ASICs", len(firstASICs))
	}

	layout := make(map[string]int, 8)
	for i := 0; i < len(firstASICs); i += 2 {
		first, next := firstASICs[i], firstASICs[i+1]
		firstPeer, _ := vrNICAddress(bays[first].PeerPCI)
		nextPeer, _ := vrNICAddress(bays[next].PeerPCI)
		layout[first], layout[nextPeer] = i, i
		layout[firstPeer], layout[next] = i+1, i+1
	}
	return layout, nil
}
