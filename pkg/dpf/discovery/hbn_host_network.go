// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// HostNetworkObserver inspects one already-selected host PF and its persistent
// backend without allocating an address, generating files or configuring links.
type HostNetworkObserver func(context.Context, config.Target, sshprobe.HostNetworkIdentity, time.Duration) (sshprobe.HostNetworkObservation, error)

func collectHostHBNNetworking(ctx context.Context, request config.Request, inv *inventory.Inventory, observer HostNetworkObserver, deferEligibility bool) map[string]inventory.Check {
	if ctx == nil || request.HBN == nil || inv == nil || observer == nil {
		return nil
	}
	type selectedHost struct {
		serial  string
		host    *inventory.Device
		adapter *inventory.Adapter
	}
	selected := make([]selectedHost, 0, len(inv.DPUs))
	hostCounts := make(map[string]int)
	for _, dpu := range inv.DPUs {
		_, host, adapter, state, _, _ := resolveHostHBNAssociation(inv, dpu)
		if state == "passed" && host != nil && adapter != nil {
			selected = append(selected, selectedHost{serial: dpu.SerialNumber, host: host, adapter: adapter})
			hostCounts[host.ID]++
		}
	}
	checks := make(map[string]inventory.Check, len(selected))
	for _, item := range selected {
		subject := item.serial + " @ " + hostDisplayName(*item.host)
		recordResult := func(state, code, message string) {
			checks[item.serial] = hostCheck("host-network-backend", subject, state, code, message)
			status := "BLOCKED"
			if state == "passed" {
				status = "PASS"
			}
			if deferEligibility && state != "passed" {
				status = "UNAVAILABLE"
			}
			ui.FromContext(ctx).Info("[%s] Host %q: %s", status, hostDisplayName(*item.host), message)
		}
		if hostCounts[item.host.ID] != 1 {
			recordResult("failed", "HOST_NETWORK_MULTIPLE_DPUS_UNSUPPORTED", "The flat workload route supports one selected DPU per physical host; several paths require a separately configured multipath policy.")
			continue
		}
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(*item.host, item.adapter.Key, item.adapter.SerialNumber)
		if err != nil {
			recordResult("unknown", "HOST_NETWORK_IDENTITY_UNAVAILABLE", "The host workload p0 identity cannot be established: "+err.Error()+".")
			continue
		}
		target, state := selectedHostSSH(request.Targets, *item.host)
		if state != "passed" {
			recordResult("unknown", "HOST_NETWORK_ACCESS_UNAVAILABLE", "The exact selected host has no unique explicit SSH access and trust binding for backend inspection.")
			continue
		}
		ui.FromContext(ctx).Info("[RUNNING] Host %q: inspect workload backend and MTU support on %s", hostDisplayName(*item.host), identity.InterfaceName)
		observation, err := observer(ctx, target, identity, request.RequestTimeout)
		if err != nil {
			code := sshprobe.Code(err)
			state := "unknown"
			switch code {
			case "HOST_NETWORK_BACKEND_UNSUPPORTED", "HOST_NETWORK_NETPLAN_UNSUPPORTED", "HOST_NETWORK_LINK_UNSUPPORTED", "HOST_NETWORK_ROUTING_POLICY_UNSUPPORTED", "HOST_NETWORK_MANAGEMENT_PATH_CONFLICT", "HOST_NETWORK_IDENTITY_CHANGED", "HOST_NETWORK_MTU_UNSUPPORTED":
				state = "failed"
			}
			recordResult(state, code, "Host workload backend inspection did not complete: "+sshprobe.Reason(err)+" ("+code+").")
			continue
		}
		if observation.Identity != identity {
			recordResult("failed", "HOST_NETWORK_IDENTITY_CHANGED", "The workload backend response identifies a different host, card, PCI function, physical port or permanent MAC.")
			continue
		}
		if observation.BackendVersion == "" || observation.ManagementInterface == "" || observation.ManagementInterface == identity.InterfaceName ||
			observation.MinMTU < 1 || observation.MaxMTU < observation.MinMTU || observation.MTU < observation.MinMTU || observation.MTU > observation.MaxMTU ||
			observation.MinimumWorkloadMTU < 1 || observation.MinimumWorkloadMTU > observation.MaxMTU {
			recordResult("unknown", "HOST_NETWORK_BACKEND_FACTS_INCOMPLETE", "Complete supported-backend, separate management-path, kernel MTU-bound and backend minimum-MTU facts are required.")
			continue
		}
		mtu := request.HBN.WorkloadMTU
		if mtu < max(observation.MinMTU, observation.MinimumWorkloadMTU) || mtu > observation.MaxMTU || observation.MTU < observation.MinimumWorkloadMTU {
			recordResult("failed", "HOST_NETWORK_MTU_UNSUPPORTED", fmt.Sprintf("Workload p0 %s: current MTU %d or desired MTU %d is unsupported (kernel range %d–%d; backend minimum %d).", identity.InterfaceName, observation.MTU, mtu, observation.MinMTU, observation.MaxMTU, observation.MinimumWorkloadMTU))
			continue
		}
		recordResult("passed", "HOST_NETWORK_BACKEND_SUPPORTED", fmt.Sprintf("Workload p0 %s uses supported Netplan %q; desired MTU %d is supported (current %d). Exact host address and route checks follow the real DPF allocation; no settings were changed.", identity.InterfaceName, observation.BackendVersion, mtu, observation.MTU))
	}
	return checks
}
