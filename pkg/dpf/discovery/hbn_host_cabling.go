// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// HostLLDPCapturer captures one already-selected host adapter using that
// target's resolved SSH trust and authentication.
type HostLLDPCapturer interface {
	CaptureHostLLDP(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error)
}

// CollectHostHBNCabling obtains bounded, passive host LLDP observations for
// selected bindings unresolved by DPU MAC and switch LLDP observations. It
// updates only transient observations on the supplied current inventory.
func CollectHostHBNCabling(ctx context.Context, request config.Request, inv *inventory.Inventory, capturer HostLLDPCapturer) {
	if ctx == nil || inv == nil || inv.HBN == nil || len(inv.DPUs) == 0 {
		return
	}
	output := ui.FromContext(ctx)
	needed := map[string]bool{}
	for _, target := range request.Switches {
		var sw *inventory.FabricSwitch
		for i := range inv.HBN.Switches {
			candidate := &inv.HBN.Switches[i]
			if candidate.Name == target.Name && candidate.Endpoint == target.Endpoint {
				if sw != nil {
					sw = nil
					break
				}
				sw = candidate
			}
		}
		for _, binding := range target.Ports {
			if containsDPU(inv.DPUs, binding.DPUSerial) && (sw == nil || hbnCablingCheck(inv, *sw, binding).State == "unknown") {
				needed[binding.DPUSerial] = true
			}
		}
	}
	completed := map[string]bool{}
	for i := range inv.DPUs {
		dpu := &inv.DPUs[i]
		if !needed[dpu.SerialNumber] {
			continue
		}
		_, host, adapter, state, _, _ := resolveHostHBNAssociation(inv, *dpu)
		if state != "passed" || host == nil || adapter == nil {
			continue
		}
		key := host.ID + "\x00" + adapter.Key
		if completed[key] {
			continue
		}
		completed[key] = true
		fail := func(code, detail string) {
			output.Info("[UNAVAILABLE] Host %q: DPU %q LLDP capture (%s): %s", hostDisplayName(*host), dpu.SerialNumber, code, detail)
			if host.Runtime == nil {
				host.Runtime = &inventory.RuntimeObservation{}
			}
			if host.Runtime.Observations == nil {
				host.Runtime.Observations = map[string]inventory.Observation{}
			}
			host.Runtime.Observations["hbn-host-lldp"] = inventory.Observation{State: "unavailable", ReasonCode: code, Message: detail}
		}
		if capturer == nil {
			fail("HOST_LLDP_COLLECTOR_UNAVAILABLE", "No authenticated host LLDP collector was configured.")
			continue
		}
		target, targetState := selectedHostSSH(request.Targets, *host)
		if targetState != "passed" {
			fail("HOST_LLDP_ACCESS_UNAVAILABLE", "The exact selected host SSH endpoint and source reference are not available for a fresh LLDP read.")
			continue
		}
		if host.Runtime == nil || host.Runtime.Network == nil || host.Runtime.Network.Links == nil || runtimeObservation(*host, "network-links").State != "observed" {
			observation := runtimeObservation(*host, "network-links")
			fail(firstReason(observation.ReasonCode, "HOST_LINKS_UNOBSERVED"), "A complete current host link list is required before selecting interfaces for LLDP capture.")
			continue
		}
		captureRequest, captureErr := selectedHostLLDPRequest(*host, *adapter)
		if captureErr != nil {
			fail("HOST_LLDP_PORT_MAPPING_UNAVAILABLE", "The selected adapter does not have two uniquely mapped PCI functions and interfaces for fresh LLDP capture.")
			continue
		}
		output.Info("[RUNNING] Host %q: receive LLDP on %d selected DPU interfaces with bounded packet captures", hostDisplayName(*host), len(captureRequest.Ports))
		capture, captureErr := capturer.CaptureHostLLDP(ctx, target, captureRequest, request.RequestTimeout)
		if captureErr != nil {
			fail(sshprobe.Code(captureErr), sshprobe.Reason(captureErr))
			continue
		}
		if err := validateHostLLDPCapture(captureRequest, capture); err != nil || !captureMatchesHostLinks(*host, capture) {
			fail("HOST_LLDP_CAPTURE_INCONSISTENT", "The LLDP collector returned facts outside the selected adapter and port request.")
			continue
		}
		if host.Runtime == nil {
			host.Runtime = &inventory.RuntimeObservation{}
		}
		if host.Runtime.Network == nil {
			host.Runtime.Network = &inventory.NetworkObservation{}
		}
		copy := cloneHostLLDPCapture(capture)
		host.Runtime.Network.HostLLDP = &copy
		for _, port := range capture.Ports {
			if port.State == "observed" {
				output.Info("[PASS] Host %q: LLDP peer received on %s (%s); cabling comparison follows", hostDisplayName(*host), port.Interface, port.PhysicalPortName)
			} else {
				output.Info("[UNAVAILABLE] Host %q: LLDP on %s (%s): %s", hostDisplayName(*host), port.Interface, port.ReasonCode, port.Message)
			}
		}
		if host.Runtime.Observations == nil {
			host.Runtime.Observations = map[string]inventory.Observation{}
		}
		host.Runtime.Observations["hbn-host-lldp"] = inventory.Observation{State: "observed"}
	}
}

