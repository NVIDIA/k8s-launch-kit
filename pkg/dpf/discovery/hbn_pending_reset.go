// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"net"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

// ResumePendingResetHostChecks verifies only the current host observations needed
// to finish an owned pending reset. The caller must separately prove that exact
// operation and pending DPU request; these checks never qualify host networking.
func ResumePendingResetHostChecks(inv *inventory.Inventory, serial string) ([]inventory.Check, error) {
	if inv == nil {
		return nil, fmt.Errorf("pending reset inventory is missing")
	}
	var device *inventory.Device
	for i := range inv.DPUs {
		if inv.DPUs[i].SerialNumber == serial {
			if device != nil {
				return nil, fmt.Errorf("pending reset DPU is ambiguous")
			}
			device = &inv.DPUs[i]
		}
	}
	if device == nil {
		return nil, fmt.Errorf("pending reset DPU is missing")
	}
	_, host, adapter, state, _, _ := resolveHostHBNAssociation(inv, *device)
	if state != "passed" || host == nil || adapter == nil || len(adapter.PCIFunctions) != 2 {
		return nil, fmt.Errorf("pending reset host/adapter identity is incomplete")
	}
	if err := sshprobe.VerifyHostManagementPhysicalSeparation(*host, *adapter); err != nil {
		return nil, err
	}
	subject := serial + " @ " + hostDisplayName(*host)
	checks := []inventory.Check{}
	selectedPCI := map[string]bool{}
	for _, pf := range adapter.PCIFunctions {
		driver := runtimeObservation(*host, "host-pf:"+pf.Address+":driver")
		netdev := runtimeObservation(*host, "host-pf:"+pf.Address+":netdevs")
		if pf.Address == "" || selectedPCI[strings.ToLower(pf.Address)] || driver.State != "observed" || pf.Driver != "mlx5_core" || netdev.State != "observed" || !pf.NetdevsAbsent || len(pf.Netdevs) != 0 {
			return nil, fmt.Errorf("pending reset needs present exact PCI functions and positively measured netdev absence")
		}
		selectedPCI[strings.ToLower(pf.Address)] = true
		checks = append(checks, hostCheck("host-pf", subject+" / "+pf.Address, "passed", "OWNED_PENDING_RESET_PF_UNAVAILABLE", "Present selected PCI function has a measured absent network interface during the owned pending reset; this admits activation only, not host-network readiness."))
	}
	if err := pendingResetManagementSeparate(*host, selectedPCI); err != nil {
		return nil, err
	}
	checks = append(checks,
		hostCheck("host-management-separation", subject, "passed", "OWNED_PENDING_RESET_MANAGEMENT_SEPARATE", "Complete current host interfaces, PCI/link ancestry and IPv4/IPv6 routes are separate from the absent selected PFs; full post-activation checks remain required."),
		hostCheck("host-network-backend", subject, "passed", "OWNED_PENDING_RESET_BACKEND_DEFERRED", "The selected p0 backend cannot be measured while its interface is absent; only the owned activation may continue, followed by strict fresh backend verification before host-network effects."))
	return checks, nil
}

func pendingResetManagementSeparate(host inventory.Device, selectedPCI map[string]bool) error {
	if host.Runtime == nil || host.Runtime.Network == nil || host.Runtime.Interfaces == nil || host.Runtime.Routes == nil || host.Runtime.Network.Links == nil {
		return fmt.Errorf("pending reset host network facts are incomplete")
	}
	for _, key := range []string{"interfaces", "network-links", "network-link-details", "routes"} {
		if runtimeObservation(host, key).State != "observed" {
			return fmt.Errorf("pending reset needs complete current host network observations")
		}
	}
	interfaces := map[string]inventory.Interface{}
	indexes := map[int]string{}
	for _, iface := range host.Runtime.Interfaces {
		if iface.Name == "" || iface.Index < 1 || interfaces[iface.Name].Name != "" || indexes[iface.Index] != "" {
			return fmt.Errorf("pending reset host interface identity is ambiguous")
		}
		for _, value := range iface.Addresses {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return fmt.Errorf("pending reset host address is invalid")
			}
		}
		interfaces[iface.Name], indexes[iface.Index] = iface, iface.Name
	}
	links := map[string]inventory.NetworkLink{}
	for _, link := range host.Runtime.Network.Links {
		if link.Name == "" || links[link.Name].Name != "" || interfaces[link.Name].Name == "" || selectedPCI[strings.ToLower(link.PCIAddress)] {
			return fmt.Errorf("pending reset link graph is ambiguous or contradicts selected PF absence")
		}
		links[link.Name] = link
	}
	if len(links) != len(interfaces) {
		return fmt.Errorf("pending reset link and address snapshots disagree")
	}
	relevant, err := sshprobe.HostManagementInterfaces(host.Runtime)
	if err != nil {
		return err
	}
	for _, name := range relevant {
		seen := map[string]bool{}
		for current := name; current != ""; {
			link, found := links[current]
			if !found || seen[current] {
				return fmt.Errorf("pending reset management master ancestry is unresolved")
			}
			seen[current] = true
			current = link.Master
		}
	}
	for _, route := range host.Runtime.Routes {
		_, prefix, err := net.ParseCIDR(route.Destination)
		if err != nil {
			return fmt.Errorf("pending reset host route is invalid")
		}
		bits, _ := prefix.Mask.Size()
		if bits == 0 && route.InterfaceIndex != 0 && indexes[route.InterfaceIndex] == "" {
			return fmt.Errorf("pending reset default path is unresolved")
		}
	}
	return nil
}
