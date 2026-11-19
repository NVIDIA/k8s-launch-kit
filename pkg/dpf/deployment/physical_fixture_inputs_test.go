// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// physicalFixtureInputs owns only fresh request and inventory inputs. Creating
// these inputs performs no preparation, enrollment, workload or observation.
type physicalFixtureInputs struct {
	request   config.Request
	inventory *inventory.Inventory
}

func newPhysicalFixtureInputs(t *testing.T, devices []generation.ResolvedDevice, sameHost bool) physicalFixtureInputs {
	t.Helper()
	if len(devices) == 0 {
		t.Fatal("physical fixture requires a selected cohort")
	}
	serials, bmcs := map[string]bool{}, map[string]bool{}
	for _, device := range devices {
		if device.Serial == "" || device.BMCIP == "" || serials[device.Serial] || bmcs[device.BMCIP] {
			t.Fatal("physical fixture requires distinct, nonempty serials and BMC IPs")
		}
		serials[device.Serial], bmcs[device.BMCIP] = true, true
	}
	request := controlPlaneTestRequest(t)
	firstDPUTarget := -1
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://" + devices[0].BMCIP
			firstDPUTarget = i
		}
	}
	if firstDPUTarget < 0 {
		t.Fatal("physical fixture is missing its initial DPU access target")
	}
	for i := 1; i < len(devices); i++ {
		target := request.Targets[firstDPUTarget]
		target.InputRef = fmt.Sprintf("dpu-input-%d", i+1)
		target.Endpoint = "https://" + devices[i].BMCIP
		request.Targets = append(request.Targets, target)
	}
	hosts := make([]inventory.Device, 0, len(devices))
	dpus := make([]inventory.Device, 0, len(devices))
	associations := make([]inventory.Association, 0, len(devices))
	for i, item := range devices {
		suffix := string(rune('a' + i))
		systemUUID := "12345678-1234-1234-1234-123456789abc"
		if i > 0 {
			systemUUID = fmt.Sprintf("12345678-1234-1234-1234-123456789ab%d", i+1)
		}
		host := inventory.Device{ID: "host-" + suffix, InputRef: "host-input-" + suffix,
			SSHEndpoint: "ssh://host-" + suffix + ".example.test:22", SystemUUID: systemUUID,
			Adapters: []inventory.Adapter{{Key: "adapter-" + suffix, SerialNumber: item.Serial, IdentityState: "known"}}}
		if sameHost && i > 0 {
			hosts[0].Adapters = append(hosts[0].Adapters, host.Adapters[0])
			host = hosts[0]
		} else {
			hosts = append(hosts, host)
			request.Targets = append(request.Targets, config.Target{Role: "hosts", InputRef: host.InputRef, Endpoint: host.SSHEndpoint, SSH: &config.SSHAccess{}})
		}
		dpu := inventory.Device{ID: "dpu-" + suffix, SerialNumber: item.Serial}
		dpus = append(dpus, dpu)
		associations = append(associations, inventory.Association{DPUID: dpu.ID, HostID: host.ID, HostAdapterKey: "adapter-" + suffix, SerialNumber: dpu.SerialNumber})
	}
	inv := &inventory.Inventory{FormatVersion: inventory.FormatVersion, Hosts: hosts, DPUs: dpus, Associations: associations}

	return physicalFixtureInputs{request: request, inventory: inv}
}

