// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

func hostNetworkTestIdentity() HostNetworkIdentity {
	return HostNetworkIdentity{SystemUUID: "123e4567-e89b-12d3-a456-426614174000", AdapterSerial: "CARD-A", PCIAddress: "0000:21:00.0", InterfaceName: "test0", PermanentMAC: "02:00:00:00:00:01", PhysicalPort: "p0"}
}
func hostNetworkTestRequest() HostNetworkRequest {
	return HostNetworkRequest{Identity: hostNetworkTestIdentity(),
		LeaseBinding: strings.Repeat("a", 64), MTU: 9000, HostAddressPrefix: "172.25.0.2/24", Gateway: "172.25.0.1", WorkloadCIDR: "172.25.0.0/16"}
}

func hostNetworkTestSnapshot() hostNetworkSnapshot {
	identity := hostNetworkTestIdentity()
	otherMatchOnly := false
	return hostNetworkSnapshot{WaitOnlineOtherMatchOnly: &otherMatchOnly, OS: "ubuntu:24.04", NetplanVersion: "1.1.2-8ubuntu1~24.04.3", SystemdVersion: "systemd 255 (255.4)", IPVersion: "ip utility, iproute2-6.1.0",
		Networkd: "active", OtherManager: "inactive", UUID: identity.SystemUUID, BootID: "123e4567-e89b-12d3-a456-426614174001",
		VPD: base64.StdEncoding.EncodeToString([]byte("[SN] Serial number: CARD-A\n")), Binding: "/sys/devices/pci0000:20/0000:21:00.0", PhysicalPort: "p0", PermanentMAC: "Permanent address: 02:00:00:00:00:01",
		Link: json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":1500,"min_mtu":68,"max_mtu":9700,"flags":["UP"]}]`), Addresses: json.RawMessage(`[{"addr_info":[]}]`), Routes: json.RawMessage(`[{"dst":"default","gateway":"192.0.2.1","dev":"eth0"}]`),
		Rules4: json.RawMessage(`[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main"},{"priority":32767,"src":"all","table":"default"}]`),
		Rules6: json.RawMessage(`[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main"}]`), ManagementDevice: "eth0", NetworkFile: "/run/systemd/network/10-netplan-data0.network",
		DefaultRoutes6: json.RawMessage(`[]`),
		Merged:         base64.StdEncoding.EncodeToString([]byte("network:\n  version: 2\n  renderer: networkd\n  ethernets:\n    data0:\n      match: {macaddress: '02:00:00:00:00:01'}\n      set-name: test0\n      mtu: 1500\n")),
		SourceDigest:   strings.Repeat("b", 64), Fence: strings.Repeat("c", 64), RuntimeDigest: strings.Repeat("d", 64)}
}

type hostNetworkFakeRemote struct {
	snapshot             hostNetworkSnapshot
	calls                []string
	configureError       bool
	responseOverride     string
	lastConfigureCommand string
}

func (f *hostNetworkFakeRemote) Run(_ context.Context, stage, command string) (string, error) {
	f.calls = append(f.calls, stage)
	if stage == "host-network-ssh-path" {
		return "192.0.2.10 40000 192.0.2.20 22", nil
	}
	if stage == "host-network-snapshot" {
		data, _ := json.Marshal(f.snapshot)
		return string(data), nil
	}
	if !strings.Contains(command, hostNetworkTestRequest().LeaseBinding) && stage != "host-network-configure" && stage != "host-network-inspect" {
		return "", errors.New("unexpected command")
	}
	if f.configureError {
		return "", errors.New("connection acknowledgement lost")
	}
	f.lastConfigureCommand = command
	if f.responseOverride != "" {
		return f.responseOverride, nil
	}
	r := hostNetworkTestRequest()
	data, _ := json.Marshal(HostNetworkReceipt{RequestID: networkDigest(r), State: "complete", RuntimeVerified: true, FileVerified: true})
	return string(data), nil
}

func (f *hostNetworkFakeRemote) RunWithInput(ctx context.Context, stage, command string, _ []byte) (string, error) {
	return f.Run(ctx, stage, command)
}

func (f *hostNetworkFakeRemote) RunWithInputTimeout(ctx context.Context, stage, command string, input []byte, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f.RunWithInput(ctx, stage, command, input)
}

func TestHostNetworkCoordinatorGatesEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*hostNetworkSnapshot)
	}{
		{"wrong UUID", func(s *hostNetworkSnapshot) { s.UUID = "123e4567-e89b-12d3-a456-426614174099" }},
		{"wrong PCI", func(s *hostNetworkSnapshot) { s.Binding = "/sys/devices/0000:21:00.1" }},
		{"wrong permanent MAC", func(s *hostNetworkSnapshot) { s.PermanentMAC = "Permanent address: 02:00:00:00:00:02" }},
		{"wrong physical port", func(s *hostNetworkSnapshot) { s.PhysicalPort = "pf0hpf" }},
		{"management PF", func(s *hostNetworkSnapshot) { s.ManagementDevice = "test0" }},
		{"stale active Netplan logical ID", func(s *hostNetworkSnapshot) { s.NetworkFile = "/run/systemd/network/10-netplan-management.network" }},
		{"selected IPv6 default", func(s *hostNetworkSnapshot) { s.DefaultRoutes6 = json.RawMessage(`[{"dst":"default","dev":"test0"}]`) }},
		{"selected IPv4 default", func(s *hostNetworkSnapshot) { s.Routes = json.RawMessage(`[{"dst":"default","dev":"test0"}]`) }},
		{"missing MTU capabilities", func(s *hostNetworkSnapshot) {
			s.Link = json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":1500,"flags":["UP"]}]`)
		}},
		{"wrong manager", func(s *hostNetworkSnapshot) { s.OtherManager = "active" }},
		{"unavailable tool", func(s *hostNetworkSnapshot) { s.Error = "HOST_NETWORK_TOOL_MISSING" }},
		{"conflicting address", func(s *hostNetworkSnapshot) {
			s.Addresses = json.RawMessage(`[{"addr_info":[{"family":"inet","local":"172.25.0.3","prefixlen":24}]}]`)
		}},
		{"second DPU on same host workload", func(s *hostNetworkSnapshot) {
			s.Routes = json.RawMessage(`[{"dst":"172.25.0.0/16","gateway":"172.25.1.1","dev":"other0"}]`)
		}},
		{"malformed Netplan", func(s *hostNetworkSnapshot) { s.Merged = base64.StdEncoding.EncodeToString([]byte("network: [")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot()}
			tc.edit(&f.snapshot)
			s := hostNetworkSession{remote: f}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := s.configure(ctx, hostNetworkTestRequest()); err == nil {
				t.Fatal("accepted invalid preflight")
			}
			for _, stage := range f.calls {
				if stage == "host-network-configure" {
					t.Fatal("invalid preflight reached effect backend")
				}
			}
		})
	}
}

func TestHostNetworkUnknownAcknowledgementIsBlocking(t *testing.T) {
	f := &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot(), configureError: true}
	s := hostNetworkSession{remote: f}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	receipt, err := s.configure(ctx, hostNetworkTestRequest())
	if err == nil || !receipt.Blocking || receipt.State != "unknown" {
		t.Fatalf("uncertainty lost: %+v %v", receipt, err)
	}
	if strings.Join(f.calls, ",") != "host-network-ssh-path,host-network-snapshot,host-network-configure" {
		t.Fatalf("unexpected retry/cleanup: %v", f.calls)
	}
}

func TestHostNetworkObservationDoesNotSerializePrivateNetplan(t *testing.T) {
	s := hostNetworkTestSnapshot()
	observation, err := validateHostNetworkSnapshot(hostNetworkTestRequest().Identity, s, "192.0.2.10 40000 192.0.2.20 22")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "merged") || strings.Contains(string(data), "network:") || strings.Contains(string(data), s.Merged) {
		t.Fatal("raw configuration escaped")
	}
}

func TestHostNetworkRejectsInvalidDesiredGeometry(t *testing.T) {
	for _, edit := range []func(*HostNetworkRequest){func(r *HostNetworkRequest) { r.MTU = 0 }, func(r *HostNetworkRequest) { r.Gateway = "172.25.1.1" }, func(r *HostNetworkRequest) { r.HostAddressPrefix = "172.25.0.0/24" }, func(r *HostNetworkRequest) { r.WorkloadCIDR = "0.0.0.0/0" }} {
		r := hostNetworkTestRequest()
		edit(&r)
		if validateHostNetworkDesired(r) == nil {
			t.Fatalf("accepted invalid desired: %+v", r)
		}
	}
}

