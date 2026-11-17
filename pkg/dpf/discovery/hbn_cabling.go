// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"net"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// hbnCablingCheck corroborates LLDP against MAC ownership read from the selected
// physical card. LLDP remains an unauthenticated port claim; this is not proof of
// a Redfish current physical-port assignment or of end-to-end packet delivery.
func hbnCablingCheck(inv *inventory.Inventory, sw inventory.FabricSwitch, binding inventory.SwitchPort) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.cabling", Subject: sw.Name + "/" + binding.Name, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "DPU_MAC_OWNERSHIP_UNAVAILABLE", Message: "Read the selected DPU card's network-function MAC addresses through its BMC to corroborate the switch LLDP peer."}
	var selected *inventory.Device
	for i := range inv.DPUs {
		dpu := &inv.DPUs[i]
		if dpu.SerialNumber != binding.DPUSerial {
			continue
		}
		if selected != nil {
			check.State, check.ReasonCode, check.Message = "failed", "DPU_MAC_IDENTITY_AMBIGUOUS", "The selected DPU serial occurs more than once; resolve physical identity before validating cabling."
			return check
		}
		selected = dpu
	}
	if selected == nil || selected.ID == "" || selected.ObservationState != "observed" {
		return check
	}
	if len(selected.NetworkFunctionMACs) == 0 {
		if observation, ok := selected.Observations["networkFunctionMACs"]; ok && observation.ReasonCode != "" {
			check.ReasonCode = observation.ReasonCode
			check.Message = "DPU network-function MAC ownership is unavailable: " + observation.Message
		}
		return check
	}
	for _, issue := range sw.Snapshot.Issues {
		if issue.Input == "lldp" {
			check.ReasonCode, check.Message = issue.Code, "Switch LLDP inspection is incomplete: "+issue.Message
			return check
		}
	}
	peers := []fabric.LLDPPeerHint{}
	for _, peer := range sw.Snapshot.Operational.LLDP {
		if peer.LocalInterface == binding.Name {
			peers = append(peers, peer)
		}
	}
	if len(peers) != 1 {
		check.ReasonCode, check.Message = "LLDP_PEER_NOT_UNIQUE", "The selected switch port must report exactly one LLDP peer; no unique peer was observed."
		if len(peers) > 1 {
			check.State = "failed"
		}
		return check
	}
	peer := peers[0]
	mac := lldpMAC(peer.PortID)
	if mac == "" {
		check.ReasonCode, check.Message = "LLDP_PORT_MAC_UNAVAILABLE", "The selected switch port's LLDP peer does not expose a valid six-byte unicast port MAC."
		return check
	}
	if !slices.Contains(selected.NetworkFunctionMACs, mac) {
		check.State, check.ReasonCode, check.Message = "failed", "LLDP_DPU_MAC_MISMATCH", "The switch LLDP port MAC is not owned by the selected DPU card; verify the cable and configured DPU binding."
		return check
	}
	for _, dpu := range inv.DPUs {
		if dpu.ID != selected.ID && slices.Contains(dpu.NetworkFunctionMACs, mac) {
			check.State, check.ReasonCode, check.Message = "failed", "DPU_MAC_OWNERSHIP_CONFLICT", "The LLDP port MAC is attributed to more than one selected physical DPU."
			return check
		}
	}
	matches := 0
	for _, candidateSwitch := range inv.HBN.Switches {
		for _, candidate := range candidateSwitch.Snapshot.Operational.LLDP {
			if lldpMAC(candidate.PortID) == mac {
				matches++
			}
		}
	}
	if matches != 1 {
		check.State, check.ReasonCode, check.Message = "failed", "LLDP_PORT_MAC_AMBIGUOUS", "The same DPU port MAC appears on multiple inspected switch ports; resolve ambiguous cabling."
		return check
	}
	label := ""
	for _, observed := range []*string{peer.PortName, peer.PortDescription} {
		if observed == nil || strings.TrimSpace(*observed) == "" {
			continue
		}
		value := strings.TrimSpace(*observed)
		if value != binding.DPUInterface {
			check.State, check.ReasonCode, check.Message = "failed", "LLDP_DPU_PORT_MISMATCH", "The LLDP port name or description disagrees with the requested DPU uplink; verify the p0/p1 binding."
			return check
		}
		label = value
	}
	if label == "" {
		check.ReasonCode, check.Message = "LLDP_DPU_PORT_UNAVAILABLE", "The peer MAC belongs to the selected DPU, but LLDP does not identify its p0/p1 uplink."
		return check
	}
	check.State, check.ReasonCode, check.Message = "passed", "LLDP_CABLING_CORROBORATED", "The unique LLDP port MAC belongs to the selected BMC-identified DPU and its advertised uplink matches the binding; packet delivery remains a post-deployment check."
	return check
}

func lldpMAC(raw *string) string {
	if raw == nil {
		return ""
	}
	mac, err := net.ParseMAC(*raw)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return ""
	}
	return mac.String()
}