func captureMatchesHostLinks(host inventory.Device, capture inventory.HostLLDPCapture) bool {
	if host.Runtime == nil || host.Runtime.Network == nil {
		return false
	}
	links := make(map[string]inventory.NetworkLink, len(host.Runtime.Network.Links))
	for _, link := range host.Runtime.Network.Links {
		links[link.Name] = link
	}
	for _, port := range capture.Ports {
		link, ok := links[port.Interface]
		if !ok || link.PCIAddress != "" && !strings.EqualFold(link.PCIAddress, port.PCIAddress) || link.PhysicalPortName != "" && link.PhysicalPortName != port.PhysicalPortName {
			return false
		}
	}
	return true
}

func selectedHostSSH(targets []config.Target, host inventory.Device) (config.Target, string) {
	var selected config.Target
	count := 0
	for _, target := range targets {
		if target.Role == "hosts" && target.InputRef == host.InputRef && target.Endpoint == host.SSHEndpoint {
			selected, count = target, count+1
		}
	}
	if host.InputRef == "" || host.SSHEndpoint == "" || count != 1 || selected.SSH == nil {
		return config.Target{}, "unknown"
	}
	return selected, "passed"
}

func selectedHostLLDPRequest(host inventory.Device, adapter inventory.Adapter) (inventory.HostLLDPCaptureRequest, error) {
	if host.SystemUUID == "" || adapter.Key == "" || adapter.SerialNumber == "" || adapter.PCIAddress == "" || host.Runtime == nil || host.Runtime.Network == nil || host.Runtime.Network.Links == nil || runtimeObservation(host, "network-links").State != "observed" {
		return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("selected host identity or link inventory is incomplete")
	}
	if len(adapter.PCIFunctions) != 2 {
		return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("selected host adapter does not expose exactly two PCI functions")
	}
	links := make(map[string]inventory.NetworkLink, len(host.Runtime.Network.Links))
	for _, link := range host.Runtime.Network.Links {
		if link.Name == "" {
			return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("host network link has no name")
		}
		if _, exists := links[link.Name]; exists {
			return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("host network link name is ambiguous")
		}
		links[link.Name] = link
	}
	request := inventory.HostLLDPCaptureRequest{HostUUID: host.SystemUUID, AdapterKey: adapter.Key, AdapterSerial: adapter.SerialNumber, AdapterPCIAddress: adapter.PCIAddress, Ports: make([]inventory.HostLLDPCapturePort, 0, 2)}
	seenBDF, seenInterface := map[string]bool{}, map[string]bool{}
	for _, function := range adapter.PCIFunctions {
		if function.Address == "" || len(function.Netdevs) != 1 || seenBDF[strings.ToLower(function.Address)] {
			return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("host PCI function does not map to one unique interface")
		}
		iface := function.Netdevs[0]
		if iface == "" || seenInterface[iface] {
			return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("host interface is empty or duplicated")
		}
		link, exists := links[iface]
		if !exists || link.PCIAddress != "" && !strings.EqualFold(link.PCIAddress, function.Address) {
			return inventory.HostLLDPCaptureRequest{}, fmt.Errorf("selected interface does not match the current PCI function inventory")
		}
		seenBDF[strings.ToLower(function.Address)], seenInterface[iface] = true, true
		request.Ports = append(request.Ports, inventory.HostLLDPCapturePort{PCIAddress: strings.ToLower(function.Address), Interface: iface})
	}
	slices.SortFunc(request.Ports, func(a, b inventory.HostLLDPCapturePort) int { return strings.Compare(a.PCIAddress, b.PCIAddress) })
	return request, nil
}

