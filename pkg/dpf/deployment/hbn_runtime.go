// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

const hbnRuntimeStage = "hbn-runtime-validation"

// HBNRuntimeResult retains individual measurements on every later failure.
// Fenced measurements are point-in-time observations, never a cohort pass.
type HBNRuntimeResult struct {
	State            string                 `json:"state"`
	ReasonCode       string                 `json:"reasonCode,omitempty"`
	TrafficValidated bool                   `json:"trafficValidated"`
	BGP              []HBNBGPResult         `json:"bgp,omitempty"`
	Routes           []HBNRouteResult       `json:"routes,omitempty"`
	Traffic          []HBNHostTrafficResult `json:"traffic,omitempty"`
}
type HBNBGPResult struct {
	DPUSerial    string `json:"dpuSerial"`
	HostInputRef string `json:"hostInputRef"`
	LocalASN     uint32 `json:"localASN"`
	FabricASN    uint32 `json:"fabricASN"`
	ObservedASN  uint32 `json:"observedASN,omitempty"`
	State        string `json:"state"`
	ReasonCode   string `json:"reasonCode,omitempty"`
	Fenced       bool   `json:"fenced"`
}
type HBNRouteResult struct {
	DPUSerial       string `json:"dpuSerial"`
	RemoteDPUSerial string `json:"remoteDpuSerial"`
	Prefix          string `json:"prefix"`
	State           string `json:"state"`
	ReasonCode      string `json:"reasonCode,omitempty"`
	Fenced          bool   `json:"fenced"`
}
type HBNHostTrafficResult struct {
	SourceHost           string                     `json:"sourceHost"`
	DestinationHost      string                     `json:"destinationHost"`
	SourceDPUSerial      string                     `json:"sourceDpuSerial"`
	DestinationDPUSerial string                     `json:"destinationDpuSerial"`
	Measurement          sshprobe.HostTrafficResult `json:"measurement"`
	Fenced               bool                       `json:"fenced"`
}

type hbnRuntimeRead func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error)
type hbnHostTrafficProbe func(context.Context, config.Target, sshprobe.HostTrafficRequest, time.Duration) (sshprobe.HostTrafficResult, error)

func validateHBNRuntimeHostReceipts(activation activationStageReceipt, selected []activationSelectedDevice, fence hostNetworkLeaseFence, hosts []HostWorkloadNetworkResult, mtu int, cidr string) error {
	if len(selected) != len(hosts) || len(fence.leases) != len(selected) {
		return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
	}
	seen := map[string]bool{}
	for _, device := range selected {
		serial := device.item.device.Serial
		var receipt *HostWorkloadNetworkResult
		for i := range hosts {
			if hosts[i].DPUSerial == serial {
				if receipt != nil {
					return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
				}
				receipt = &hosts[i]
			}
		}
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(device.host, device.adapter.Key, device.adapter.SerialNumber)
		if err != nil {
			return err
		}
		request, err := hostNetworkRequestForLease(activation, identity, fence, serial, mtu, cidr)
		if err != nil {
			return err
		}
		if seen[device.host.ID] || receipt == nil || !receipt.LeaseReverified || receipt.HostInputRef != device.host.InputRef || receipt.Interface != request.Identity.InterfaceName || receipt.HostAddressPrefix != request.HostAddressPrefix || receipt.Gateway != request.Gateway || receipt.MTU != mtu ||
			receipt.Configuration.RequestID != hbnBindingDigest(request) || receipt.Configuration.Blocking || !receipt.Configuration.RuntimeVerified || !receipt.Configuration.FileVerified || receipt.Configuration.State != "complete" && receipt.Configuration.State != "verified-external" {
			return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
		}
		seen[device.host.ID] = true
	}
	return nil
}