func TestPhysicalFixtureInputsPreserveCohortIdentitiesAndTargets(t *testing.T) {
	devices := []generation.ResolvedDevice{
		{Serial: "SERIAL-B", BMCIP: "192.0.2.71", BMCPort: 443},
		{Serial: "SERIAL-A", BMCIP: "192.0.2.70", BMCPort: 443},
	}
	for _, tc := range []struct {
		name      string
		count     int
		sameHost  bool
		wantHosts int
	}{
		{name: "one device", count: 1, sameHost: false, wantHosts: 1},
		{name: "two hosts", count: 2, sameHost: false, wantHosts: 2},
		{name: "two cards on one host", count: 2, sameHost: true, wantHosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := newPhysicalFixtureInputs(t, devices[:tc.count], tc.sameHost)
			inv := inputs.inventory
			if inv.FormatVersion != inventory.FormatVersion || len(inv.DPUs) != tc.count || len(inv.Associations) != tc.count || len(inv.Hosts) != tc.wantHosts {
				t.Fatalf("unexpected physical cohort: %+v", inv)
			}
			dpuTargets := []config.Target{}
			for _, target := range inputs.request.Targets {
				if target.Role == "dpus" {
					dpuTargets = append(dpuTargets, target)
				}
			}
			if len(dpuTargets) != tc.count {
				t.Fatalf("got %d DPU targets, want %d", len(dpuTargets), tc.count)
			}
			for i, dpu := range inv.DPUs {
				association := inv.Associations[i]
				if dpu.SerialNumber != devices[i].Serial || association.DPUID != dpu.ID || association.SerialNumber != dpu.SerialNumber || dpuTargets[i].Endpoint != "https://"+devices[i].BMCIP || dpuTargets[i].InputRef == "" {
					t.Fatalf("device %d lost ordered identity or target correlation: %+v %+v %+v", i, dpu, association, dpuTargets[i])
				}
				if i > 0 && dpuTargets[i].InputRef == dpuTargets[0].InputRef {
					t.Fatal("DPU targets share an input reference")
				}
				hostIndex := i
				if tc.sameHost {
					hostIndex = 0
				}
				host := inv.Hosts[hostIndex]
				if association.HostID != host.ID {
					t.Fatalf("device %s associated with wrong host: %+v", dpu.SerialNumber, association)
				}
				foundAdapter, foundTarget := false, false
				for _, adapter := range host.Adapters {
					if adapter.Key == association.HostAdapterKey && adapter.SerialNumber == dpu.SerialNumber && adapter.IdentityState == "known" {
						foundAdapter = true
					}
				}
				for _, target := range inputs.request.Targets {
					if target.Role == "hosts" && target.InputRef == host.InputRef && target.Endpoint == host.SSHEndpoint && target.SSH != nil {
						foundTarget = true
					}
				}
				if !foundAdapter || !foundTarget {
					t.Fatalf("device %s lacks associated adapter or SSH target: %+v", dpu.SerialNumber, host)
				}
			}
			if tc.sameHost && len(inv.Hosts[0].Adapters) != tc.count {
				t.Fatal("same-host fixture lost a selected card")
			}
		})
	}
}

func TestPhysicalFixtureInputsKeepMutableStateFresh(t *testing.T) {
	devices := []generation.ResolvedDevice{{Serial: "SERIAL-A", BMCIP: "192.0.2.30", BMCPort: 443}, {Serial: "SERIAL-B", BMCIP: "192.0.2.31", BMCPort: 443}}
	for _, sameHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameHost=%t", sameHost), func(t *testing.T) {
			first := newPhysicalFixtureInputs(t, devices, sameHost)
			second := newPhysicalFixtureInputs(t, devices, sameHost)
			const changed = "first-only"
			first.request.Targets[0].Endpoint = changed
			*first.request.Input.Site.Credentials["bmc"].PasswordRef.File = changed
			first.inventory.DPUs[0].SerialNumber = changed
			first.inventory.Associations[0].HostAdapterKey = changed
			first.inventory.Hosts[0].SystemUUID = changed
			first.inventory.Hosts[0].Adapters[0].SerialNumber = changed
			for i := range first.request.Targets {
				if first.request.Targets[i].SSH != nil {
					first.request.Targets[i].SSH.Sudo = true
				}
			}
			if second.inventory == first.inventory || second.request.Targets[0].Endpoint == changed || *second.request.Input.Site.Credentials["bmc"].PasswordRef.File == changed || second.inventory.DPUs[0].SerialNumber != devices[0].Serial || second.inventory.Associations[0].HostAdapterKey == changed || second.inventory.Hosts[0].SystemUUID == changed || second.inventory.Hosts[0].Adapters[0].SerialNumber != devices[0].Serial {
				t.Fatal("fixture inputs share mutable request or inventory state across calls")
			}
			for _, target := range second.request.Targets {
				if target.SSH != nil && target.SSH.Sudo {
					t.Fatal("fixtures share mutable SSH access")
				}
			}
		})
	}
}
