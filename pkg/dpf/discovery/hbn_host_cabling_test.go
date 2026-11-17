// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type hostLLDPCapturerFunc func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error)

func (f hostLLDPCapturerFunc) CaptureHostLLDP(ctx context.Context, target config.Target, request inventory.HostLLDPCaptureRequest, timeout time.Duration) (inventory.HostLLDPCapture, error) {
	return f(ctx, target, request, timeout)
}

func TestCollectHostHBNCablingUsesOnlyExactSelectedAssociation(t *testing.T) {
	inv, request := hostLLDPCablingFixture()
	calls := 0
	capturer := hostLLDPCapturerFunc(func(_ context.Context, target config.Target, captureRequest inventory.HostLLDPCaptureRequest, timeout time.Duration) (inventory.HostLLDPCapture, error) {
		calls++
		if target.InputRef != "/discovery/hosts/0" || target.Endpoint != "ssh://host.example:22" || timeout != 5*time.Second {
			t.Fatalf("capture used wrong target or timeout: %#v %s", target, timeout)
		}
		if captureRequest.AdapterKey != "selected-adapter" || captureRequest.AdapterSerial != "SERIAL-1" || captureRequest.AdapterPCIAddress != "0000:21:00.0" || len(captureRequest.Ports) != 2 {
			t.Fatalf("capture request was not selected to the associated card: %#v", captureRequest)
		}
		return hostLLDPCaptureResult(), nil
	})
	collectHostHBNCabling(context.Background(), request, &inv, capturer)
	if calls != 1 {
		t.Fatalf("got %d calls, want one capture", calls)
	}
	for _, binding := range inv.HBN.Switches[0].Ports {
		check := hostHBNCablingCheck(&inv, &inv.HBN.Switches[0], binding)
		if check.State != "passed" || check.ReasonCode != "HOST_LLDP_CABLING_CORROBORATED" {
			t.Fatalf("unexpected cabling result: %#v", check)
		}
	}
	if got := inv.Hosts[0].Runtime.Network.HostLLDP; got == nil || len(got.Ports) != 2 {
		t.Fatalf("selected host did not retain parsed LLDP facts: %#v", got)
	}
}

func TestCollectHostHBNCablingDoesNotFallbackToWrongTargetOrStaleFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*inventory.Inventory, *config.Request)
		code   string
	}{
		{name: "endpoint changed", mutate: func(_ *inventory.Inventory, r *config.Request) { r.Targets[0].Endpoint = "ssh://other.example:22" }, code: "HOST_LLDP_ACCESS_UNAVAILABLE"},
		{name: "source changed", mutate: func(_ *inventory.Inventory, r *config.Request) { r.Targets[0].InputRef = "/discovery/hosts/1" }, code: "HOST_LLDP_ACCESS_UNAVAILABLE"},
		{name: "association changed", mutate: func(i *inventory.Inventory, _ *config.Request) { i.Associations[0].HostAdapterKey = "other-adapter" }, code: "HOST_ASSOCIATION_CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, request := hostLLDPCablingFixture()
			tc.mutate(&inv, &request)
			stale := hostLLDPCaptureResult()
			inv.Hosts[0].Runtime.Network.HostLLDP = &stale
			calls := 0
			collectHostHBNCabling(context.Background(), request, &inv, hostLLDPCapturerFunc(func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error) {
				calls++
				return hostLLDPCaptureResult(), nil
			}))
			if calls != 0 {
				t.Fatalf("stale or unbound capture was reused; got %d calls", calls)
			}
		})
	}
}

func TestCollectHostHBNCablingSkipsAlreadyCorroboratedDPUPath(t *testing.T) {
	inv, request := hostLLDPCablingFixture()
	inv.DPUs[0].NetworkFunctionMACs = []string{"02:00:00:00:00:01"}
	inv.HBN.Switches[0].Snapshot.Operational.LLDP = []fabric.LLDPPeerHint{{LocalInterface: "swp19s0", PortID: stringPointer("02:00:00:00:00:01"), PortName: stringPointer("p0")}, {LocalInterface: "swp19s1", PortID: stringPointer("02:00:00:00:00:01"), PortName: stringPointer("p1")}}
	calls := 0
	collectHostHBNCabling(context.Background(), request, &inv, hostLLDPCapturerFunc(func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error) {
		calls++
		return hostLLDPCaptureResult(), nil
	}))
	if calls != 0 {
		t.Fatalf("host capture ran despite existing passing cabling checks: %d calls", calls)
	}
}

