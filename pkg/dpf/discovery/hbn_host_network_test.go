// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestHostBackendDiscoveryUsesDesiredProfileAndExactSSHAuthority(t *testing.T) {
	for _, mtu := range []int{9000, 8800} {
		inv, request := hostBackendDiscoveryFixture(t, mtu)
		before := rawHostHBNTestInventory(t, inv)
		calls := 0
		observer := func(_ context.Context, target config.Target, identity sshprobe.HostNetworkIdentity, timeout time.Duration) (sshprobe.HostNetworkObservation, error) {
			calls++
			if target.SSH != request.Targets[0].SSH || target.Endpoint != inv.Hosts[0].SSHEndpoint || target.InputRef != inv.Hosts[0].InputRef || timeout != request.RequestTimeout {
				t.Fatal("backend observer received another SSH authority")
			}
			if identity.PCIAddress != "0000:21:00.0" || identity.InterfaceName != "hostpf0" || identity.PermanentMAC != "20:4d:52:3e:9a:b8" || identity.AdapterSerial != inv.DPUs[0].SerialNumber || identity.PhysicalPort != "p0" {
				t.Fatalf("backend observer received a guessed physical identity: %+v", identity)
			}
			maximum := 9216
			if mtu == 8800 {
				maximum = 8900 // A hardcoded default of 9000 must fail this override case.
			}
			return sshprobe.HostNetworkObservation{Identity: identity, MTU: 1500, MinMTU: 68, MaxMTU: maximum, MinimumWorkloadMTU: 1280, BackendVersion: "1.1.2-8ubuntu1", ManagementInterface: "mgmt0"}, nil
		}
		checks := collectHostHBNNetworking(context.Background(), request, &inv, observer, false)
		check := checks["SERIAL-1"]
		if calls != 1 || check.State != "passed" || check.ReasonCode != "HOST_NETWORK_BACKEND_SUPPORTED" || !strings.Contains(check.Message, fmt.Sprintf("desired MTU %d", mtu)) || !strings.Contains(check.Message, "current 1500") ||
			!strings.Contains(check.Message, "Exact host address and route checks follow the real DPF allocation") {
			t.Fatalf("backend prerequisite result must preserve desired/current distinction: calls=%d check=%+v", calls, check)
		}
		if request.HBN.WorkloadMTU != mtu || string(before) != string(rawHostHBNTestInventory(t, inv)) {
			t.Fatal("observed MTU replaced desired intent or discovery mutated host inventory")
		}
		final := HostHBNChecks(&inv, checks)
		if got := hostHBNCheck(t, final, "host-network-backend", "SERIAL-1 @ worker-01"); got.State != "passed" {
			t.Fatalf("pure readiness did not consume the current backend result: %+v", got)
		}
	}
}

func TestHBNProgressQuotesObservedHostAndBackendValues(t *testing.T) {
	hostname := "worker-\x1b[31m\u202e"
	backendVersion := "1.1.2-8ubuntu1\x1b[31m\u202e"
	inv, request := hostBackendDiscoveryFixture(t, 9000)
	inv.Hosts[0].Runtime.Hostname = hostname
	var progress strings.Builder
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
	observer := func(_ context.Context, _ config.Target, identity sshprobe.HostNetworkIdentity, _ time.Duration) (sshprobe.HostNetworkObservation, error) {
		return sshprobe.HostNetworkObservation{Identity: identity, MTU: 1500, MinMTU: 68, MaxMTU: 9216, MinimumWorkloadMTU: 1280,
			BackendVersion: backendVersion, ManagementInterface: "mgmt0"}, nil
	}

	checks := collectHostHBNNetworking(ctx, request, &inv, observer, false)
	output := progress.String()
	for _, raw := range []string{"\x1b", "\u202e"} {
		if strings.Contains(output, raw) {
			t.Fatalf("progress output contains raw control value %q: %q", raw, output)
		}
	}
	for _, r := range output {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("progress output contains raw control rune %U: %q", r, output)
		}
	}
	for _, quoted := range []string{strconv.Quote(hostname), strconv.Quote(backendVersion)} {
		if !strings.Contains(output, quoted) {
			t.Fatalf("progress output does not contain quoted value %q: %q", quoted, output)
		}
	}
	if inv.Hosts[0].Runtime.Hostname != hostname || !strings.Contains(checks["SERIAL-1"].Subject, hostname) {
		t.Fatalf("display escaping changed the retained host fact or check subject: host=%q check=%+v", inv.Hosts[0].Runtime.Hostname, checks["SERIAL-1"])
	}
}