func TestHostNetworkRejectsUntrustedReceiptFields(t *testing.T) {
	for _, edit := range []func(*HostNetworkReceipt){
		func(r *HostNetworkReceipt) { r.ReasonCode = "untrusted\noutput" },
		func(r *HostNetworkReceipt) { r.JournalPath = "/etc/foreign" },
		func(r *HostNetworkReceipt) { r.FragmentSHA256 = "invalid" },
		func(r *HostNetworkReceipt) { r.State = "unexpected" },
		func(r *HostNetworkReceipt) { r.RuntimeVerified = false },
	} {
		r := HostNetworkReceipt{RequestID: networkDigest(hostNetworkTestRequest()), State: "complete", RuntimeVerified: true, FileVerified: true}
		edit(&r)
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		remote := &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot(), responseOverride: string(data)}
		session := hostNetworkSession{remote: remote}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		receipt, err := session.configure(ctx, hostNetworkTestRequest())
		cancel()
		if err == nil || !receipt.Blocking || receipt.State != "unknown" || !hostNetworkReason.MatchString(Code(err)) || !hostNetworkReason.MatchString(receipt.ReasonCode) {
			t.Fatalf("untrusted receipt escaped: %+v %v", receipt, err)
		}
	}
}

func TestHostNetworkRejectsUnknownRoutingRuleSelectors(t *testing.T) {
	if standardHostNetworkRules([]byte(`[{"priority":0,"src":"all","table":"local"},{"priority":32766,"src":"all","table":"main","fwmark":"0x1"}]`)) {
		t.Fatal("ignored unvalidated rule selector")
	}
}

func TestHostNetworkReadOnlyFactsAcceptUnrelatedIPv6Default(t *testing.T) {
	snapshot := hostNetworkTestSnapshot()
	snapshot.DefaultRoutes6 = json.RawMessage(`[{"dst":"default","dev":"eth0"}]`)
	if _, err := validateHostNetworkSnapshot(hostNetworkTestIdentity(), snapshot, "192.0.2.10 40000 192.0.2.20 22"); err != nil {
		t.Fatalf("unrelated IPv6 default must be preserved: %v", err)
	}
}

func TestHostNetworkPublicIdentityAndDeadlineInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := config.Target{Role: "hosts", SSH: &config.SSHAccess{}}
	for _, edit := range []func(*HostNetworkIdentity){
		func(i *HostNetworkIdentity) { i.SystemUUID = "invalid" },
		func(i *HostNetworkIdentity) { i.PCIAddress = "../../invalid" },
		func(i *HostNetworkIdentity) { i.InterfaceName = ".." },
		func(i *HostNetworkIdentity) { i.InterfaceName = "test0;id" },
		func(i *HostNetworkIdentity) { i.PhysicalPort = "p1" },
		func(i *HostNetworkIdentity) { i.PermanentMAC = "00:00:00:00:00:00" },
	} {
		identity := hostNetworkTestIdentity()
		edit(&identity)
		if err := validateHostNetworkAccess(ctx, target, identity, time.Second); Code(err) != "HOST_NETWORK_IDENTITY_INVALID" {
			t.Fatalf("identity accepted: %+v %v", identity, err)
		}
	}
	if err := validateHostNetworkAccess(context.Background(), target, hostNetworkTestIdentity(), time.Second); Code(err) != "HOST_NETWORK_DEADLINE_REQUIRED" {
		t.Fatalf("unbounded call accepted: %v", err)
	}
	snapshot := hostNetworkTestSnapshot()
	snapshot.Link = json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":1500,"flags":["UP"]}]`)
	if _, err := validateHostNetworkSnapshot(hostNetworkTestIdentity(), snapshot, "192.0.2.10 40000 192.0.2.20 22"); Code(err) != "HOST_NETWORK_MTU_CAPABILITY_UNAVAILABLE" {
		t.Fatalf("unknown MTU bound reason: %v", err)
	}
}

