// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"slices"
	"strings"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// groupPFMembers records the actual collapse projection, keyed by worker set so
// later group filtering, identifiers and ordering cannot change its association.
// Neither projection nor node-local aliases are persisted into ClusterConfig.
type groupPFMembers map[string]map[string][]string

func groupWorkerKey(workers []string) string {
	nodes := slices.Clone(workers)
	slices.Sort(nodes)
	return strings.Join(nodes, "\x00")
}

type nodePFIdentity struct {
	groupIndex       int
	node             string
	retainedPCI      string
	pci              string
	rdmaDevice       string
	networkInterface string
	missing          string
}

// buildNodePFIdentities establishes expected membership before observing probe
// successes. A missing/ambiguous raw record remains an explicit expected entry.
func buildNodePFIdentities(groups []config.ClusterConfig, devices []nicop.NicDevice, members groupPFMembers) []nodePFIdentity {
	type alias struct{ rdma, net string }
	inventory := map[string]map[string][]alias{}
	for _, d := range devices {
		if inventory[d.Status.Node] == nil {
			inventory[d.Status.Node] = map[string][]alias{}
		}
		for _, p := range d.Status.Ports {
			pci := normalizePCI(p.PCI)
			inventory[d.Status.Node][pci] = append(inventory[d.Status.Node][pci], alias{p.RdmaInterface, p.NetworkInterface})
		}
	}
	var identities []nodePFIdentity
	for i, group := range groups {
		projection := members[groupWorkerKey(group.WorkerNodes)]
		for _, pf := range group.PFs {
			if pf.Traffic != "east-west" {
				continue
			}
			retained := normalizePCI(pf.PciAddress)
			raw := projection[retained]
			reason := ""
			if len(raw) == 0 {
				raw = []string{retained}
				reason = "missing collapse projection"
			}
			for _, node := range group.WorkerNodes {
				for _, pci := range raw {
					identity := nodePFIdentity{groupIndex: i, node: node, retainedPCI: retained, pci: pci, missing: reason}
					aliases := inventory[node][pci]
					switch len(aliases) {
					case 0:
						identity.missing = "missing NicDevice PCI membership"
					case 1:
						identity.rdmaDevice, identity.networkInterface = aliases[0].rdma, aliases[0].net
					default:
						identity.missing = "ambiguous NicDevice PCI membership"
					}
					identities = append(identities, identity)
				}
			}
		}
	}
	return identities
}