func TestHostLLDPProgressQuotesObservedHost(t *testing.T) {
	hostname := "worker-\x1b[31m\u202e"
	inv, request := hostLLDPCablingFixture()
	inv.Hosts[0].Runtime.Hostname = hostname
	var progress strings.Builder
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
	capturer := hostLLDPCapturerFunc(func(context.Context, config.Target, inventory.HostLLDPCaptureRequest, time.Duration) (inventory.HostLLDPCapture, error) {
		return hostLLDPCaptureResult(), nil
	})

	collectHostHBNCabling(ctx, request, &inv, capturer)
	output := progress.String()
	if strings.Contains(output, "\x1b") || strings.Contains(output, "\u202e") || !strings.Contains(output, strconv.Quote(hostname)) {
		t.Fatalf("LLDP progress did not safely quote the observed host: %q", output)
	}
}

func TestHostLLDPUnavailableProgressQuotesObservedDPU(t *testing.T) {
	inv, request := hostLLDPCablingFixture()
	serial := inv.DPUs[0].SerialNumber + "\x1b[31m\u202e"
	inv.DPUs[0].SerialNumber = serial
	inv.Hosts[0].Adapters[0].SerialNumber = serial
	inv.Associations[0].SerialNumber = serial
	for i := range request.Switches {
		for j := range request.Switches[i].Ports {
			request.Switches[i].Ports[j].DPUSerial = serial
		}
	}
	for i := range inv.HBN.Switches {
		for j := range inv.HBN.Switches[i].Ports {
			inv.HBN.Switches[i].Ports[j].DPUSerial = serial
		}
	}
	var progress strings.Builder
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
	collectHostHBNCabling(ctx, request, &inv, nil)
	output := progress.String()
	if strings.Contains(output, "\x1b") || strings.Contains(output, "\u202e") || !strings.Contains(output, strconv.Quote(serial)) {
		t.Fatalf("unavailable LLDP progress did not safely quote the observed DPU: %q", output)
	}
}

func TestHostBackendDiscoveryRejectsMultipleSelectedDPUsPerHostBeforeReads(t *testing.T) {
	inv, request := hostBackendDiscoveryFixture(t, 9000)
	inv.DPUs = append(inv.DPUs, inventory.Device{ID: "dpu-second", SerialNumber: "SERIAL-2", ObservationState: "observed"})
	inv.Hosts[0].Adapters = append(inv.Hosts[0].Adapters, inventory.Adapter{Key: "second-card", SerialNumber: "SERIAL-2", IdentityState: "known"})
	inv.Associations = append(inv.Associations, inventory.Association{DPUID: "dpu-second", SerialNumber: "SERIAL-2", HostID: inv.Hosts[0].ID, HostAdapterKey: "second-card"})
	checks := collectHostHBNNetworking(context.Background(), request, &inv, func(context.Context, config.Target, sshprobe.HostNetworkIdentity, time.Duration) (sshprobe.HostNetworkObservation, error) {
		t.Fatal("unsupported host routing cohort must be rejected before SSH reads")
		return sshprobe.HostNetworkObservation{}, nil
	}, false)
	for _, serial := range []string{"SERIAL-1", "SERIAL-2"} {
		if got := checks[serial]; got.State != "failed" || got.ReasonCode != "HOST_NETWORK_MULTIPLE_DPUS_UNSUPPORTED" {
			t.Fatalf("host cohort %s was not rejected before effects: %+v", serial, got)
		}
	}
}