func TestHostNetworkBackendMinimumMTUIsDistinctFromKernel(t *testing.T) {
	for _, mtu := range []int{576, MinimumHostWorkloadMTU - 1} {
		request := hostNetworkTestRequest()
		request.MTU = mtu
		remote := &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot()}
		session := hostNetworkSession{remote: remote}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := session.configure(ctx, request)
		cancel()
		if Code(err) != "HOST_NETWORK_MTU_UNSUPPORTED" || len(remote.calls) != 0 {
			t.Fatalf("low backend MTU reached transport: mtu=%d calls=%v err=%v", mtu, remote.calls, err)
		}
	}
	request := hostNetworkTestRequest()
	request.MTU = MinimumHostWorkloadMTU
	if err := validateHostNetworkDesired(request); err != nil {
		t.Fatalf("qualified boundary rejected: %v", err)
	}
	observation, err := validateHostNetworkSnapshot(request.Identity, hostNetworkTestSnapshot(), "192.0.2.10 40000 192.0.2.20 22")
	if err != nil || observation.MinMTU != 68 || observation.MinimumWorkloadMTU != MinimumHostWorkloadMTU {
		t.Fatalf("kernel/backend minima conflated: %+v %v", observation, err)
	}
}

func TestHostNetworkCurrentMTUBelowBackendMinimumStopsBeforeTransportEffects(t *testing.T) {
	snapshot := hostNetworkTestSnapshot()
	snapshot.Link = json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":576,"min_mtu":68,"max_mtu":9700,"flags":["UP"]}]`)
	remote := &hostNetworkFakeRemote{snapshot: snapshot}
	session := hostNetworkSession{remote: remote}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.configure(ctx, hostNetworkTestRequest())
	if Code(err) != "HOST_NETWORK_MTU_UNSUPPORTED" {
		t.Fatalf("unsupported current MTU accepted: %v", err)
	}
	for _, stage := range remote.calls {
		if stage == "host-network-configure" {
			t.Fatal("unsupported current MTU reached effects")
		}
	}
}

// Malformed backend identifiers must be rejected in the returned receipt as
// well as the error: deployment retains the receipt before examining the error.
func TestHostNetworkRejectsUnallowlistedReadReasonsInEveryReceiptState(t *testing.T) {
	for _, state := range []string{"complete", "verified-external", "refused", "rolled-back", "partial"} {
		for _, reason := range []string{"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7_SECRET", "HOST_NETWORK_READ_UNKNOWN_IP_EXIT_7", "HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_256"} {
			t.Run(state+"/"+reason, func(t *testing.T) {
				data, err := json.Marshal(HostNetworkReceipt{RequestID: networkDigest(hostNetworkTestRequest()), State: state, ReasonCode: reason, RuntimeVerified: true, FileVerified: true})
				if err != nil {
					t.Fatal(err)
				}
				session := hostNetworkSession{remote: &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot(), responseOverride: string(data)}}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				receipt, err := session.configure(ctx, hostNetworkTestRequest())
				if Code(err) != "HOST_NETWORK_RESPONSE_INVALID" || receipt.State != "unknown" || !receipt.Blocking || receipt.ReasonCode != "HOST_NETWORK_RESPONSE_INVALID" {
					t.Fatalf("untrusted reason escaped in receipt or error: %+v %v", receipt, err)
				}
				retained, marshalErr := json.Marshal(receipt)
				if marshalErr != nil || strings.Contains(string(retained), reason) {
					t.Fatalf("untrusted reason retained: %s %v", retained, marshalErr)
				}
			})
		}
	}
}

func TestHostNetworkReadFailureCannotVerifySuccess(t *testing.T) {
	const reason = "HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7"
	for _, state := range []string{"complete", "verified-external", "refused", "rolled-back", "partial"} {
		t.Run(state, func(t *testing.T) {
			data, err := json.Marshal(HostNetworkReceipt{RequestID: networkDigest(hostNetworkTestRequest()), State: state, ReasonCode: reason, RuntimeVerified: true, FileVerified: true})
			if err != nil {
				t.Fatal(err)
			}
			session := hostNetworkSession{remote: &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot(), responseOverride: string(data)}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			receipt, err := session.configure(ctx, hostNetworkTestRequest())
			if state == "complete" || state == "verified-external" {
				if Code(err) != "HOST_NETWORK_RESPONSE_INVALID" || receipt.ReasonCode != "HOST_NETWORK_RESPONSE_INVALID" || receipt.State != "unknown" || !receipt.Blocking {
					t.Fatalf("read failure verified success: %+v %v", receipt, err)
				}
			} else if Code(err) != reason || receipt.ReasonCode != reason || receipt.State != state {
				t.Fatalf("valid typed failure lost: %+v %v", receipt, err)
			}
		})
	}
}

