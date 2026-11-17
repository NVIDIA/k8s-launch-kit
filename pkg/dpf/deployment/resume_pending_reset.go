// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// assessPendingResetHosts admits only the unavailable host observations caused
// by the exact owned pending request. It is never used after activation.
func assessPendingResetHosts(ctx context.Context, client activationObservationReader, prepared *preparedControlPlaneRun, proof ownedResumePrerequisites, original, current *inventory.Inventory, checks []inventory.Check, phase PrerequisitePhase) ([]inventory.Check, error) {
	if phase == AfterActivation {
		return checks, nil
	}
	if phase != BeforeActivation {
		return nil, fmt.Errorf("unsupported pending-reset prerequisite phase")
	}
	indexes := []int{}
	for i, item := range proof.selected {
		if item.host.Runtime == nil {
			continue
		}
		for _, pf := range item.adapter.PCIFunctions {
			if pf.NetdevsAbsent {
				indexes = append(indexes, i)
				break
			}
		}
	}
	if len(indexes) == 0 {
		return checks, nil
	}
	if original == nil || current == nil || current.HBN == nil || prepared.operation == nil || proof.parentUID == "" {
		return nil, fmt.Errorf("owned pending reset requires original physical scope and established parent")
	}
	deployment, err := retainedDeploymentObject(prepared.execution.verificationInputs())
	if err != nil {
		return nil, err
	}
	observed, err := observeActivationScope(ctx, client, prepared.execution.verificationInputs(), deployment, proof.parentUID, proof.selected, prepared.operation.restoreProgress())
	if err != nil || !observed.complete {
		return nil, fmt.Errorf("owned pending reset scope cannot be observed")
	}
	for _, index := range indexes {
		item := proof.selected[index]
		pending, actionable, err := selectedHostRebootPending(observed, proof.selected, []int{index})
		if err != nil || !pending || !actionable || !activationResetParticipantsPresent(observed, proof.selected, []int{index}) {
			return nil, fmt.Errorf("owned pending reset request or participants changed")
		}
		round, err := readActivationRebootRound(observed.dpus[item.dpuName])
		if err != nil {
			return nil, err
		}
		for _, reset := range prepared.operation.state.Resets {
			if previous, found := reset.Rounds[round.uid]; found && previous.Source == round.source && !liveRound(previous).sameRequest(round) {
				return nil, fmt.Errorf("recorded pending reset round changed")
			}
		}
		prior, known := prepared.operation.state.Progress[item.nodeName]
		maintenance := observed.maintenance[item.nodeName+"-hold"]
		if !known || prior.ReleasedVersion == "" || maintenance == nil || prior.MaintenanceUID != maintenance.GetUID() {
			return nil, fmt.Errorf("owned pending reset has no recorded maintenance release history")
		}
		if _, configured := prepared.operation.state.Hosts[item.item.device.Serial]; configured {
			return nil, fmt.Errorf("configured host cannot defer its current physical/backend proof")
		}
		if err := pendingHostPhysicalBinding(item, original); err != nil {
			return nil, err
		}
		hostChecks, err := discovery.ResumePendingResetHostChecks(current, item.item.device.Serial)
		if err != nil {
			return nil, err
		}
		checks = slices.Clone(checks)
		for _, replacement := range hostChecks {
			for i, raw := range checks {
				if raw.ID != replacement.ID || raw.Subject != replacement.Subject || raw.Vantage != "executor" || raw.RequiredPhase != "deploy" || raw.State != "unknown" {
					continue
				}
				allowed := raw.ID == "dpf.hbn.host-pf" && raw.ReasonCode == "HOST_PF_NETDEV_ABSENT" || raw.ID == "dpf.hbn.host-management-separation" && raw.ReasonCode == "HOST_PF_NETDEV_UNAVAILABLE" || raw.ID == "dpf.hbn.host-network-backend" && raw.ReasonCode == "HOST_NETWORK_IDENTITY_UNAVAILABLE"
				if allowed {
					checks[i] = replacement
				}
			}
		}
		for _, sw := range current.HBN.Switches {
			for _, port := range sw.Ports {
				if port.DPUSerial != item.item.device.Serial {
					continue
				}
				for i, raw := range checks {
					if raw.ID == "dpf.hbn.cabling" && raw.Subject == sw.Name+"/"+port.Name && raw.Vantage == "executor" && raw.RequiredPhase == "deploy" && raw.State == "unknown" && raw.ReasonCode == "HOST_LLDP_PORT_MAPPING_UNAVAILABLE" {
						checks[i] = ownedResumeCheck(raw, "OWNED_PENDING_RESET_CABLING_DEFERRED", "Only this owned pending-reset DPU's current host-port capture is unavailable; static switch policy remains required and strict fresh cabling must pass after activation.")
					}
				}
			}
		}
	}
	return checks, nil
}

func pendingHostPhysicalBinding(item activationSelectedDevice, original *inventory.Inventory) error {
	prior := uniqueInventoryHost(original, item.host.ID)
	if prior == nil || prior.SystemUUID != item.host.SystemUUID || prior.InputRef != item.host.InputRef || prior.SSHEndpoint != item.host.SSHEndpoint {
		return fmt.Errorf("pending reset physical host differs from original selection")
	}
	var adapter *inventory.Adapter
	for i := range prior.Adapters {
		if prior.Adapters[i].Key == item.adapter.Key {
			if adapter != nil {
				return fmt.Errorf("original pending adapter is ambiguous")
			}
			adapter = &prior.Adapters[i]
		}
	}
	if adapter == nil || !samePhysicalAdapter(*adapter, item.adapter) {
		return fmt.Errorf("pending reset PCI/VPD binding changed")
	}
	oldNames := map[string]bool{}
	for _, pf := range adapter.PCIFunctions {
		for _, name := range pf.Netdevs {
			oldNames[name] = true
		}
	}
	if item.host.Runtime == nil || item.host.Runtime.Network == nil {
		return fmt.Errorf("pending reset current host observations unavailable")
	}
	if !validUUID(item.host.Runtime.BootID) || item.host.Runtime.Observations["host-boot-id"].State != "" {
		return fmt.Errorf("pending reset current host boot is unverified")
	}
	for _, link := range item.host.Runtime.Network.Links {
		if oldNames[link.Name] {
			return fmt.Errorf("original selected PF name remains on an inconsistent current path")
		}
	}
	return nil
}