func TestHostBackendDiscoveryRetainsRequiredUnknownAndMismatchReasons(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*sshprobe.HostNetworkObservation) error
		state  string
		reason string
	}{
		{name: "required tool absent", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_TOOL_MISSING", Blocking: false}
		}, state: "unknown", reason: "HOST_NETWORK_TOOL_MISSING"},
		{name: "wait-online cannot be safely projected", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED", Blocking: false}
		}, state: "unknown", reason: "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED"},
		{name: "wait-online matches other interfaces only", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED", Blocking: false}
		}, state: "unknown", reason: "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED"},
		{name: "OD tool absent", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_TOOL_MISSING_OD", Blocking: false}
		}, state: "unknown", reason: "HOST_NETWORK_TOOL_MISSING_OD"},
		{name: "Netplan source changed during observation", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_SOURCE_CHANGED", Blocking: false}
		}, state: "unknown", reason: "HOST_NETWORK_SOURCE_CHANGED"},
		{name: "unsupported backend", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_BACKEND_UNSUPPORTED", Blocking: false}
		}, state: "failed", reason: "HOST_NETWORK_BACKEND_UNSUPPORTED"},
		{name: "native observer rejects unsupported current MTU", change: func(*sshprobe.HostNetworkObservation) error {
			return &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_MTU_UNSUPPORTED", Blocking: false}
		}, state: "failed", reason: "HOST_NETWORK_MTU_UNSUPPORTED"},
		{name: "changed identity", change: func(o *sshprobe.HostNetworkObservation) error {
			o.Identity.PermanentMAC = "20:4d:52:3e:9a:bb"
			return nil
		}, state: "failed", reason: "HOST_NETWORK_IDENTITY_CHANGED"},
		{name: "unknown MTU range", change: func(o *sshprobe.HostNetworkObservation) error { o.MinMTU, o.MaxMTU = 0, 0; return nil }, state: "unknown", reason: "HOST_NETWORK_BACKEND_FACTS_INCOMPLETE"},
		{name: "unknown backend MTU minimum", change: func(o *sshprobe.HostNetworkObservation) error { o.MinimumWorkloadMTU = 0; return nil }, state: "unknown", reason: "HOST_NETWORK_BACKEND_FACTS_INCOMPLETE"},
		{name: "invalid backend MTU minimum", change: func(o *sshprobe.HostNetworkObservation) error { o.MinimumWorkloadMTU = o.MaxMTU + 1; return nil }, state: "unknown", reason: "HOST_NETWORK_BACKEND_FACTS_INCOMPLETE"},
		{name: "insufficient MTU range", change: func(o *sshprobe.HostNetworkObservation) error { o.MaxMTU = 1500; return nil }, state: "failed", reason: "HOST_NETWORK_MTU_UNSUPPORTED"},
		{name: "management interface conflict", change: func(o *sshprobe.HostNetworkObservation) error {
			o.ManagementInterface = o.Identity.InterfaceName
			return nil
		}, state: "unknown", reason: "HOST_NETWORK_BACKEND_FACTS_INCOMPLETE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv, request := hostBackendDiscoveryFixture(t, 9000)
			checks := collectHostHBNNetworking(context.Background(), request, &inv, func(_ context.Context, _ config.Target, identity sshprobe.HostNetworkIdentity, _ time.Duration) (sshprobe.HostNetworkObservation, error) {
				o := sshprobe.HostNetworkObservation{Identity: identity, MTU: 1500, MinMTU: 68, MaxMTU: 9216, MinimumWorkloadMTU: 1280, BackendVersion: "1.1.2", ManagementInterface: "mgmt0"}
				err := test.change(&o)
				return o, err
			}, false)
			check := checks["SERIAL-1"]
			if check.State != test.state || check.ReasonCode != test.reason || check.Message == "" {
				t.Fatalf("required backend issue lost its reason: %+v", check)
			}
			// The high-level readiness projection must retain this exact current
			// backend result, including its state and reason, and cannot pass it.
			inv.HBN = &inventory.HBNInventory{Profile: *request.HBN}
			readiness := evaluateHBN(&inv, nil, checks)
			readinessCheck := hostHBNCheck(t, readiness.Checks, "host-network-backend", "SERIAL-1 @ worker-01")
			if readinessCheck.State != test.state || readinessCheck.ReasonCode != test.reason || readiness.State != "blocked" {
				t.Fatalf("readiness lost the prerequisite result or passed HBN: check=%+v readiness=%+v", readinessCheck, readiness)
			}
		})
	}
}