func TestCollectHostHBNCablingDoesNotCaptureOverKnownDPUIdentityMismatch(t *testing.T) {
	inv, request := hostLLDPCablingFixture()
	inv.DPUs[0].NetworkFunctionMACs = []string{"02:00:00:00:00:02"}
	inv.HBN.Switches[0].Snapshot.Operational.LLDP = []fabric.LLDPPeerHint{
		{LocalInterface: "swp19s0", PortID: stringPointer("02:00:00:00:00:01"), PortDescription: stringPointer("p0")},
		{LocalInterface: "swp19s1", PortID: stringPointer("02:00:00:00:00:01"), PortDescription: stringPointer("p1")},
	}
	calls := 0
	collectHostHBNCabling(context.Background(), request, &inv, hostLLDPCapturerFunc(func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error) {
		calls++
		return hostLLDPCaptureResult(), nil
	}))
	if calls != 0 {
		t.Fatalf("host path attempted to override known DPU MAC mismatch: %d calls", calls)
	}
}

func TestHostHBNCablingRequiresSelectedSwitchIdentityAndExactPort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*inventory.Inventory)
		state  string
		code   string
	}{
		{name: "selected chassis mismatch", mutate: func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.HostLLDP.Ports[0].Neighbors[0].ChassisID = "cc:40:f3:55:e1:2d"
		}, state: "failed", code: "HOST_LLDP_SWITCH_MISMATCH"},
		{name: "wrong configured switch port", mutate: func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.HostLLDP.Ports[0].Neighbors[0].PortID = "swp99"
		}, state: "failed", code: "HOST_LLDP_SWITCH_PORT_MISMATCH"},
		{name: "switch local identity absent", mutate: func(i *inventory.Inventory) { i.HBN.Switches[0].Snapshot.Operational.LocalLLDP = nil }, state: "unknown", code: "SWITCH_LLDP_IDENTITY_UNAVAILABLE"},
		{name: "peer not observed", mutate: func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.HostLLDP.Ports[0].Neighbors = []inventory.HostLLDPNeighbor{}
		}, state: "unknown", code: "HOST_LLDP_PEER_UNOBSERVED"},
		{name: "conflicting peers", mutate: func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Network.HostLLDP.Ports[0].Neighbors = append(i.Hosts[0].Runtime.Network.HostLLDP.Ports[0].Neighbors, inventory.HostLLDPNeighbor{SourceMAC: "02:00:00:00:00:02", ChassisIDSubtype: 4, ChassisID: "cc:40:f3:55:e1:2c", PortIDSubtype: 5, PortID: "swp1"})
		}, state: "failed", code: "HOST_LLDP_PEER_AMBIGUOUS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, _ := hostLLDPCablingFixture()
			tc.mutate(&inv)
			check := hostHBNCablingCheck(&inv, &inv.HBN.Switches[0], inv.HBN.Switches[0].Ports[0])
			if check.State != tc.state || check.ReasonCode != tc.code {
				t.Fatalf("got %#v, want %s/%s", check, tc.state, tc.code)
			}
		})
	}
}

func TestCombineHBNCablingNeverOverridesKnownDPUPathOutcome(t *testing.T) {
	for _, state := range []string{"passed", "failed"} {
		base := inventory.Check{ID: "dpf.hbn.cabling", Subject: "leaf/swp1", RequiredPhase: "deploy", State: state, ReasonCode: "BASE_RESULT", Message: "DPU-side source result"}
		host := inventory.Check{ID: base.ID, Subject: base.Subject, RequiredPhase: base.RequiredPhase, State: "failed", ReasonCode: "HOST_LLDP_SWITCH_MISMATCH", Message: "host-side observation"}
		if got := combineHBNCablingCheck(base, host); !reflect.DeepEqual(got, base) {
			t.Fatalf("host observation replaced known %s base outcome: %#v", state, got)
		}
	}
	base := inventory.Check{ID: "dpf.hbn.cabling", Subject: "leaf/swp1", RequiredPhase: "deploy", State: "unknown", ReasonCode: "DPU_MAC_UNAVAILABLE", Message: "missing DPU MAC"}
	host := inventory.Check{ID: base.ID, Subject: base.Subject, RequiredPhase: base.RequiredPhase, State: "passed", ReasonCode: "HOST_LLDP_CABLING_CORROBORATED", Message: "host path"}
	if got := combineHBNCablingCheck(base, host); !reflect.DeepEqual(got, host) {
		t.Fatalf("host observation did not resolve unknown base outcome: %#v", got)
	}
	host.ID = "wrong-check"
	if got := combineHBNCablingCheck(base, host); !reflect.DeepEqual(got, base) {
		t.Fatalf("mismatched host check replaced base outcome: %#v", got)
	}
}