func validateHostLLDPCapture(request inventory.HostLLDPCaptureRequest, capture inventory.HostLLDPCapture) error {
	if len(capture.Ports) != len(request.Ports) {
		return fmt.Errorf("host LLDP result port count differs from request")
	}
	wanted := map[string]inventory.HostLLDPCapturePort{}
	for _, port := range request.Ports {
		wanted[port.Interface] = port
	}
	seenInterface, physicalCounts := map[string]bool{}, map[string]int{}
	for _, result := range capture.Ports {
		port, ok := wanted[result.Interface]
		if !ok || seenInterface[result.Interface] || !strings.EqualFold(result.PCIAddress, port.PCIAddress) || len(result.PhysicalPortName) > 32 || strings.ContainsAny(result.PhysicalPortName, "\x00\r\n \t") || result.State != "observed" && result.State != "unknown" && result.State != "failed" || result.ReasonCode == "" || result.Message == "" || result.Neighbors == nil {
			return fmt.Errorf("host LLDP result has invalid or ambiguous port facts")
		}
		if result.State == "observed" && result.PhysicalPortName != "p0" && result.PhysicalPortName != "p1" {
			return fmt.Errorf("observed host LLDP port has no supported physical port name")
		}
		seenInterface[result.Interface] = true
		if result.PhysicalPortName == "p0" || result.PhysicalPortName == "p1" {
			physicalCounts[result.PhysicalPortName]++
		}
		for _, neighbor := range result.Neighbors {
			if neighbor.SourceMAC == "" || neighbor.ChassisID == "" || neighbor.ChassisIDSubtype == 0 || neighbor.PortID == "" || neighbor.PortIDSubtype == 0 {
				return fmt.Errorf("host LLDP result has incomplete neighbor identity")
			}
		}
	}
	for _, result := range capture.Ports {
		if physicalCounts[result.PhysicalPortName] > 1 && (result.State != "failed" || result.ReasonCode != "HOST_PHYSICAL_PORT_AMBIGUOUS") {
			return fmt.Errorf("duplicate host physical port lacks an explicit failure result")
		}
	}
	if physicalCounts["p0"] == 0 || physicalCounts["p1"] == 0 {
		unmeasuredMapping := false
		for _, result := range capture.Ports {
			if result.State == "unknown" {
				unmeasuredMapping = true
			}
		}
		if !unmeasuredMapping {
			return fmt.Errorf("host LLDP result does not identify both p0 and p1 interfaces")
		}
	}
	return nil
}

func cloneHostLLDPCapture(in inventory.HostLLDPCapture) inventory.HostLLDPCapture {
	out := inventory.HostLLDPCapture{Ports: slices.Clone(in.Ports)}
	for i := range out.Ports {
		out.Ports[i].Neighbors = slices.Clone(in.Ports[i].Neighbors)
	}
	return out
}

