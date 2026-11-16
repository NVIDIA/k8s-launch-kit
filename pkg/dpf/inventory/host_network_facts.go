// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// MaxHostManagementRoots bounds the complete relevant-interface ancestry read,
// including its interface visits, PCI-parent visits and published root rows.
const MaxHostManagementRoots = 64

var hostBootID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validateHostNetworkFacts(w DeviceOutput, role string) error {
	if w.Runtime != nil {
		r := w.Runtime
		if r.VirtualMachine != nil && (role != "hosts" || w.Method != "ssh" || (hasIssue(w.Issues, "runtime.host-virtual-machine") || hasIssue(w.Issues, "runtime.virtualMachine"))) {
			return fmt.Errorf("device %s has invalid or conflicting host VM classification", w.RecordKey)
		}
		if r.BootID != "" && (role != "hosts" || w.Method != "ssh" || !hostBootID.MatchString(r.BootID) || (hasIssue(w.Issues, "runtime.host-boot-id") || hasIssue(w.Issues, "runtime.bootID"))) {
			return fmt.Errorf("device %s has invalid or conflicting host boot ID", w.RecordKey)
		}
		if r.SSHServerAddress != "" {
			ip, err := netip.ParseAddr(r.SSHServerAddress)
			if role != "hosts" || w.Method != "ssh" || err != nil || ip.Zone() != "" || ip.IsUnspecified() || (hasIssue(w.Issues, "runtime.ssh-server-address") || hasIssue(w.Issues, "runtime.sshServerAddress")) {
				return fmt.Errorf("device %s has invalid or conflicting SSH server address", w.RecordKey)
			}
		}
		if r.Network != nil {
			names := map[string]bool{}
			total := 0
			for _, link := range r.Network.Links {
				if len(link.PhysicalPFRoots) == 0 {
					continue
				}
				if role != "hosts" || w.Method != "ssh" || link.Name == "" || names[link.Name] || hasIssue(w.Issues, "runtime.host-management-roots") {
					return fmt.Errorf("device %s has invalid or conflicting management roots", w.RecordKey)
				}
				names[link.Name] = true
				seen := map[string]bool{}
				for _, root := range link.PhysicalPFRoots {
					root = strings.ToLower(root)
					total++
					if !pciFunctionAddress.MatchString(root) || seen[root] || total > MaxHostManagementRoots {
						return fmt.Errorf("device %s has invalid or excessive management roots", w.RecordKey)
					}
					seen[root] = true
				}
			}
		}
	}
	for _, adapter := range w.Adapters {
		for _, pf := range adapter.PCIFunctions {
			if !pf.NetdevsAbsent {
				continue
			}
			if role != "hosts" || w.Method != "ssh" || len(pf.Netdevs) != 0 || !pciFunctionAddress.MatchString(pf.Address) || !pciFunctionAddress.MatchString(adapter.PCIAddress) || !strings.EqualFold(pf.Address[:10], adapter.PCIAddress[:10]) || hasIssue(w.Issues, "runtime.host-pf:"+pf.Address+":netdevs") {
				return fmt.Errorf("device %s has invalid or conflicting measured PCI netdev absence", w.RecordKey)
			}
		}
	}
	return nil
}