func TestValidateHostLLDPCaptureRejectsIncompleteAndAmbiguousFacts(t *testing.T) {
	request := inventory.HostLLDPCaptureRequest{Ports: []inventory.HostLLDPCapturePort{{PCIAddress: "0000:21:00.0", Interface: "hostpf0"}, {PCIAddress: "0000:21:00.1", Interface: "hostpf1"}}}
	tests := []struct {
		name string
		edit func(*inventory.HostLLDPCapture)
	}{
		{name: "duplicate interface", edit: func(c *inventory.HostLLDPCapture) { c.Ports[1].Interface = c.Ports[0].Interface }},
		{name: "same physical port", edit: func(c *inventory.HostLLDPCapture) { c.Ports[1].PhysicalPortName = "p0" }},
		{name: "missing message", edit: func(c *inventory.HostLLDPCapture) { c.Ports[0].Message = "" }},
		{name: "missing neighbor id", edit: func(c *inventory.HostLLDPCapture) { c.Ports[0].Neighbors[0].PortID = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture := hostLLDPCaptureResult()
			tc.edit(&capture)
			if err := validateHostLLDPCapture(request, capture); err == nil {
				t.Fatal("invalid capture was accepted")
			}
		})
	}
}

func hostLLDPCablingFixture() (inventory.Inventory, config.Request) {
	ports := []inventory.HostLLDPCaptureResult{
		{PCIAddress: "0000:21:00.0", Interface: "hostpf0", PhysicalPortName: "p0", State: "observed", ReasonCode: "HOST_LLDP_PEER_OBSERVED", Message: "peer observed", Neighbors: []inventory.HostLLDPNeighbor{{SourceMAC: "20:4d:52:3e:9a:b8", ChassisIDSubtype: 4, ChassisID: "cc:40:f3:55:e1:2c", PortIDSubtype: 5, PortID: "swp19s0", SystemName: "leaf-a"}}},
		{PCIAddress: "0000:21:00.1", Interface: "hostpf1", PhysicalPortName: "p1", State: "observed", ReasonCode: "HOST_LLDP_PEER_OBSERVED", Message: "peer observed", Neighbors: []inventory.HostLLDPNeighbor{{SourceMAC: "20:4d:52:3e:9a:ba", ChassisIDSubtype: 4, ChassisID: "cc:40:f3:55:e1:2c", PortIDSubtype: 5, PortID: "swp19s1", SystemName: "leaf-a"}}},
	}
	local := &fabric.LLDPLocalIdentity{ChassisID: "cc:40:f3:55:e1:2c", SystemName: "leaf-a"}
	inv := inventory.Inventory{
		DPUs:         []inventory.Device{{ID: "dpu-id", SerialNumber: "SERIAL-1", ObservationState: "observed"}},
		Hosts:        []inventory.Device{{ID: "host-id", InputRef: "/discovery/hosts/0", SSHEndpoint: "ssh://host.example:22", SystemUUID: "123e4567-e89b-12d3-a456-426614174000", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "selected-adapter", SerialNumber: "SERIAL-1", PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0", Netdevs: []string{"hostpf0"}}, {Address: "0000:21:00.1", Netdevs: []string{"hostpf1"}}}}}, Runtime: &inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "hostpf0", PCIAddress: "0000:21:00.0"}, {Name: "hostpf1", PCIAddress: "0000:21:00.1"}}, HostLLDP: &inventory.HostLLDPCapture{Ports: ports}}, Observations: map[string]inventory.Observation{"network-links": {State: "observed"}, "hbn-host-lldp": {State: "observed"}}}}},
		Associations: []inventory.Association{{DPUID: "dpu-id", HostID: "host-id", HostAdapterKey: "selected-adapter", SerialNumber: "SERIAL-1"}},
		HBN:          &inventory.HBNInventory{Switches: []inventory.FabricSwitch{{Name: "switch-a", Endpoint: "ssh://switch.example:22", Ports: []inventory.SwitchPort{{Name: "swp19s0", DPUSerial: "SERIAL-1", DPUInterface: "p0"}, {Name: "swp19s1", DPUSerial: "SERIAL-1", DPUInterface: "p1"}}, Snapshot: fabric.Snapshot{Operational: fabric.OperationalFacts{LocalLLDP: local}}}}},
	}
	target := config.Target{Role: "hosts", Endpoint: "ssh://host.example:22", InputRef: "/discovery/hosts/0", SSH: &config.SSHAccess{}}
	request := config.Request{Targets: []config.Target{target}, RequestTimeout: 5 * time.Second, Switches: []config.SwitchTarget{{Name: "switch-a", Endpoint: "ssh://switch.example:22", Ports: slices.Clone(inv.HBN.Switches[0].Ports)}}}
	return inv, request
}

func hostLLDPCaptureResult() inventory.HostLLDPCapture {
	inv, _ := hostLLDPCablingFixture()
	return cloneHostLLDPCapture(*inv.Hosts[0].Runtime.Network.HostLLDP)
}