func hostHBNCablingCheck(inv *inventory.Inventory, sw *inventory.FabricSwitch, binding inventory.SwitchPort) inventory.Check {
	switchName := "unknown-switch"
	if sw != nil && sw.Name != "" {
		switchName = sw.Name
	}
	check := inventory.Check{ID: "dpf.hbn.cabling", Subject: switchName + "/" + binding.Name, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "HOST_LLDP_UNAVAILABLE", Message: "A same-run host-side LLDP peer could not be matched to the selected switch identity and port."}
	_, host, adapter, state, reason, message := resolveHostHBNAssociation(inv, selectedDPU(inv.DPUs, binding.DPUSerial))
	if state != "passed" || host == nil || adapter == nil {
		check.ReasonCode = reason
		check.Message = message
		return check
	}
	if host.Runtime == nil {
		return check
	}
	if observation := runtimeObservation(*host, "hbn-host-lldp"); observation.State != "observed" {
		check.ReasonCode = firstReason(observation.ReasonCode, "HOST_LLDP_UNAVAILABLE")
		if observation.Message != "" {
			check.Message = observation.Message
		}
		return check
	}
	if host.Runtime.Network == nil || host.Runtime.Network.HostLLDP == nil {
		check.ReasonCode, check.Message = "HOST_LLDP_CAPTURE_INCONSISTENT", "The successful host LLDP observation has no typed captured ports."
		return check
	}
	if sw == nil || sw.Snapshot.Operational.LocalLLDP == nil {
		check.ReasonCode, check.Message = "SWITCH_LLDP_IDENTITY_UNAVAILABLE", "The selected switch did not expose a current local LLDP chassis identity."
		return check
	}
	local := sw.Snapshot.Operational.LocalLLDP
	localChassis, err := normalizedUnicastMAC(local.ChassisID)
	if err != nil || strings.TrimSpace(local.SystemName) == "" {
		check.ReasonCode, check.Message = "SWITCH_LLDP_IDENTITY_INVALID", "The selected switch local LLDP chassis identity is malformed or incomplete."
		return check
	}
	var selected *inventory.HostLLDPCaptureResult
	for i := range host.Runtime.Network.HostLLDP.Ports {
		port := &host.Runtime.Network.HostLLDP.Ports[i]
		if port.PhysicalPortName != binding.DPUInterface {
			continue
		}
		if selected != nil {
			check.State, check.ReasonCode, check.Message = "failed", "HOST_PHYSICAL_PORT_AMBIGUOUS", "More than one selected host interface claims the configured DPU physical port."
			return check
		}
		selected = port
	}
	if selected == nil {
		for _, port := range host.Runtime.Network.HostLLDP.Ports {
			if port.State != "observed" && port.ReasonCode != "" {
				check.ReasonCode, check.Message = port.ReasonCode, port.Message
				return check
			}
		}
		check.ReasonCode, check.Message = "HOST_PHYSICAL_PORT_UNAVAILABLE", "The selected host adapter has no observed interface for the configured p0 or p1 uplink."
		return check
	}
	if selected.State != "observed" {
		check.State, check.ReasonCode, check.Message = selected.State, selected.ReasonCode, selected.Message
		if check.State != "failed" {
			check.State = "unknown"
		}
		return check
	}
	if len(selected.Neighbors) == 0 {
		check.ReasonCode, check.Message = "HOST_LLDP_PEER_UNOBSERVED", "No LLDP peer was observed on the selected host uplink during the capture window."
		return check
	}
	if len(selected.Neighbors) != 1 {
		check.State, check.ReasonCode, check.Message = "failed", "HOST_LLDP_PEER_AMBIGUOUS", "Conflicting LLDP peers were observed on the selected host uplink."
		return check
	}
	peer := selected.Neighbors[0]
	peerChassis, peerErr := normalizedUnicastMAC(peer.ChassisID)
	if peer.ChassisIDSubtype != 4 || peerErr != nil {
		check.ReasonCode, check.Message = "HOST_LLDP_CHASSIS_UNSUPPORTED", "The host LLDP peer does not expose a valid MAC chassis identifier."
		return check
	}
	if peerChassis != localChassis {
		check.State, check.ReasonCode, check.Message = "failed", "HOST_LLDP_SWITCH_MISMATCH", "The host LLDP peer chassis does not match the selected switch's current chassis identity."
		return check
	}
	if peer.SystemName != "" && peer.SystemName != local.SystemName {
		check.State, check.ReasonCode, check.Message = "failed", "HOST_LLDP_SWITCH_NAME_MISMATCH", "The host LLDP peer system name disagrees with the selected switch identity."
		return check
	}
	if peer.PortIDSubtype != 5 || peer.PortID != binding.Name {
		check.State, check.ReasonCode, check.Message = "failed", "HOST_LLDP_SWITCH_PORT_MISMATCH", "The host LLDP peer port does not match the configured switch port binding."
		return check
	}
	check.State, check.ReasonCode, check.Message = "passed", "HOST_LLDP_CABLING_CORROBORATED", "The selected host PCI physical port received LLDP from the selected switch chassis and configured switch port; this does not attest DPU-side assignment or packet delivery."
	return check
}

// combineHBNCablingCheck allows the identity-bound host path to resolve an
// incomplete DPU-MAC path, while preserving any known base success or failure.
func combineHBNCablingCheck(base, host inventory.Check) inventory.Check {
	if base.State != "unknown" {
		return base
	}
	if host.ID != base.ID || host.Subject != base.Subject || host.RequiredPhase != base.RequiredPhase || host.State == "" || host.ReasonCode == "" || host.Message == "" {
		return base
	}
	return host
}

func selectedDPU(dpus []inventory.Device, serial string) inventory.Device {
	for _, dpu := range dpus {
		if dpu.SerialNumber == serial {
			return dpu
		}
	}
	return inventory.Device{SerialNumber: serial}
}

func containsDPU(dpus []inventory.Device, serial string) bool {
	count := 0
	for _, dpu := range dpus {
		if dpu.SerialNumber == serial {
			count++
		}
	}
	return count == 1
}

func normalizedUnicastMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(strings.TrimSpace(raw))
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || slices.Equal(mac, []byte{0, 0, 0, 0, 0, 0}) {
		return "", fmt.Errorf("invalid unicast MAC")
	}
	return strings.ToLower(mac.String()), nil
}