func TestHostBackendDiscoverySeparatesKernelAndBackendMTUMinimum(t *testing.T) {
	for _, test := range []struct {
		name           string
		desired        int
		backendMinimum int
		current        int
		want           string
	}{
		{name: "valid IPv4 intent unsupported by backend", desired: 576, backendMinimum: 1280, want: "failed"},
		{name: "backend boundary supported", desired: 1280, backendMinimum: 1280, want: "passed"},
		{name: "observed backend minimum remains authoritative", desired: 1500, backendMinimum: 2000, current: 2500, want: "failed"},
		{name: "raising unsupported current MTU can change IPv6 state", desired: 9000, backendMinimum: 1280, current: 576, want: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv, request := hostBackendDiscoveryFixture(t, test.desired)
			current := test.current
			if current == 0 {
				current = 1500
			}
			checks := collectHostHBNNetworking(context.Background(), request, &inv, func(_ context.Context, _ config.Target, identity sshprobe.HostNetworkIdentity, _ time.Duration) (sshprobe.HostNetworkObservation, error) {
				return sshprobe.HostNetworkObservation{Identity: identity, MTU: current, MinMTU: 68, MaxMTU: 9216,
					MinimumWorkloadMTU: test.backendMinimum, BackendVersion: "1.1.2", ManagementInterface: "mgmt0"}, nil
			}, false)
			check := checks["SERIAL-1"]
			if check.State != test.want || request.HBN.WorkloadMTU != test.desired {
				t.Fatalf("backend limit must block unsupported intent without replacing it: %+v desired=%d", check, request.HBN.WorkloadMTU)
			}
			if test.want == "failed" && (check.ReasonCode != "HOST_NETWORK_MTU_UNSUPPORTED" || !strings.Contains(check.Message, fmt.Sprintf("backend minimum %d", test.backendMinimum)) || !strings.Contains(check.Message, "kernel range 68–9216")) {
				t.Fatalf("backend and kernel limits must be distinguishable: %+v", check)
			}
		})
	}
}

func TestHostBackendDiscoveryNeverGuessesMissingAccessOrPermanentMAC(t *testing.T) {
	for _, missing := range []string{"collector", "access", "permanentMAC"} {
		t.Run(missing, func(t *testing.T) {
			inv, request := hostBackendDiscoveryFixture(t, 9000)
			calls := 0
			observer := HostNetworkObserver(func(context.Context, config.Target, sshprobe.HostNetworkIdentity, time.Duration) (sshprobe.HostNetworkObservation, error) {
				calls++
				t.Fatal("discovery attempted backend inspection without required authority or identity")
				return sshprobe.HostNetworkObservation{}, nil
			})
			switch missing {
			case "collector":
				observer = nil
			case "access":
				request.Targets = nil
			case "permanentMAC":
				inv.Hosts[0].Runtime.Network.Links[0].PermanentMAC = ""
			}
			checks := collectHostHBNNetworking(context.Background(), request, &inv, observer, false)
			got := hostHBNCheck(t, HostHBNChecks(&inv, checks), "host-network-backend", "SERIAL-1 @ worker-01")
			if calls != 0 || got.State != "unknown" || got.ReasonCode == "" || got.Message == "" {
				t.Fatalf("missing prerequisite became a readiness pass: %+v", got)
			}
		})
	}
}

func hostBackendDiscoveryFixture(t *testing.T, mtu int) (inventory.Inventory, config.Request) {
	t.Helper()
	inv := hostHBNTestInventory()
	inv.Hosts[0].SystemUUID = "12345678-1234-1234-1234-123456789abc"
	inv.Hosts[0].InputRef = "/discovery/hosts/0"
	inv.Hosts[0].Adapters[0].IdentityState = "known"
	inv.Hosts[0].Runtime.Network.Links = []inventory.NetworkLink{
		{Name: "hostpf0", PCIAddress: "0000:21:00.0", PhysicalPortName: "p0", MAC: "02:00:00:00:00:01", PermanentMAC: "20:4d:52:3e:9a:b8"},
		{Name: "hostpf1", PCIAddress: "0000:21:00.1", PhysicalPortName: "p1", MAC: "02:00:00:00:00:02", PermanentMAC: "20:4d:52:3e:9a:b9"},
		{Name: "mgmt0", PCIAddress: "0000:35:00.0", PhysicalPFRoots: []string{"0000:35:00.0"}},
	}
	profile, err := hbn.Resolve(hbn.Options{WorkloadMTU: &mtu})
	if err != nil {
		t.Fatal(err)
	}
	request := config.Request{HBN: &profile, RequestTimeout: time.Second, Targets: []config.Target{{Role: "hosts", InputRef: inv.Hosts[0].InputRef, Endpoint: inv.Hosts[0].SSHEndpoint, SSH: &config.SSHAccess{}}}}
	return inv, request
}
