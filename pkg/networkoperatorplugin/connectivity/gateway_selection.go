// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"encoding/json"
	"fmt"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const networkSelectionAnnotation = "k8s.v1.cni.cncf.io/networks"

type gatewayNetworkSelection struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Interface string          `json:"interface"`
	CNIArgs   json.RawMessage `json:"cni-args"`
}

// validateGatewayNetworkSelection checks the pod-side overrides that are absent
// from Multus network-status and the NAD. Do not include annotation values in
// errors: CNI args can contain credentials. This guard applies only to NV-IPAM;
// the pair matrix and other IPAM providers retain their existing behavior.
func validateGatewayNetworkSelection(raw, podNamespace, namespace, network, iface string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var selections []gatewayNetworkSelection
	if strings.ContainsAny(raw, `[{"`) {
		if err := json.Unmarshal([]byte(raw), &selections); err != nil {
			return fmt.Errorf("cannot verify NV-IPAM gateway: malformed pod network-selection JSON")
		}
	} else {
		for _, item := range strings.Split(raw, ",") {
			name, requestedIface, _ := strings.Cut(strings.TrimSpace(item), "@")
			selections = append(selections, gatewayNetworkSelection{Name: name, Interface: requestedIface})
		}
	}
	matches := 0
	for _, selection := range selections {
		selectedNamespace := selection.Namespace
		selectedName := selection.Name
		if prefix, name, qualified := strings.Cut(selectedName, "/"); qualified {
			if selectedNamespace != "" && selectedNamespace != prefix {
				return fmt.Errorf("cannot verify NV-IPAM gateway: conflicting pod network-selection namespaces")
			}
			selectedNamespace, selectedName = prefix, name
		}
		if selectedNamespace == "" {
			selectedNamespace = podNamespace
		}
		if len(k8svalidation.IsDNS1123Subdomain(selectedName)) > 0 ||
			len(k8svalidation.IsDNS1123Label(selectedNamespace)) > 0 ||
			(selection.Interface != "" && !gatewayInterfaceName.MatchString(selection.Interface)) {
			return fmt.Errorf("cannot verify NV-IPAM gateway: malformed pod network-selection attachment identity")
		}
		if selectedNamespace != namespace || selectedName != network ||
			(selection.Interface != "" && selection.Interface != iface) {
			continue
		}
		matches++
		if matches > 1 {
			return fmt.Errorf("cannot verify NV-IPAM gateway: ambiguous pod network-selection for NAD %s/%s interface %q", namespace, network, iface)
		}
		if selection.CNIArgs == nil {
			continue
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal(selection.CNIArgs, &args); err != nil {
			return fmt.Errorf("cannot verify NV-IPAM gateway: malformed cni-args for NAD %s/%s interface %q", namespace, network, iface)
		}
		if len(args) > 0 {
			return fmt.Errorf("cannot verify NV-IPAM gateway: pod cni-args overrides are unsupported for NAD %s/%s interface %q", namespace, network, iface)
		}
	}
	return nil
}
