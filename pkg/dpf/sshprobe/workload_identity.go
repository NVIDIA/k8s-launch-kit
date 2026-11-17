// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"fmt"
	"net"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// SelectHostWorkloadNetworkIdentity binds the selected adapter's physical p0
// link from current host inventory. It never falls back to interface order,
// current MAC, or a guessed PCI function number.
func SelectHostWorkloadNetworkIdentity(host inventory.Device, adapterKey, adapterSerial string) (HostNetworkIdentity, error) {
	fail := func(reason string) (HostNetworkIdentity, error) {
		return HostNetworkIdentity{}, fmt.Errorf("host workload interface identity: %s", reason)
	}
	if !validUUID(host.SystemUUID) || strings.TrimSpace(adapterKey) == "" || strings.TrimSpace(adapterSerial) == "" {
		return fail("host and selected adapter identity are incomplete")
	}

	var selected *inventory.Adapter
	for i := range host.Adapters {
		adapter := &host.Adapters[i]
		keyMatches := adapter.Key == adapterKey
		serialMatches := adapter.SerialNumber == adapterSerial
		if !keyMatches && !serialMatches {
			continue
		}
		if !keyMatches || !serialMatches || selected != nil {
			return fail("adapter key and serial do not identify exactly one adapter")
		}
		selected = adapter
	}
	if selected == nil || selected.IdentityState != "known" || !externalPCIAddress.MatchString(selected.PCIAddress) || len(selected.PCIFunctions) == 0 {
		return fail("selected adapter has no known identity and valid PCI inventory")
	}
	rootSlot := pciSlot(strings.ToLower(selected.PCIAddress))
	functions := make(map[string]inventory.PCIFunction, len(selected.PCIFunctions))
	netdevOwners := make(map[string]string)
	rootFunctionFound := false
	for _, function := range selected.PCIFunctions {
		address := strings.ToLower(function.Address)
		if !externalPCIAddress.MatchString(address) || pciSlot(address) != rootSlot {
			return fail("selected adapter PCI functions are invalid or belong to another card slot")
		}
		if _, duplicate := functions[address]; duplicate {
			return fail("selected adapter has duplicate PCI function identities")
		}
		functions[address] = function
		rootFunctionFound = rootFunctionFound || strings.EqualFold(address, selected.PCIAddress)
		for _, name := range function.Netdevs {
			if !hostInterfaceName.MatchString(name) || strings.HasPrefix(name, "-") || name == "." || name == ".." {
				return fail("selected adapter has an invalid PCI-to-interface mapping")
			}
			if _, duplicate := netdevOwners[name]; duplicate {
				return fail("selected adapter has an ambiguous PCI-to-interface mapping")
			}
			netdevOwners[name] = address
		}
	}
	if !rootFunctionFound {
		return fail("selected adapter root PCI address is absent from its function inventory")
	}
	for i := range host.Adapters {
		adapter := &host.Adapters[i]
		if adapter == selected {
			continue
		}
		if externalPCIAddress.MatchString(adapter.PCIAddress) && pciSlot(strings.ToLower(adapter.PCIAddress)) == rootSlot {
			return fail("selected PCI slot is also assigned to another adapter")
		}
		for _, function := range adapter.PCIFunctions {
			if externalPCIAddress.MatchString(function.Address) && pciSlot(strings.ToLower(function.Address)) == rootSlot {
				return fail("selected PCI slot is also assigned to another adapter")
			}
			for _, name := range function.Netdevs {
				if _, claimed := netdevOwners[name]; claimed {
					return fail("selected adapter interface is also assigned to another adapter")
				}
			}
		}
	}

	if host.Runtime == nil || host.Runtime.Network == nil {
		return fail("current host network links are unavailable")
	}
	var selectedLink *inventory.NetworkLink
	seenSelectedLinkNames := make(map[string]struct{})
	for i := range host.Runtime.Network.Links {
		link := &host.Runtime.Network.Links[i]
		linkPCI := strings.ToLower(link.PCIAddress)
		functionAddress, mapped := netdevOwners[link.Name]
		pciMapped := false
		if linkPCI != "" {
			_, pciMapped = functions[linkPCI]
		}
		if !mapped && !pciMapped {
			continue
		}
		if _, duplicate := seenSelectedLinkNames[link.Name]; duplicate {
			return fail("selected adapter has duplicate runtime link names")
		}
		seenSelectedLinkNames[link.Name] = struct{}{}
		if link.PCIAddress == "" || !externalPCIAddress.MatchString(link.PCIAddress) || !mapped || functionAddress != linkPCI {
			return fail("selected adapter link has a missing or conflicting PCI-to-interface mapping")
		}
		if link.PhysicalPortName != "p0" {
			if link.PhysicalPortName == "" {
				return fail("selected adapter link has no explicit physical port identity")
			}
			continue
		}
		if selectedLink != nil {
			return fail("selected adapter has multiple p0 links")
		}
		selectedLink = link
	}
	if selectedLink == nil {
		return fail("selected adapter has no uniquely mapped p0 link")
	}
	function := functions[strings.ToLower(selectedLink.PCIAddress)]
	if function.Driver != "mlx5_core" || len(function.Netdevs) != 1 || function.Netdevs[0] != selectedLink.Name {
		return fail("selected p0 PCI function is not uniquely bound to one mlx5_core interface")
	}
	if selectedLink.Master != "" || selectedLink.Kind != "" && selectedLink.Kind != "ether" {
		return fail("selected p0 link is attached to a virtual or unsupported device")
	}
	permanentMAC, err := net.ParseMAC(selectedLink.PermanentMAC)
	if err != nil || len(permanentMAC) != 6 || permanentMAC[0]&1 != 0 || permanentMAC.String() == "00:00:00:00:00:00" {
		return fail("selected p0 permanent MAC is missing or invalid")
	}
	return HostNetworkIdentity{
		SystemUUID:    strings.ToLower(host.SystemUUID),
		AdapterSerial: adapterSerial,
		PCIAddress:    strings.ToLower(selectedLink.PCIAddress),
		InterfaceName: selectedLink.Name,
		PermanentMAC:  permanentMAC.String(),
		PhysicalPort:  "p0",
	}, nil
}
