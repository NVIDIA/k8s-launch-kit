// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func pendingResetInventory(t *testing.T) inventory.Inventory {
	t.Helper()
	inv, _ := hostBackendDiscoveryFixture(t, 9000)
	host := &inv.Hosts[0]
	host.Runtime.Interfaces = []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{"192.0.2.20/24"}}}
	host.Runtime.Network.Links = []inventory.NetworkLink{{Name: "mgmt0", PCIAddress: "0000:35:00.0", PhysicalPFRoots: []string{"0000:35:00.0"}}}
	host.Runtime.Routes = []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 2}, {Destination: "::/0", InterfaceIndex: 2}}
	for _, key := range []string{"interfaces", "network-links", "network-link-details", "routes"} {
		host.Runtime.Observations[key] = inventory.Observation{State: "observed"}
	}
	for i := range host.Adapters[0].PCIFunctions {
		pf := &host.Adapters[0].PCIFunctions[i]
		pf.Netdevs = []string{}
		pf.NetdevsAbsent = true
		host.Runtime.Observations["host-pf:"+pf.Address+":driver"] = inventory.Observation{State: "observed"}
		host.Runtime.Observations["host-pf:"+pf.Address+":netdevs"] = inventory.Observation{State: "observed", ReasonCode: "HOST_PF_NETDEV_ABSENT"}
	}
	return inv
}

func TestPendingResetHostChecksNeedMeasuredAbsenceAndCompleteSeparatePaths(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*inventory.Inventory)
		pass bool
	}{
		{"measured transition", func(*inventory.Inventory) {}, true},
		{"generic SSH failure", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Observations["host-pf:0000:21:00.0:netdevs"] = inventory.Observation{State: "unavailable", ReasonCode: "SSH_COMMAND_FAILED"}
		}, false},
		{"wrong driver", func(i *inventory.Inventory) { i.Hosts[0].Adapters[0].PCIFunctions[0].Driver = "foreign" }, false},
		{"one PF still present", func(i *inventory.Inventory) { i.Hosts[0].Adapters[0].PCIFunctions[0].Netdevs = []string{"pf0"} }, false},
		{"missing PF", func(i *inventory.Inventory) {
			i.Hosts[0].Adapters[0].PCIFunctions = i.Hosts[0].Adapters[0].PCIFunctions[:1]
		}, false},
		{"selected PCI on current path", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Network.Links[0].PCIAddress = "0000:21:00.0" }, false},
		{"same-card VF root despite different visible BDF", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.Links[0].PCIAddress = "0000:43:00.0"
			i.Hosts[0].Runtime.Network.Links[0].PhysicalPFRoots = []string{"0000:21:00.0"}
			if c := HostManagementPhysicalCheck(i, i.DPUs[0].SerialNumber); c.State != "failed" {
				t.Fatalf("initial check did not reject same-card VF: %+v", c)
			}
		}, false},
		{"unrelated virtual interface", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Interfaces = append(i.Hosts[0].Runtime.Interfaces, inventory.Interface{Name: "docker0", Index: 3, Addresses: []string{"172.17.0.1/16"}})
			i.Hosts[0].Runtime.Network.Links = append(i.Hosts[0].Runtime.Network.Links, inventory.NetworkLink{Name: "docker0", Kind: "bridge", Master: "unrelated0"})
		}, true},
		{"unmeasured roots", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Network.Links[0].PhysicalPFRoots = nil }, false},
		{"unknown route source", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Observations["routes"] = inventory.Observation{State: "unavailable"}
		}, false},
		{"unresolved IPv6 default", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Routes[1].InterfaceIndex = 99 }, false},
		{"unresolved IPv4 default", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Routes[0].InterfaceIndex = 99 }, false},
		{"unknown master", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Network.Links[0].Master = "missing" }, false},
		{"master cycle", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Network.Links[0].Master = "mgmt0" }, false},
		{"measured VLAN lower path", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.Links[0].PCIAddress = ""
			i.Hosts[0].Runtime.Network.Links[0].Kind = "vlan"
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := pendingResetInventory(t)
			test.edit(&inv)
			before := rawHostHBNTestInventory(t, inv)
			checks, err := ResumePendingResetHostChecks(&inv, inv.DPUs[0].SerialNumber)
			if (err == nil) != test.pass {
				t.Fatalf("checks=%+v err=%v", checks, err)
			}
			if !bytes.Equal(before, rawHostHBNTestInventory(t, inv)) {
				t.Fatal("current observations mutated")
			}
			if test.pass {
				for _, c := range checks {
					if c.State != "passed" || !strings.HasPrefix(c.ReasonCode, "OWNED_PENDING_RESET_") {
						t.Fatalf("bad activation-only evidence: %+v", c)
					}
				}
				if hostHBNCheck(t, hostHBNChecks(&inv, nil), "host-management-separation", "SERIAL-1 @ worker-01").State == "passed" {
					t.Fatal("ordinary initial policy was weakened")
				}
			}
		})
	}
}

func TestPendingResetCollectionPresentationDoesNotChangeUnknownFacts(t *testing.T) {
	inv := pendingResetInventory(t)
	_, request := hostBackendDiscoveryFixture(t, 9000)
	var progress strings.Builder
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
	checks := collectHostHBNNetworking(ctx, request, &inv, func(context.Context, config.Target, sshprobe.HostNetworkIdentity, time.Duration) (sshprobe.HostNetworkObservation, error) {
		t.Fatal("backend transport must not be called without p0")
		return sshprobe.HostNetworkObservation{}, nil
	}, true)
	if checks[inv.DPUs[0].SerialNumber].ReasonCode != "HOST_NETWORK_IDENTITY_UNAVAILABLE" || strings.Contains(progress.String(), "[BLOCKED]") || !strings.Contains(progress.String(), "[UNAVAILABLE]") {
		t.Fatalf("checks=%+v progress=%s", checks, progress.String())
	}
}