func TestHostNetworkPrivateOptionalBooleanPreservesFalseAndTrue(t *testing.T) {
	for _, value := range []bool{false, true} {
		snapshot := hostNetworkTestSnapshot()
		merged, err := base64.StdEncoding.DecodeString(snapshot.Merged)
		if err != nil {
			t.Fatal(err)
		}
		optional := "false"
		if value {
			optional = "true"
		}
		snapshot.Merged = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(merged), "      mtu: 1500\n", "      mtu: 1500\n      optional: "+optional+"\n", 1)))
		remote := &hostNetworkFakeRemote{snapshot: snapshot}
		session := hostNetworkSession{remote: remote}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = session.configure(ctx, hostNetworkTestRequest())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(remote.lastConfigureCommand)
		if len(fields) == 0 {
			t.Fatal("configure command missing")
		}
		data, err := base64.StdEncoding.DecodeString(fields[len(fields)-1])
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(data, &payload) != nil {
			t.Fatal("invalid private request")
		}
		got, present := payload["waitOnlineOptional"]
		if !present || string(got) != optional {
			t.Fatalf("bool omitted or changed: %s", data)
		}
	}
}

func TestHostNetworkReadOnlyWaitOnlineMatchingAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		other    *bool
		optional bool
		code     string
	}{
		{"direct or absent", boolPointer(false), false, ""},
		{"other only nonoptional", boolPointer(true), false, "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED"},
		{"other only optional", boolPointer(true), true, ""},
		{"missing capability", nil, false, "HOST_NETWORK_RESPONSE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := hostNetworkTestSnapshot()
			snapshot.WaitOnlineOtherMatchOnly = tc.other
			if tc.optional {
				merged, _ := base64.StdEncoding.DecodeString(snapshot.Merged)
				snapshot.Merged = base64.StdEncoding.EncodeToString(append(merged, []byte("      optional: true\n")...))
			}
			remote := &hostNetworkFakeRemote{snapshot: snapshot}
			session := hostNetworkSession{remote: remote}
			observation, err := session.observe(context.Background(), hostNetworkTestIdentity())
			if (err == nil) != (tc.code == "") || (err != nil && Code(err) != tc.code) {
				t.Fatalf("read-only matching: %v; want %s", err, tc.code)
			}
			if tc.code == "" {
				data, err := json.Marshal(observation)
				if err != nil || strings.Contains(string(data), "waitOnline") || strings.Contains(string(data), "merged") {
					t.Fatalf("private capability leaked: %s %v", data, err)
				}
			}
			for _, call := range remote.calls {
				if call == "host-network-configure" {
					t.Fatal("read-only qualification configured host")
				}
			}
		})
	}
	var snapshot hostNetworkSnapshot
	if json.Unmarshal([]byte(`{"waitOnlineOtherMatchOnly":"false"}`), &snapshot) == nil {
		t.Fatal("wrong private capability type accepted")
	}
}

func boolPointer(value bool) *bool { return &value }

func TestHostNetworkInspectUsesSeparateReadOnlyAction(t *testing.T) {
	remote := &hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot()}
	session := &hostNetworkSession{remote: remote}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.configureAction(ctx, hostNetworkTestRequest(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote.lastConfigureCommand, " -- inspect ") {
		t.Fatal("inspection used configuration action")
	}
	for _, call := range remote.calls {
		if call == "host-network-configure" {
			t.Fatal("inspection called configure transport")
		}
	}
	// The backend must refuse inspect before any mutation/journal intent path.
	stop := strings.Index(hostNetworkBackend, `[[ "$action" != inspect ]] || die HOST_NETWORK_PRIOR_INTENT_BLOCKS`)
	mutation := strings.Index(hostNetworkBackend, "if ! write_journal intent ''")
	if stop < 0 || mutation < stop {
		t.Fatal("inspect may fall through into mutation")
	}
}
