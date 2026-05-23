// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

// Package connectivity drives the Phase-2 data-plane verification flow:
// apply the example DaemonSet, wait for it to reach Ready, parse each
// pod's multus network-status annotation, and run a ping matrix between
// the secondary-network IPs.
package connectivity

import (
	"encoding/json"
	"fmt"
)

// MultusAnnotation is the metadata.annotations key the multus-cni
// controller writes onto every pod attached to a secondary network.
// The value is a JSON-encoded []NetworkStatus.
const MultusAnnotation = "k8s.v1.cni.cncf.io/network-status"

// NetworkStatus mirrors the upstream multus
// `k8s.v1.cni.cncf.io/network-status` annotation entry shape. We only
// model the fields we read; the upstream struct carries a few more
// (gateway, dns, mtu) that aren't relevant to the ping matrix.
type NetworkStatus struct {
	// Name is the NetworkAttachmentDefinition name in
	// `<namespace>/<name>` form (or just the name when in the pod's
	// own namespace).
	Name string `json:"name"`
	// Interface is the in-pod interface name (e.g. "net1", "rdma0").
	Interface string `json:"interface,omitempty"`
	// IPs lists every IPv4/IPv6 address assigned to Interface. Most
	// pod attachments have a single IP per family.
	IPs []string `json:"ips,omitempty"`
	// MAC, gateway, etc. — present in the upstream schema but
	// unused here. Decoded best-effort via json.RawMessage so a
	// future shape change doesn't break parsing.
	Mac string `json:"mac,omitempty"`
	// Default reports whether this is the pod's default network
	// (the cluster CNI). Multus marks the cluster CNI as
	// `default: true`; secondary networks omit the field.
	Default bool `json:"default,omitempty"`
}

// ParseNetworkStatus decodes the JSON payload from a pod's
// `k8s.v1.cni.cncf.io/network-status` annotation. Returns an empty
// slice (not an error) when the annotation is empty — pods without any
// secondary networks simply have nothing to ping.
//
// Malformed JSON returns a non-nil error so callers can decide whether
// to skip the pod or fail the whole matrix.
func ParseNetworkStatus(annotation string) ([]NetworkStatus, error) {
	if annotation == "" {
		return nil, nil
	}
	var out []NetworkStatus
	if err := json.Unmarshal([]byte(annotation), &out); err != nil {
		return nil, fmt.Errorf("decode multus network-status: %w", err)
	}
	return out, nil
}

// SecondaryNetworks filters out the cluster's default-CNI attachment
// (e.g. Calico/Flannel/Cilium), leaving only the secondary networks
// the deployment under test put on the pod. The ping matrix runs over
// these.
func SecondaryNetworks(all []NetworkStatus) []NetworkStatus {
	out := make([]NetworkStatus, 0, len(all))
	for _, n := range all {
		if n.Default {
			continue
		}
		out = append(out, n)
	}
	return out
}
