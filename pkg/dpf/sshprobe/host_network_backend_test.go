// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests are opt-in and refuse privileged/host-network containers. The
// container must be disposable Ubuntu24.04 with netplan/iproute/jq installed.
// Hardware and systemd PID1 are explicit test stubs; file publication, Netplan
// rendering, fsync and dummy-link netlink effects use the real Ubuntu tools.
func TestHostNetworkBackendIsolatedUbuntu(t *testing.T) {
	container := os.Getenv("DPF_HOST_NETWORK_TEST_CONTAINER")
	if container == "" {
		t.Skip("requires explicitly selected disposable Ubuntu24.04 NET_ADMIN container")
	}
	if !strings.HasPrefix(container, "host-network-transaction-") {
		t.Fatal("test container must use the dedicated transaction-test name prefix")
	}
	inspect := containerCommand(t, container, "inspect")
	if !strings.Contains(inspect, `"Privileged":false`) || strings.Contains(inspect, `"NetworkMode":"host"`) || !strings.Contains(inspect, `NET_ADMIN`) || !strings.Contains(inspect, `"Mounts":[]`) {
		t.Fatalf("test container isolation is not established: %s", inspect)
	}
	setupHostNetworkContainer(t, container)
	t.Run("wait-online finite selected projection and ownership", func(t *testing.T) {
		begin := strings.Index(hostNetworkBackend, "value_is() {")
		end := strings.Index(hostNetworkBackend, "prepare_shadow() {")
		if begin < 0 || end <= begin {
			t.Fatal("wait-online helper boundary missing")
		}
		containerShell(t, container, "export LC_ALL=C\numask 077\n"+hostNetworkBackend[begin:end]+hostNetworkWaitOnlineUnitChecks)
	})
	t.Run("matching context private roundtrip and drift guards", func(t *testing.T) {
		start := time.Now()
		begin := strings.Index(hostNetworkBackend, "capture_command() {")
		end := strings.Index(hostNetworkBackend, "secure_file() {")
		if begin < 0 || end <= begin {
			t.Fatal("matching context helpers missing")
		}
		functions := hostNetworkBackend[begin:end]
		reader := strings.Index(functions, "read_generator_context() {")
		writer := strings.Index(functions, "write_generator_context() {")
		if reader < 0 || writer <= reader {
			t.Fatal("matching context reader boundary missing")
		}
		functions = functions[:reader] + strings.ReplaceAll(functions[reader:writer], "/sys/class/net", "${context_fixture}") + functions[writer:]
		containerShell(t, container, "export LC_ALL=C\numask 077\n"+functions+hostNetworkContextUnitChecks)
		t.Logf("private capture/materialization/drift unit checks including Docker transport: %s", time.Since(start))
	})
	for _, tc := range []struct {
		name     string
		fault    string
		state    string
		mtu      int
		address  bool
		fragment bool
	}{
		{"successful transaction and exact repeat", "", "complete", 9000, true, true},
		{"optional selected first IP preserves absent contribution", "optional-selected", "complete", 9000, true, true},
		{"late owned wait-online corruption remains blocking", "wait-owned-corrupt", "partial", 1500, false, true},
		{"MAC-matched management reproduces real wait-online context", "mac-matched-management", "complete", 9000, true, true},
		{"interface-set drift after fragment leaves blocking owned intent", "context-set-after-fragment", "partial", 1500, false, true},
		{"confirmed absent NetworkManager supports transaction and repeat", "nm-absent", "complete", 9000, true, true},
		{"confirmed absent NetworkManager preserves compensation", "nm-absent-route", "rolled-back", 1500, false, false},
		{"address then route failure compensates MTU", "route", "rolled-back", 1500, false, false},
		{"MTU rollback failure stays blocking", "rollback-mtu", "partial", 9000, false, false},
		{"shadow generator failure reaches outer rollback once", "shadow-generate", "rolled-back", 1500, false, false},
		{"generation failure removes exact fragment", "generate", "rolled-back", 1500, false, false},
		{"matching digest output with failed status blocks before publication", "matching-digest-status", "rolled-back", 1500, false, false},
		{"source drift blocks effects and cleanup", "source-drift", "partial", 1500, false, true},
		{"interruption after fragment blocks replay", "interrupt-generate", "partial", 1500, false, true},
		{"interruption after MTU command blocks replay", "interrupt-mtu", "partial", 9000, false, true},
		{"interruption after durable intent blocks replay", "interrupt-intent", "unknown", 1500, false, false},
		{"interruption after address command blocks replay", "interrupt-address", "partial", 9000, true, true},
		{"interruption after route command blocks replay", "interrupt-route", "partial", 9000, true, true},
		{"journal file sync failure prevents publication", "sync-journal-file", "refused", 1500, false, false},
		{"journal directory sync failure blocks replay", "sync-journal-directory", "partial", 1500, false, false},
		{"fragment allocation failure reaches outer transaction once", "mktemp-fragment", "rolled-back", 1500, false, false},
		{"fragment sync failure compensates only its temporary file", "sync-fragment", "rolled-back", 1500, false, false},
		{"valid runtime output with failed status cannot verify", "runtime-status", "rolled-back", 1500, false, false},
		{"readback mismatch compensates", "readback", "rolled-back", 1500, false, false},
		{"capture readback failure compensates after address addition", "identity-readback", "rolled-back", 1500, false, false},
		{"persistent capture failure leaves blocking intent without unsafe cleanup", "identity-readback-persistent", "partial", 9000, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHostNetworkContainer(t, container)
			if tc.fault == "mac-matched-management" {
				prepareHostNetworkMACMatchedManagement(t, container)
			}
			if tc.fault == "optional-selected" {
				containerShell(t, container, `sed -i '/      set-name: test0/a\      optional: true' /etc/netplan/10-base.yaml;/usr/sbin/netplan.real generate >/dev/null`)
			}
			if tc.fault == "" || tc.fault == "optional-selected" {
				t.Logf("real matching baseline: %s", strings.TrimSpace(containerShell(t, container, `/usr/sbin/ip.real -j link show dev test0 | jq -r '.[0].address';grep '^ExecStart=' /run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf`)))
			}
			preservedBefore := preservedHostNetworkContainerState(t, container)
			if tc.fault != "" {
				containerShell(t, container, "touch /tmp/l8k-fault-"+tc.fault)
			}
			session := &hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container, hostileEnvironment: tc.state == "complete", recordTimings: tc.state == "complete"}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			receipt, err := session.configure(ctx, hostNetworkTestRequest())
			if tc.fault == "context-set-after-fragment" {
				containerShell(t, container, `test -f /tmp/l8k-context-link-created;/usr/sbin/ip.real link show l8kctx0 >/dev/null;test -z "$(cat /tmp/l8k-effects.log)"`)
				defer containerShell(t, container, `/usr/sbin/ip.real link del l8kctx0;rm /tmp/l8k-context-link-created`)
				if receipt.ReasonCode != "HOST_NETWORK_COMPENSATION_UNCERTAIN" {
					t.Fatalf("context drift must block unsafe cleanup: %+v %v", receipt, err)
				}
			}
			if tc.fault == "identity-readback" && receipt.ReasonCode != "HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7" {
				t.Fatalf("readback reason lost: %+v %v", receipt, err)
			}
			if receipt.State != tc.state || (err == nil) != (tc.state == "complete") {
				t.Fatalf("state=%+v err=%v", receipt, err)
			}
			state := containerShell(t, container, `/usr/sbin/ip.real -j link show dev test0 | jq -r '.[0].mtu'; /usr/sbin/ip.real -j -4 addr show dev test0 | jq '[.[].addr_info[]|select(.family=="inet")]|length'; find /etc/netplan -name '99-l8k-workload-*.yaml' | wc -l`)
			wantAddress, wantFragment := 0, 0
			if tc.address {
				wantAddress = 1
			}
			if tc.fragment {
				wantFragment = 1
			}
			if strings.TrimSpace(state) != fmt.Sprintf("%d\n%d\n%d", tc.mtu, wantAddress, wantFragment) {
				t.Fatalf("runtime/files differ: %q", state)
			}
			if preservedHostNetworkContainerState(t, container) != preservedBefore {
				t.Fatal("management/PF1 runtime or generated files changed")
			}
			capabilities := containerShell(t, container, "sort -u /tmp/l8k-netlink-capabilities.log")
			if strings.TrimSpace(capabilities) != "CapEff:\t0000000000001000" {
				t.Fatalf("netlink subprocess has unexpected capabilities: %q", capabilities)
			}
			if tc.fault == "matching-digest-status" {
				proof := containerShell(t, container, `test -s /tmp/l8k-digest-status.log;if grep -vE '^systemctl (is-active (systemd-networkd|NetworkManager)|is-system-running|show NetworkManager.service --property=LoadState,ActiveState,SubState)$' /tmp/l8k-manager-calls.log;then exit 1;fi;cat /tmp/l8k-digest-status.log`)
				t.Logf("unchanged real hash output rejected: %s", strings.TrimSpace(proof))
			}
			if tc.fault == "runtime-status" {
				t.Logf("real runtime output rejected after status1: %s", strings.TrimSpace(containerShell(t, container, "test -s /tmp/l8k-runtime-status.log;cat /tmp/l8k-runtime-status.log")))
			}
			if tc.fault == "shadow-generate" {
				containerShell(t, container, `if grep -vE '^systemctl (is-active (systemd-networkd|NetworkManager)|is-system-running|show NetworkManager.service --property=LoadState,ActiveState,SubState)$' /tmp/l8k-manager-calls.log;then exit 1;fi`)
			}
			before := containerShell(t, container, "cat /tmp/l8k-effects.log")
			if strings.Contains(before, "PROHIBITED") {
				t.Fatalf("prohibited command attempted: %s", before)
			}
			second, secondErr := session.configure(ctx, hostNetworkTestRequest())
			after := containerShell(t, container, "cat /tmp/l8k-effects.log")
			if before != after {
				t.Fatal("repeat replayed effects")
			}
			if tc.state == "complete" && (secondErr != nil || second.State != "complete") {
				t.Fatalf("exact repeat failed: %+v %v", second, secondErr)
			}
			if tc.state != "complete" && secondErr == nil {
				t.Fatal("incomplete prior intent did not block")
			}
		})
	}
	t.Run("foreign and partial intent refuse before network effects", func(t *testing.T) {
		for _, kind := range []string{"foreign-fragment", "partial-journal"} {
			resetHostNetworkContainer(t, container)
			key := hostNetworkKey(hostNetworkTestRequest().Identity)
			if kind == "foreign-fragment" {
				containerShell(t, container, `printf 'network: {version: 2}\n' > /etc/netplan/99-l8k-workload-`+key+`.yaml; chmod 600 /etc/netplan/99-l8k-workload-`+key+`.yaml`)
			} else {
				containerShell(t, container, `mkdir -m700 /var/lib/l8k-host-network; printf '{"owner":"l8k-host-network-v1","state":"intent"}\n' >/var/lib/l8k-host-network/`+key+`.json; chmod 600 /var/lib/l8k-host-network/`+key+`.json`)
			}
			s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, err := s.configure(ctx, hostNetworkTestRequest())
			cancel()
			if err == nil {
				t.Fatal("accepted foreign/incomplete ownership")
			}
			if containerShell(t, container, "cat /tmp/l8k-effects.log") != "" {
				t.Fatal("ownership failure mutated runtime")
			}
		}
	})
	t.Run("modified completed owned fragment is never overwritten", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container, recordTimings: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		r, err := s.configure(ctx, hostNetworkTestRequest())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		containerShell(t, container, "printf '# foreign modification\\n' >> "+r.FragmentPath)
		before := containerShell(t, container, "cat /tmp/l8k-effects.log")
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		negative, negativeErr := s.configure(ctx, hostNetworkTestRequest())
		cancel()
		if Code(negativeErr) != "HOST_NETWORK_OWNERSHIP_CONFLICT" || !negative.Blocking || negative.State == "complete" || negative.State == "verified-external" {
			t.Fatalf("modified fragment did not produce measured ownership refusal: %+v %v", negative, negativeErr)
		}
		if containerShell(t, container, "cat /tmp/l8k-effects.log") != before {
			t.Fatal("modified fragment triggered effects")
		}
	})
	t.Run("exact external declaration is verified without adoption", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, `cat >/etc/netplan/50-external.yaml <<'YAML'
network:
  ethernets:
    data0:
      mtu: 9000
      addresses: [172.25.0.2/24]
      routes: [{to: 172.25.0.0/16, via: 172.25.0.1}]
YAML
chmod 600 /etc/netplan/50-external.yaml
/usr/sbin/netplan.real generate >/dev/null
/usr/sbin/ip.real link set dev test0 mtu 9000
/usr/sbin/ip.real -4 address add 172.25.0.2/24 dev test0
/usr/sbin/ip.real -4 route add 172.25.0.0/16 via 172.25.0.1 dev test0`)
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		r, err := s.configure(ctx, hostNetworkTestRequest())
		if err != nil || !r.ExternalVerified || r.ChangedMTU || r.AddedAddress || r.AddedRoute {
			t.Fatalf("external declaration adopted: %+v %v", r, err)
		}
		if containerShell(t, container, "cat /tmp/l8k-effects.log") != "" || containerShell(t, container, "find /var/lib/l8k-host-network -name '*.json'") != "" {
			t.Fatal("external verification wrote an intent or changed runtime")
		}
	})
	t.Run("partial external state survives route failure", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, `cat >/etc/netplan/50-external.yaml <<'YAML'
network:
  ethernets:
    data0:
      mtu: 9000
      addresses: [172.25.0.2/24]
YAML
chmod 600 /etc/netplan/50-external.yaml
/usr/sbin/netplan.real generate >/dev/null
/usr/sbin/ip.real link set dev test0 mtu 9000
/usr/sbin/ip.real -4 address add 172.25.0.2/24 dev test0
touch /tmp/l8k-fault-route`)
		before := containerShell(t, container, "sha256sum /etc/netplan/50-external.yaml")
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container, recordTimings: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		r, err := s.configure(ctx, hostNetworkTestRequest())
		cancel()
		if err == nil || r.State != "rolled-back" || r.ChangedMTU || r.AddedAddress {
			t.Fatalf("external state rollback incorrect: %+v %v", r, err)
		}
		if containerShell(t, container, "sha256sum /etc/netplan/50-external.yaml") != before {
			t.Fatal("external Netplan declaration changed")
		}
		state := containerShell(t, container, `/usr/sbin/ip.real -j link show dev test0|jq '.[0].mtu';/usr/sbin/ip.real -j -4 address show dev test0|jq '[.[].addr_info[]]|length'`)
		if state != "9000\n1\n" {
			t.Fatalf("external runtime state removed: %q", state)
		}
	})
	t.Run("shadow refuses changes outside the selected generated pair", func(t *testing.T) {
		for _, tc := range []struct{ name, mutation string }{
			{"pending PF1 MTU", `sed -i '/    data1:/,$s/mtu: 1500/mtu: 1400/' /etc/netplan/10-base.yaml`},
			{"pending NM output", `printf '\n    auxiliary:\n      match: {name: aux0}\n      renderer: NetworkManager\n      dhcp4: true\n' >>/etc/netplan/10-base.yaml`},
			{"stale udev output", `printf '# external edit\n' >>/run/udev/rules.d/90-netplan.rules`},
			{"stale global unit", `printf '# external edit\n' >>/run/systemd/system/netplan-ovs-cleanup.service`},
			{"foreign output metadata", `chmod 600 /run/systemd/network/10-netplan-data1.network`},
			{"unknown Netplan unit", `printf '[Unit]\n' >/run/systemd/system/netplan-unsupported.service`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				containerShell(t, container, tc.mutation)
				before := allHostNetworkGeneratedState(t, container)
				s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				r, err := s.configure(ctx, hostNetworkTestRequest())
				if err == nil || r.ChangedMTU || r.AddedAddress || r.AddedRoute {
					t.Fatalf("unsafe generation accepted: %+v %v", r, err)
				}
				if after := allHostNetworkGeneratedState(t, container); after != before {
					t.Fatalf("live output changed on preview refusal: before=%s after=%s", before, after)
				}
				containerShell(t, container, `test ! -s /tmp/l8k-effects.log;if grep -vE '^systemctl (is-active (systemd-networkd|NetworkManager)|is-system-running|show NetworkManager.service --property=LoadState,ActiveState,SubState)$' /tmp/l8k-manager-calls.log;then exit 1;fi;test -z "$(find /etc/netplan -name '99-l8k-workload-*.yaml')";rm -f /run/systemd/system/netplan-unsupported.service`)
			})
		}
	})

	t.Run("missing filesystem privilege refuses invalid generated metadata", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		before := allHostNetworkGeneratedState(t, container)
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container, withoutChown: true}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		r, err := s.configure(ctx, hostNetworkTestRequest())
		if err == nil || r.FileVerified || r.RuntimeVerified || r.ChangedMTU || r.AddedAddress || r.AddedRoute || r.ReasonCode != "HOST_NETWORK_GENERATED_METADATA_UNSUPPORTED" {
			t.Fatalf("insufficient filesystem privilege accepted: %+v %v", r, err)
		}
		if allHostNetworkGeneratedState(t, container) != before {
			t.Fatal("metadata failure changed live generated outputs")
		}
		containerShell(t, container, `test ! -s /tmp/l8k-effects.log;if grep -vE '^systemctl (is-active (systemd-networkd|NetworkManager)|is-system-running|show NetworkManager.service --property=LoadState,ActiveState,SubState)$' /tmp/l8k-manager-calls.log;then exit 1;fi`)
	})

	t.Run("read-only preflight rejects unsafe source ownership and modes", func(t *testing.T) {
		for _, mutation := range []string{"chmod 666 /etc/netplan/10-base.yaml", "chmod 777 /etc/netplan", "chown 65534 /etc/netplan/10-base.yaml", "chown 65534 /etc/netplan"} {
			resetHostNetworkContainer(t, container)
			containerShell(t, container, mutation)
			s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, err := s.observe(ctx, hostNetworkTestIdentity())
			cancel()
			if err == nil || containerShell(t, container, "cat /tmp/l8k-effects.log") != "" {
				t.Fatal("unsafe source allowed effects")
			}
			containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test -z "$(find /etc/netplan -name '99-l8k-workload-*.yaml')";chown 0:0 /etc/netplan /etc/netplan/10-base.yaml;chmod 755 /etc/netplan;chmod 600 /etc/netplan/10-base.yaml`)
		}
	})
	t.Run("root-owned0644 source remains observable without a lease", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, "chmod 644 /etc/netplan/10-base.yaml")
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.observe(ctx, hostNetworkTestIdentity()); err != nil {
			t.Fatal(err)
		}
		containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
	})
	t.Run("snapshot reports one sanitized typed read failure", func(t *testing.T) {
		for _, tc := range []struct{ name, fault, code string }{
			{"identity command exit", "identity-exit-7", "HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7"},
			{"private matching context digest failure", "context-digest", "HOST_NETWORK_READ_GENERATOR_CONTEXT_SHA256SUM_EXIT_7"},
			{"tool disappears after availability check", "identity-exit-127", "HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_127"},
			{"PCI VPD producer fails", "lspci-exit-7", "HOST_NETWORK_READ_PCI_VPD_LSPCI_EXIT_7"},
			{"management route producer fails", "management-route-exit-7", "HOST_NETWORK_READ_MANAGEMENT_ROUTE_IP_EXIT_7"},
			{"management route consumer fails", "management-route-jq-exit-2", "HOST_NETWORK_READ_MANAGEMENT_ROUTE_JQ_EXIT_2"},
			{"unsupported ethtool query", "ethtool-unsupported", "HOST_NETWORK_READ_PERMANENT_MAC_ETHTOOL_EXIT_2"},
			{"unmanaged interface has no active network file", "network-file-missing", "HOST_NETWORK_READ_NETWORK_FILE_MISSING"},
			{"absent manager query denied", "nm-absent-show-fails", "HOST_NETWORK_READ_NETWORKMANAGER_PROPERTIES_SYSTEMCTL_EXIT_7"},
			{"absent manager status contradicts loaded unit", "nm-absent-loaded", "HOST_NETWORK_READ_NETWORKMANAGER_ABSENCE_UNCONFIRMED"},
			{"absent manager status has wrong response", "nm-absent-wrong-active", "HOST_NETWORK_READ_NETWORKMANAGER_STATE_SYSTEMCTL_EXIT_4"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				preservedBefore := preservedHostNetworkContainerState(t, container)
				generatedBefore := allHostNetworkGeneratedState(t, container)
				sourcesBefore := containerShell(t, container, "stat -c '%u:%g:%a:%h' /etc/netplan/10-base.yaml;sha256sum /etc/netplan/10-base.yaml")
				containerShell(t, container, "touch /tmp/l8k-fault-"+tc.fault)
				remote := &containerHostNetworkRemote{t: t, container: container}
				s := hostNetworkSession{remote: remote}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_, err := s.observe(ctx, hostNetworkTestIdentity())
				if Code(err) != tc.code {
					t.Fatalf("got %q, want %q (%v), response=%q", Code(err), tc.code, err, remote.lastBackendOutput)
				}
				var response map[string]string
				if err := json.Unmarshal([]byte(remote.lastBackendOutput), &response); err != nil || len(response) != 1 || response["error"] != tc.code {
					t.Fatalf("backend did not return exactly one typed JSON response: %q (%v)", remote.lastBackendOutput, err)
				}
				if strings.Contains(remote.lastBackendOutput, "123e4567") || strings.Contains(remote.lastBackendOutput, "Netplan") {
					t.Fatalf("backend leaked private observation data: %q", remote.lastBackendOutput)
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log;test ! -s /tmp/l8k-reads-after-failure.log;if grep -vE '^systemctl (is-active (systemd-networkd|NetworkManager)|is-system-running|show NetworkManager.service --property=LoadState,ActiveState,SubState)$' /tmp/l8k-manager-calls.log;then exit 1;fi`)
				if preservedBefore != preservedHostNetworkContainerState(t, container) || generatedBefore != allHostNetworkGeneratedState(t, container) {
					t.Fatal("failed observation changed preserved runtime or generated files")
				}
				if sourcesBefore != containerShell(t, container, "stat -c '%u:%g:%a:%h' /etc/netplan/10-base.yaml;sha256sum /etc/netplan/10-base.yaml") {
					t.Fatal("failed observation changed Netplan sources")
				}
			})
		}
	})
	t.Run("matching context failure in configure refuses with precise cause before intent", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		remote := &containerHostNetworkRemote{t: t, container: container, failContextDigestOnConfigure: true}
		session := hostNetworkSession{remote: remote}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		receipt, err := session.configure(ctx, hostNetworkTestRequest())
		if Code(err) != "HOST_NETWORK_READ_GENERATOR_CONTEXT_SHA256SUM_EXIT_7" || receipt.State != "refused" || receipt.ReasonCode != Code(err) || receipt.Blocking {
			t.Fatalf("context failure escaped configure preflight: %+v %v", receipt, err)
		}
		containerShell(t, container, `test ! -s /tmp/l8k-effects.log;test -z "$(find /var/lib/l8k-host-network -name '*.json')";test -z "$(find /etc/netplan -name '99-l8k-workload-*.yaml')"`)
	})
	t.Run("snapshot binds merged configuration to stable protected sources", func(t *testing.T) {
		for _, fault := range []string{"source-after-merged", "source-late-optional", "source-hash-exit-1", "source-hash-exit-2", "source-hash-exit-3"} {
			t.Run(fault, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				containerShell(t, container, `sed -i '/      set-name: test0/a\      dhcp4: false\n      optional: false' /etc/netplan/10-base.yaml;rm -f /tmp/l8k-source-hash-count /tmp/l8k-source-mutated`)
				before := preservedHostNetworkContainerState(t, container)
				containerShell(t, container, "touch /tmp/l8k-fault-"+fault)
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				payload := hostNetworkBackendRequest{Request: HostNetworkRequest{Identity: hostNetworkTestIdentity()}, Key: hostNetworkKey(hostNetworkTestIdentity()), Connection: "172.17.0.1 40000 172.17.0.10 22"}
				data, err := session.run(context.Background(), "snapshot", payload)
				var result map[string]string
				want := "HOST_NETWORK_SOURCE_CHANGED"
				if strings.HasPrefix(fault, "source-hash-exit-") {
					want = "HOST_NETWORK_READ_NETPLAN_SOURCE_SOURCE_DIGEST_EXIT_7"
				}
				if err != nil || decodeNetworkJSON(data, &result) != nil || len(result) != 1 || result["error"] != want {
					t.Fatalf("inconsistent snapshot accepted or cause lost: %+v %v want %s", result, err, want)
				}
				if strings.HasPrefix(fault, "source-hash-exit-") {
					count := strings.TrimSpace(containerShell(t, container, "cat /tmp/l8k-source-hash-count"))
					if count != strings.TrimPrefix(fault, "source-hash-exit-") {
						t.Fatalf("wrong bracket producer failed: %s", count)
					}
				} else {
					containerShell(t, container, "test -f /tmp/l8k-source-mutated")
				}
				if before != preservedHostNetworkContainerState(t, container) {
					t.Fatal("failed snapshot changed runtime/generated artifacts")
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
			})
		}
	})
	t.Run("owned fragment changes inside reads cannot verify early success", func(t *testing.T) {
		for _, mode := range []string{"repeat-capture", "external-capture", "repeat-getter"} {
			t.Run(mode, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				if mode == "external-capture" {
					containerShell(t, container, `cat >/etc/netplan/50-external.yaml <<'YAML'
network:
  ethernets:
    data0:
      mtu: 9000
      addresses: [172.25.0.2/24]
      routes: [{to: 172.25.0.0/16, via: 172.25.0.1}]
YAML
chmod 600 /etc/netplan/50-external.yaml
/usr/sbin/netplan.real generate >/dev/null
/usr/sbin/ip.real link set dev test0 mtu 9000
/usr/sbin/ip.real -4 address add 172.25.0.2/24 dev test0
/usr/sbin/ip.real -4 route add 172.25.0.0/16 via 172.25.0.1 dev test0`)
				} else if _, err := session.configure(ctx, hostNetworkTestRequest()); err != nil {
					t.Fatal(err)
				}
				beforeEffects := containerShell(t, container, "cat /tmp/l8k-effects.log")
				beforeGenerated := allHostNetworkGeneratedState(t, container)
				fault := "owned-fragment-capture"
				if mode == "repeat-getter" {
					fault = "owned-fragment-getter"
				}
				containerShell(t, container, "rm -f /tmp/l8k-owned-get-count /tmp/l8k-owned-mutated;touch /tmp/l8k-fault-"+fault)
				receipt, err := session.configure(ctx, hostNetworkTestRequest())
				if Code(err) != "HOST_NETWORK_OWNERSHIP_CONFLICT" || receipt.State == "complete" || receipt.State == "verified-external" {
					t.Errorf("changed/appeared owned fragment falsely verified: %+v %v", receipt, err)
				}
				containerShell(t, container, `test -f /tmp/l8k-owned-mutated;test "$(cat /tmp/l8k-owned-dhcp-proof)" = true;/usr/sbin/netplan.real get network.ethernets.data0.dhcp4 | grep -Fx true`)
				if beforeEffects != containerShell(t, container, "cat /tmp/l8k-effects.log") {
					t.Fatal("ownership read failure replayed effects")
				}
				if beforeGenerated != allHostNetworkGeneratedState(t, container) {
					t.Fatal("unsafe DHCP source was generated")
				}
			})
		}
	})
	t.Run("configure source capture refuses a concurrent rewrite before intent", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, `sed -i '/      set-name: test0/a\      dhcp4: false' /etc/netplan/10-base.yaml;rm -f /tmp/l8k-source-configure-get-count /tmp/l8k-source-mutated;touch /tmp/l8k-fault-source-configure-get`)
		before := allHostNetworkGeneratedState(t, container)
		session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		receipt, err := session.configure(ctx, hostNetworkTestRequest())
		if Code(err) != "HOST_NETWORK_SOURCE_CHANGED" || receipt.State != "refused" {
			t.Fatalf("configure accepted changed source: %+v %v", receipt, err)
		}
		containerShell(t, container, `test -f /tmp/l8k-source-mutated;test ! -s /tmp/l8k-effects.log;test -z "$(find /var/lib/l8k-host-network -name '*.json' -print -quit)";test -z "$(find /etc/netplan -name '99-l8k-workload-*.yaml' -print -quit)"`)
		if before != allHostNetworkGeneratedState(t, container) {
			t.Fatal("source capture refusal generated host files")
		}
	})
	t.Run("normal completion refences sources and absent owned fragment after getters", func(t *testing.T) {
		for _, mode := range []string{"source", "absent-owned"} {
			t.Run(mode, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				if mode == "source" {
					containerShell(t, container, `sed -i '/      set-name: test0/a\      dhcp4: false' /etc/netplan/10-base.yaml`)
				} else {
					containerShell(t, container, `cat >/etc/netplan/50-external.yaml <<'YAML'
network:
  ethernets:
    data0:
      mtu: 9000
      addresses: [172.25.0.2/24]
      routes: [{to: 172.25.0.0/16, via: 172.25.0.1}]
YAML
chmod 600 /etc/netplan/50-external.yaml
/usr/sbin/netplan.real generate >/dev/null`)
				}
				containerShell(t, container, "rm -f /tmp/l8k-normal-mutated;touch /tmp/l8k-fault-normal-final-"+mode)
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				receipt, err := session.configure(ctx, hostNetworkTestRequest())
				if Code(err) != "HOST_NETWORK_COMPENSATION_UNCERTAIN" || receipt.State != "partial" || !receipt.Blocking {
					t.Errorf("normal completion published after source/ownership drift: %+v %v", receipt, err)
				}
				containerShell(t, container, `test -f /tmp/l8k-normal-mutated;test "$(cat /tmp/l8k-owned-dhcp-proof)" = true;/usr/sbin/netplan.real get network.ethernets.data0.dhcp4 | grep -Fx true;if grep PROHIBITED /tmp/l8k-effects.log;then exit 1;fi`)
			})
		}
	})
	t.Run("private optional field rejects missing and wrong types before intent", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			present bool
			value   any
		}{{name: "missing", present: false, value: nil}, {name: "string false", present: true, value: "false"}, {name: "null", present: true, value: nil}, {name: "number", present: true, value: 0}} {
			t.Run(tc.name, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				request := hostNetworkTestRequest()
				payload := map[string]any{"request": request, "requestID": networkDigest(request), "key": hostNetworkKey(request.Identity), "connection": "192.0.2.10 40000 192.0.2.20 22"}
				if tc.present {
					payload["waitOnlineOptional"] = tc.value
				}
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				data, err := session.run(ctx, "configure", payload)
				if err != nil {
					t.Fatal(err)
				}
				var receipt HostNetworkReceipt
				if decodeNetworkJSON(data, &receipt) != nil || receipt.State != "refused" || receipt.ReasonCode != "HOST_NETWORK_INPUT_INVALID" {
					t.Fatalf("bad private field accepted: %s", data)
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
			})
		}
	})
	t.Run("repeat and external verification reject invalid owned contribution", func(t *testing.T) {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if external {
					containerShell(t, container, `cat >/etc/netplan/50-external.yaml <<'YAML'
network:
  ethernets:
    data0:
      mtu: 9000
      addresses: [172.25.0.2/24]
      routes: [{to: 172.25.0.0/16, via: 172.25.0.1}]
YAML
chmod 600 /etc/netplan/50-external.yaml
/usr/sbin/netplan.real generate >/dev/null
/usr/sbin/ip.real link set dev test0 mtu 9000
/usr/sbin/ip.real -4 address add 172.25.0.2/24 dev test0
/usr/sbin/ip.real -4 route add 172.25.0.0/16 via 172.25.0.1 dev test0`)
				} else if _, err := session.configure(ctx, hostNetworkTestRequest()); err != nil {
					t.Fatal(err)
				}
				for _, mutation := range []string{"remove-selected", "trailing-reset", "wrong-section", "writable-parent", "symlink-parent"} {
					t.Run(mutation, func(t *testing.T) {
						// Retain and restore the exact qualified artifact/parent around each
						// tamper so every early-success negative starts from the same state.
						containerShell(t, container, `python3 - '`+mutation+`' <<'PY'
from pathlib import Path
import sys
parent=Path("/run/systemd/system/systemd-networkd-wait-online.service.d")
p=parent/"10-netplan.conf"
backup=Path("/tmp/l8k-wait-owned-original")
backup.write_bytes(p.read_bytes())
mode=sys.argv[1]
if mode=="remove-selected":
    prefix="ExecStart=/lib/systemd/systemd-networkd-wait-online --any -o routable"
    p.write_text("".join(line.replace(" -i test0", "") if line.startswith(prefix) else line for line in p.read_text().splitlines(keepends=True)))
elif mode=="trailing-reset": p.write_bytes(p.read_bytes()+b"ExecStart=\n")
elif mode=="wrong-section": p.write_bytes(p.read_bytes().replace(b"[Service]", b"[Unit]"))
elif mode=="writable-parent": parent.chmod(0o777)
elif mode=="symlink-parent":
    parent.rename("/tmp/l8k-wait-owned-parent")
    parent.symlink_to("/tmp/l8k-wait-owned-parent", target_is_directory=True)
PY`)
						defer containerShell(t, container, `python3 - <<'PY'
from pathlib import Path
parent=Path("/run/systemd/system/systemd-networkd-wait-online.service.d")
if parent.is_symlink():
    parent.unlink()
    Path("/tmp/l8k-wait-owned-parent").rename(parent)
parent.chmod(0o755)
backup=Path("/tmp/l8k-wait-owned-original")
(parent/"10-netplan.conf").write_bytes(backup.read_bytes())
backup.unlink()
PY`)
						before := containerShell(t, container, "cat /tmp/l8k-effects.log")
						receipt, err := session.configure(ctx, hostNetworkTestRequest())
						wantCode := "HOST_NETWORK_WAIT_ONLINE_CONTRIBUTION_INVALID"
						if mutation == "writable-parent" || mutation == "symlink-parent" || mutation == "trailing-reset" || mutation == "wrong-section" {
							wantCode = "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED"
						}
						if Code(err) != wantCode || receipt.State == "complete" || receipt.State == "verified-external" {
							t.Fatalf("owned contribution not verified: %+v %v; want %s", receipt, err, wantCode)
						}
						if before != containerShell(t, container, "cat /tmp/l8k-effects.log") {
							t.Fatal("verification failure replayed effects")
						}
					})
				}
			})
		}
	})
	t.Run("snapshot qualifies private current MAC matching without generation", func(t *testing.T) {
		for _, mode := range []string{"direct", "absent", "other-only", "other-only-optional"} {
			t.Run(mode, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				if mode != "direct" {
					containerShell(t, container, `/usr/sbin/ip.real link set dev test0 address 02:00:00:00:00:03`)
				}
				if strings.HasPrefix(mode, "other-only") {
					containerShell(t, container, `/usr/sbin/ip.real link set dev test1 address 02:00:00:00:00:01`)
				}
				if mode == "other-only-optional" {
					containerShell(t, container, `sed -i '/      set-name: test0/a\      optional: true' /etc/netplan/10-base.yaml`)
				}
				before := containerShell(t, container, `find /run/systemd/network /run/udev/rules.d /run/systemd/system/systemd-networkd-wait-online.service.d -type f -exec sha256sum {} + | sort`)
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				observation, err := session.observe(context.Background(), hostNetworkTestIdentity())
				if mode == "other-only" {
					if Code(err) != "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED" {
						t.Fatalf("matching readiness reason lost: %v", err)
					}
				} else {
					if err != nil || observation.raw.WaitOnlineOtherMatchOnly == nil || *observation.raw.WaitOnlineOtherMatchOnly != (mode == "other-only-optional") {
						t.Fatalf("matching fact: %+v %v", observation.raw.WaitOnlineOtherMatchOnly, err)
					}
					encoded, err := json.Marshal(observation)
					if err != nil || strings.Contains(string(encoded), "waitOnline") {
						t.Fatalf("private matching fact leaked: %s %v", encoded, err)
					}
				}
				if before != containerShell(t, container, `find /run/systemd/network /run/udev/rules.d /run/systemd/system/systemd-networkd-wait-online.service.d -type f -exec sha256sum {} + | sort`) {
					t.Fatal("read-only matching check generated files")
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
			})
		}
	})
	t.Run("snapshot rejects unsupported wait-online before deployment", func(t *testing.T) {
		for _, mutation := range []string{"missing", "trailing-reset", "wrong-section", "writable-parent"} {
			t.Run(mutation, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				containerShell(t, container, `python3 - '`+mutation+`' <<'PY'
from pathlib import Path
import sys
parent=Path("/run/systemd/system/systemd-networkd-wait-online.service.d")
p=parent/"10-netplan.conf"
mode=sys.argv[1]
if mode=="missing": p.unlink()
elif mode=="trailing-reset": p.write_bytes(p.read_bytes()+b"ExecStart=\n")
elif mode=="wrong-section": p.write_bytes(p.read_bytes().replace(b"[Service]", b"[Unit]"))
elif mode=="writable-parent": parent.chmod(0o777)
PY`)
				defer containerShell(t, container, "chmod 755 /run/systemd/system/systemd-networkd-wait-online.service.d")
				session := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				payload := hostNetworkBackendRequest{Request: HostNetworkRequest{Identity: hostNetworkTestIdentity()}, Key: hostNetworkKey(hostNetworkTestIdentity()), Connection: "172.17.0.1 40000 172.17.0.10 22"}
				data, err := session.run(context.Background(), "snapshot", payload)
				var result map[string]string
				if err != nil || decodeNetworkJSON(data, &result) != nil || len(result) != 1 || result["error"] != "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED" {
					t.Fatalf("snapshot did not return one finite blocked JSON: %+v %v", result, err)
				}
				if _, err := session.observe(context.Background(), hostNetworkTestIdentity()); Code(err) != "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED" {
					t.Fatalf("observer lost discovery failure: %v", err)
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
			})
		}
	})
	t.Run("missing tool refuses before snapshot reads", func(t *testing.T) {
		for _, tool := range []struct{ name, path string }{{"ETHTOOL", "/usr/sbin/ethtool"}, {"OD", "/usr/bin/od"}} {
			t.Run(tool.name, func(t *testing.T) {
				resetHostNetworkContainer(t, container)
				containerShell(t, container, "mv "+tool.path+" "+tool.path+".hidden")
				defer containerShell(t, container, "mv "+tool.path+".hidden "+tool.path)
				s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
				_, err := s.observe(context.Background(), hostNetworkTestIdentity())
				if Code(err) != "HOST_NETWORK_TOOL_MISSING_"+tool.name {
					t.Fatalf("missing tool: %v", err)
				}
				containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log;test ! -s /tmp/l8k-snapshot-reads.log`)
			})
		}
	})
	t.Run("stale active definition rejects before any publication", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, "touch /tmp/l8k-fault-active-definition")
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.observe(ctx, hostNetworkTestIdentity()); Code(err) != "HOST_NETWORK_BACKEND_OWNERSHIP_UNAVAILABLE" {
			t.Fatalf("active definition mismatch accepted: %v", err)
		}
		containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
	})
	t.Run("actual selected IPv6 default rejects while other-link default survives", func(t *testing.T) {
		resetHostNetworkContainer(t, container)
		containerShell(t, container, "/usr/sbin/ip.real -6 route add default dev test0 metric 100")
		s := hostNetworkSession{remote: &containerHostNetworkRemote{t: t, container: container}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := s.observe(ctx, hostNetworkTestIdentity()); Code(err) != "HOST_NETWORK_MANAGEMENT_PATH_CONFLICT" {
			t.Fatalf("selected IPv6 default accepted: %v", err)
		}
		containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log;/usr/sbin/ip.real -6 route del default dev test0 metric 100;/usr/sbin/ip.real -6 route add default dev test1 metric 100`)
		defer containerShell(t, container, "/usr/sbin/ip.real -6 route del default dev test1 metric 100")
		if _, err := s.observe(ctx, hostNetworkTestIdentity()); err != nil {
			t.Fatalf("unrelated IPv6 default refused: %v", err)
		}
		containerShell(t, container, `test ! -e /var/lib/l8k-host-network;test ! -s /tmp/l8k-effects.log`)
	})
}

type containerHostNetworkRemote struct {
	t                            *testing.T
	container                    string
	withoutChown                 bool // Negative filesystem-privilege qualification only.
	hostileEnvironment           bool
	lastBackendOutput            string
	failContextDigestOnConfigure bool
	recordTimings                bool
}

func (r *containerHostNetworkRemote) Run(ctx context.Context, stage, command string) (string, error) {
	if stage == "host-network-ssh-path" {
		return containerShell(r.t, r.container, `gateway=$(/usr/sbin/ip.real -j route show default|jq -r '.[0].gateway'); local=$(/usr/sbin/ip.real -j -4 addr show dev eth0|jq -r '.[0].addr_info[0].local'); printf '%s 40000 %s 22' "$gateway" "$local"`), nil
	}
	if stage == "host-network-configure" && r.failContextDigestOnConfigure {
		containerShell(r.t, r.container, "touch /tmp/l8k-fault-context-digest")
	}
	// Execute the exact command constructed by the native-SSH coordinator,
	// including its shell quoting; do not substitute a test backend action.
	if r.hostileEnvironment {
		command = "export NETPLAN_GENERATE_PATH=/bin/false SNAP=unexpected PYTHONPATH=/untrusted BASH_ENV=/untrusted\n" + command
	}
	args := []string{"exec", "-i", r.container}
	if r.withoutChown {
		args = append(args, "setpriv", "--bounding-set=-chown", "--inh-caps=-all", "--ambient-caps=-all")
	}
	args = append(args, "/bin/bash", "-c", "exec 2>/tmp/host-network-transaction-backend.stderr\n"+command)
	cmd := exec.CommandContext(ctx, "docker", args...)
	// The SSH transport returns stdout separately from its bounded stderr.
	// Preserve that protocol boundary while retaining container diagnostics.
	started := time.Now()
	output, err := cmd.CombinedOutput()
	if r.recordTimings {
		r.t.Logf("backend %s elapsed including isolated Docker transport: %s", stage, time.Since(started))
	}
	if err != nil {
		return "", fmt.Errorf("isolated backend: %w: %s", err, output)
	}
	r.lastBackendOutput = string(output)
	return string(output), nil
}
func (r *containerHostNetworkRemote) RunWithInput(ctx context.Context, stage, command string, _ []byte) (string, error) {
	return r.Run(ctx, stage, command)
}

func (r *containerHostNetworkRemote) RunWithInputTimeout(ctx context.Context, stage, command string, input []byte, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.RunWithInput(ctx, stage, command, input)
}

func containerCommand(t *testing.T, container, action string) string {
	t.Helper()
	cmd := exec.Command("docker", action, container)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v %s", action, err, output)
	}
	if action == "inspect" {
		var objects []map[string]any
		if json.Unmarshal(output, &objects) != nil || len(objects) != 1 {
			t.Fatal("invalid inspect")
		}
		minimal, _ := json.Marshal(map[string]any{"Privileged": objects[0]["HostConfig"].(map[string]any)["Privileged"], "NetworkMode": objects[0]["HostConfig"].(map[string]any)["NetworkMode"], "CapAdd": objects[0]["HostConfig"].(map[string]any)["CapAdd"], "Mounts": objects[0]["Mounts"]})
		return string(minimal)
	}
	return string(output)
}
func containerShell(t *testing.T, container, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "/bin/bash", "-s")
	cmd.Stdin = strings.NewReader("set -Eeuo pipefail\n" + script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated setup/assertion: %v %s", err, output)
	}
	return string(output)
}

func setupHostNetworkContainer(t *testing.T, container string) {
	containerShell(t, container, strings.ReplaceAll(hostNetworkContainerStubs, "__L8K_FRAGMENT_KEY__", hostNetworkKey(hostNetworkTestIdentity())))
	proof := exec.Command("docker", "exec", container, "setpriv", "--bounding-set=-all,+net_admin", "--inh-caps=-all", "--ambient-caps=-all", "/bin/bash", "-c", `grep '^CapEff:\|^CapPrm:\|^CapBnd:' /proc/self/status`)
	data, err := proof.CombinedOutput()
	if err != nil || strings.Count(string(data), "0000000000001000") != 3 {
		t.Fatalf("netlink-only NET_ADMIN capability proof failed: %s %v", data, err)
	}
}

func preservedHostNetworkContainerState(t *testing.T, container string) string {
	state := containerShell(t, container, `/usr/sbin/ip.real -j -4 route show default table all
/usr/sbin/ip.real -j -6 route show default table all
/usr/sbin/ip.real -j -4 rule show
/usr/sbin/ip.real -j -6 rule show
/usr/sbin/ip.real -j address show dev eth0 | jq -cS '[.[]|{ifname,mtu,addr_info}]'
/usr/sbin/ip.real -j link show dev test1 | jq -cS '[.[]|{ifname,mtu,flags}]'
/usr/sbin/ip.real -j -4 address show dev test1
stat -c '%n %u:%g:%a' /run/systemd/network/10-netplan-management.network /run/systemd/network/10-netplan-data1.network /run/systemd/network/10-netplan-data1.link /run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf
sha256sum /run/systemd/network/10-netplan-management.network /run/systemd/network/10-netplan-data1.network /run/systemd/network/10-netplan-data1.link`)
	unit := containerShell(t, container, "cat /run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf")
	return state + fmt.Sprintf("\nwait-online nonselected bytes: %x", sha256.Sum256(independentWaitOnlineProjection([]byte(unit), "test0")))
}

// Compare every qualified output byte and its owner/group/mode/path, including
// symlink targets. This independent test assertion also covers deletions.
func allHostNetworkGeneratedState(t *testing.T, container string) string {
	t.Helper()
	return containerShell(t, container, `for root in /run/systemd/network /run/NetworkManager/conf.d /run/NetworkManager/system-connections /run/udev/rules.d /run/systemd/system/systemd-networkd-wait-online.service.d; do
 test ! -d "$root" || find "$root" -mindepth 1 -maxdepth 1 -printf '%p %y %U:%G:%m %l\n' -exec sha256sum {} \;
done
for file in /run/systemd/system/netplan-*.service /run/systemd/system/systemd-networkd.service.wants/netplan-*.service; do
 if test -L "$file"; then stat -c '%n %u:%g:%a %N' "$file";elif test -f "$file";then stat -c '%n %u:%g:%a' "$file";sha256sum "$file";fi
done`)
}

func resetHostNetworkContainer(t *testing.T, container string) {
	containerShell(t, container, `rm -f /tmp/l8k-fault-* /etc/netplan/*.yaml /run/systemd/system/netplan-unsupported.service; rm -rf /var/lib/l8k-host-network
/usr/sbin/ip.real -4 route del 172.25.0.0/16 dev test0 2>/dev/null || true
/usr/sbin/ip.real -4 addr flush dev test0
/usr/sbin/ip.real link set dev test0 mtu 1500
/usr/sbin/ip.real link set dev test0 down
/usr/sbin/ip.real link set dev test0 address 02:00:00:00:00:01
/usr/sbin/ip.real link set dev test0 up
/usr/sbin/ip.real link set dev test1 down
/usr/sbin/ip.real link set dev test1 address 02:00:00:00:00:02
/usr/sbin/ip.real link set dev test1 up
cat >/etc/netplan/10-base.yaml <<'YAML'
network:
  version: 2
  renderer: networkd
  ethernets:
    data0:
      match: {macaddress: '02:00:00:00:00:01'}
      set-name: test0
      mtu: 1500
    management:
      match: {name: eth0}
      dhcp4: true
    data1:
      match: {macaddress: '02:00:00:00:00:02'}
      set-name: test1
      mtu: 1500
YAML
chmod 600 /etc/netplan/10-base.yaml
/usr/sbin/netplan.real generate >/dev/null
: >/tmp/l8k-effects.log
: >/tmp/l8k-netlink-capabilities.log
: >/tmp/l8k-manager-calls.log
: >/tmp/l8k-digest-status.log
: >/tmp/l8k-runtime-status.log
: >/tmp/l8k-snapshot-reads.log
: >/tmp/l8k-reads-after-failure.log`)
}

const hostNetworkContainerStubs = `
# Explicit physical identity and service-manager test doubles, confined to this
# disposable container. Production has no bypass flag or injected executable.
for tool in ip netplan; do if [ ! -e /usr/sbin/$tool.real ]; then mv /usr/sbin/$tool /usr/sbin/$tool.real; fi; done
for tool in cat readlink networkctl sync udevadm mktemp sha256sum jq; do if [ ! -e /usr/bin/$tool.real ]; then mv /usr/bin/$tool /usr/bin/$tool.real; fi; done
# Inject native-preview failure explicitly; successful calls execute the preserved
# installed official binary unchanged. No production bypass exists.
if [ ! -e /usr/libexec/netplan/generate.real ]; then mv /usr/libexec/netplan/generate /usr/libexec/netplan/generate.real; fi
/usr/bin/cat.real >/usr/libexec/netplan/generate <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-shadow-generate && "$1" == --root-dir ]]; then exit 2; fi
exec /usr/libexec/netplan/generate.real "$@"
SH
chmod 755 /usr/libexec/netplan/generate
/usr/bin/cat.real >/usr/sbin/dmidecode <<'SH'
#!/bin/bash
printf 'identity\n' >>/tmp/l8k-snapshot-reads.log
if [[ -f /tmp/l8k-fault-read-failed ]]; then printf 'identity\n' >>/tmp/l8k-reads-after-failure.log; fi
if [[ -f /tmp/l8k-fault-identity-exit-7 ]]; then touch /tmp/l8k-fault-read-failed; exit 7; fi
if [[ -f /tmp/l8k-fault-identity-exit-127 ]]; then touch /tmp/l8k-fault-read-failed; exit 127; fi
if [[ -f /tmp/l8k-fault-identity-readback-persistent ]] && jq -e 'select(.addedAddress)' /var/lib/l8k-host-network/*.json >/dev/null 2>&1; then exit 7; fi
if [[ -f /tmp/l8k-fault-identity-readback ]] && jq -e 'select(.addedAddress)' /var/lib/l8k-host-network/*.json >/dev/null 2>&1; then rm /tmp/l8k-fault-identity-readback; exit 7; fi
printf '123e4567-e89b-12d3-a456-426614174000\n'
SH
/usr/bin/cat.real >/usr/sbin/lspci <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-read-failed ]]; then printf 'lspci\n' >>/tmp/l8k-reads-after-failure.log; fi
if [[ -f /tmp/l8k-fault-lspci-exit-7 ]]; then touch /tmp/l8k-fault-read-failed; exit 7; fi
printf '[SN] Serial number: CARD-A\n'
SH
/usr/bin/cat.real >/usr/sbin/ethtool <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-read-failed ]]; then printf 'ethtool\n' >>/tmp/l8k-reads-after-failure.log; fi
if [[ -f /tmp/l8k-fault-ethtool-unsupported ]]; then touch /tmp/l8k-fault-read-failed; printf 'unsupported option\n' >&2; exit 2; fi
[[ "$1" == -P && "$2" == test0 ]] || exit 2
printf 'Permanent address: 02:00:00:00:00:01\n'
SH
/usr/bin/cat.real >/usr/bin/jq <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-management-route-jq-exit-2 && "$*" == '-c map(del(.uid,.cache))' ]]; then touch /tmp/l8k-fault-read-failed; exit 2; fi
exec /usr/bin/jq.real "$@"
SH
/usr/bin/cat.real >/usr/bin/readlink <<'SH'
#!/bin/bash
if [[ "${*: -1}" == /sys/class/net/test0/device ]]; then printf '/sys/devices/pci0000:20/0000:21:00.0\n'; else exec /usr/bin/readlink.real "$@"; fi
SH
/usr/bin/cat.real >/usr/bin/cat <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-read-failed && "$*" == /proc/sys/kernel/random/boot_id ]]; then printf 'boot\n' >>/tmp/l8k-reads-after-failure.log; fi
if [[ "$*" == /sys/class/net/test0/phys_port_name ]]; then printf 'p0\n'; else exec /usr/bin/cat.real "$@"; fi
SH
/usr/bin/cat.real >/usr/bin/systemctl <<'SH'
#!/bin/bash
printf 'systemctl %s\n' "$*" >>/tmp/l8k-manager-calls.log
if [[ "$*" == 'is-active systemd-networkd' ]]; then printf 'active\n'; exit 0; fi
if [[ "$*" == 'is-active NetworkManager' ]]; then
 if [[ -f /tmp/l8k-fault-source-late-optional ]]; then
  sed -i 's/      optional: false/      optional: true/' /etc/netplan/10-base.yaml
  touch /tmp/l8k-source-mutated
  rm /tmp/l8k-fault-source-late-optional
 fi
 if compgen -G '/tmp/l8k-fault-nm-absent*' >/dev/null; then
  if [[ -f /tmp/l8k-fault-nm-absent-wrong-active ]]; then printf 'unknown\n'; else printf 'inactive\n'; fi
  exit 4
 fi
 printf 'inactive\n'; exit 3
fi
if [[ "$*" == 'show NetworkManager.service --property=LoadState,ActiveState,SubState' ]]; then
 if [[ -f /tmp/l8k-fault-nm-absent-show-fails ]]; then exit 7; fi
 if [[ -f /tmp/l8k-fault-nm-absent-loaded ]]; then printf 'LoadState=loaded\nActiveState=inactive\nSubState=dead\n'; else printf 'LoadState=not-found\nActiveState=inactive\nSubState=dead\n'; fi
 exit 0
fi
if [[ "$1" == is-system-running ]]; then printf 'offline\n'; exit 1; fi
if [[ "$1" == daemon-reload ]]; then exit 0; fi
printf 'PROHIBITED systemctl %s\n' "$*" >>/tmp/l8k-effects.log
exit 2
SH
/usr/bin/cat.real >/usr/bin/udevadm <<'SH'
#!/bin/bash
printf 'udevadm %s\n' "$*" >>/tmp/l8k-manager-calls.log
if [[ "$*" == 'control --reload' ]]; then exit 0; fi
printf 'PROHIBITED udevadm %s\n' "$*" >>/tmp/l8k-effects.log
exit 99
SH
# Preserve the real hash bytes, but inject a nonzero status at the first
# post-preview artifact fence. An output match must never discard this failure.
/usr/bin/cat.real >/usr/bin/sha256sum <<'SH'
#!/bin/bash
if [[ $# == 0 && -f /tmp/l8k-fault-context-digest ]]; then
 input=$(/usr/bin/cat.real) || exit
 if [[ "$input" == *$'\t0\t-'* || "$input" == *$'\t1\t'* ]]; then exit 7; fi
 printf '%s\n' "$input" | /usr/bin/sha256sum.real
 exit "${PIPESTATUS[1]}"
fi
if [[ $# == 0 ]] && compgen -G '/tmp/l8k-fault-source-hash-exit-*' >/dev/null; then
 source_input=$(/usr/bin/mktemp.real /tmp/l8k-source-hash-input-XXXXXX) || exit
 trap 'rm -f -- "$source_input"' EXIT
 /usr/bin/cat.real >"$source_input" || exit
 if grep -Eq '^[0-9a-f]{64}  /etc/netplan/10-base.yaml$' "$source_input"; then
  source_count=0
  [[ ! -f /tmp/l8k-source-hash-count ]] || read -r source_count </tmp/l8k-source-hash-count
  ((source_count+=1));printf '%s\n' "$source_count" >/tmp/l8k-source-hash-count
  if [[ -f /tmp/l8k-fault-source-hash-exit-$source_count ]]; then /usr/bin/sha256sum.real <"$source_input";exit 7;fi
 fi
 /usr/bin/sha256sum.real <"$source_input"
 exit $?
fi
if [[ $# == 0 && -f /tmp/l8k-fault-matching-digest-status ]] && jq -e 'select(.phase=="shadow" and .shadow.path=="" and .shadow.plannedArtifacts!="")' /var/lib/l8k-host-network/*.json >/dev/null 2>&1; then
 digest=$(/usr/bin/sha256sum.real "$@") || exit
 printf '%s\n' "$digest"
 printf '%s status=1\n' "$digest" >>/tmp/l8k-digest-status.log
 rm /tmp/l8k-fault-matching-digest-status
 exit 1
fi
exec /usr/bin/sha256sum.real "$@"
SH
/usr/bin/cat.real >/usr/bin/mktemp <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-mktemp-fragment && "$*" == /etc/netplan/.l8k-workload-* ]]; then exit 2; fi
exec /usr/bin/mktemp.real "$@"
SH
/usr/bin/cat.real >/usr/bin/sync <<'SH'
#!/bin/bash
if [[ -f /tmp/l8k-fault-sync-journal-file && "${*: -1}" == /var/lib/l8k-host-network/.intent-* ]]; then exit 1; fi
if [[ -f /tmp/l8k-fault-sync-journal-directory && "${*: -1}" == /var/lib/l8k-host-network ]]; then exit 1; fi
if [[ -f /tmp/l8k-fault-sync-fragment && "${*: -1}" == /etc/netplan/.l8k-workload-* ]]; then exit 1; fi
/usr/bin/sync.real "$@" || exit
if [[ -f /tmp/l8k-fault-interrupt-intent && "${*: -1}" == /var/lib/l8k-host-network && -n $(find /var/lib/l8k-host-network -name '*.json' -print -quit) ]]; then kill -TERM "$PPID"; exit 143; fi
SH
/usr/bin/cat.real >/usr/bin/networkctl <<'SH'
#!/bin/bash
if [[ "$1" == status ]]; then
 if [[ -f /tmp/l8k-fault-network-file-missing ]]; then printf 'Network File: n/a\n';exit 0;fi
 if [[ -f /tmp/l8k-fault-active-definition ]]; then printf 'Network File: /run/systemd/network/10-netplan-management.network\n'; else printf 'Network File: /run/systemd/network/10-netplan-data0.network\n'; fi
else exec /usr/bin/networkctl.real "$@"; fi
SH
/usr/bin/cat.real >/usr/sbin/ip <<'SH'
#!/bin/bash
real_ip() {
 /usr/bin/setpriv --bounding-set=-all,+net_admin --inh-caps=-all --ambient-caps=-all /bin/bash -c 'grep "^CapEff:" /proc/self/status >>/tmp/l8k-netlink-capabilities.log; exec /usr/sbin/ip.real "$@"' -- "$@"
}
if [[ -f /tmp/l8k-fault-read-failed ]]; then printf 'ip %s\n' "$*" >>/tmp/l8k-reads-after-failure.log; fi
if [[ -f /tmp/l8k-fault-management-route-exit-7 && "$*" == '-j route get '* ]]; then touch /tmp/l8k-fault-read-failed; exit 7; fi
case "$*" in
  'link set dev test0 mtu 9000'|'link set dev test0 mtu 1500'|'-4 address add 172.25.0.2/24 dev test0'|'-4 address del 172.25.0.2/24 dev test0'|'-4 route add 172.25.0.0/16 via 172.25.0.1 dev test0'|'-4 route del 172.25.0.0/16 via 172.25.0.1 dev test0') printf '%s\n' "$*" >>/tmp/l8k-effects.log;;
  *'link set '*|*'address add '*|*'address del '*|*'route add '*|*'route del '*) printf 'PROHIBITED ip %s\n' "$*" >>/tmp/l8k-effects.log;exit 99;;
esac
if [[ "$*" == '-j -d link show dev test0' ]]; then
 real_ip "$@" | /usr/bin/jq 'map(.min_mtu=68|.max_mtu=9700|del(.linkinfo))' || exit
 if [[ -f /tmp/l8k-fault-runtime-status ]] && jq -e 'select(.phase=="route" and .addedRoute)' /var/lib/l8k-host-network/*.json >/dev/null 2>&1; then
  rm /tmp/l8k-fault-runtime-status
  printf 'real link JSON with expected MTU9000; exit1 at final readback\n' >>/tmp/l8k-runtime-status.log
  exit 1
 fi
 exit
fi
if [[ -f /tmp/l8k-fault-route || -f /tmp/l8k-fault-rollback-mtu || -f /tmp/l8k-fault-nm-absent-route ]]; then [[ "$*" != '-4 route add '* ]] || exit 2; fi
if [[ -f /tmp/l8k-fault-rollback-mtu && "$*" == 'link set dev test0 mtu 1500' ]]; then exit 2; fi
if [[ -f /tmp/l8k-fault-interrupt-mtu && "$*" == 'link set dev test0 mtu 9000' ]]; then real_ip "$@"; kill -TERM "$PPID"; exit 143; fi
if [[ -f /tmp/l8k-fault-interrupt-address && "$*" == '-4 address add 172.25.0.2/24 dev test0' ]]; then real_ip "$@"; kill -TERM "$PPID"; exit 143; fi
if [[ -f /tmp/l8k-fault-interrupt-route && "$*" == '-4 route add 172.25.0.0/16 via 172.25.0.1 dev test0' ]]; then real_ip "$@"; kill -TERM "$PPID"; exit 143; fi
if [[ -f /tmp/l8k-fault-readback && "$*" == '-j -4 route get 172.25.1.1 from 172.25.0.2' ]]; then printf '[{"dev":"eth0"}]\n'; exit 0; fi
real_ip "$@"
SH
/usr/bin/cat.real >/usr/sbin/netplan <<'SH'
#!/bin/bash
if [[ "$1" != get && "$1" != generate ]]; then printf 'PROHIBITED netplan %s\n' "$*" >>/tmp/l8k-effects.log;exit 99;fi
mutate_owned_fragment() {
 fragment_target=/etc/netplan/99-l8k-workload-__L8K_FRAGMENT_KEY__.yaml
 if [[ -f "$fragment_target" ]]; then
  python3 - "$fragment_target" <<'OWNED_YAML'
from pathlib import Path
import re, sys
p=Path(sys.argv[1])
data,count=re.subn(r"(?m)^([ ]*)data0:\n", lambda m:m.group(0)+" "*(len(m.group(1))+4)+"dhcp4: true\n", p.read_text(), count=1)
assert count==1
p.write_text(data)
OWNED_YAML
 else printf 'network:\n  ethernets:\n    data0:\n      dhcp4: true\n' >"$fragment_target";chmod 600 "$fragment_target";fi
 /usr/sbin/netplan.real get network.ethernets.data0.dhcp4 >/tmp/l8k-owned-dhcp-proof || exit
 [[ $(/usr/bin/cat.real /tmp/l8k-owned-dhcp-proof) == true ]] || exit 99
 touch /tmp/l8k-owned-mutated
}
if [[ "$*" == 'get network.ethernets.data0.mtu' ]] && compgen -G '/tmp/l8k-fault-normal-final-*' >/dev/null && jq -e 'select(.phase=="route" and .addedRoute)' /var/lib/l8k-host-network/*.json >/dev/null 2>&1; then
 /usr/sbin/netplan.real "$@" || exit
 if [[ -f /tmp/l8k-fault-normal-final-source ]]; then
  sed -i 's/      dhcp4: false/      dhcp4: true/' /etc/netplan/10-base.yaml
  /usr/sbin/netplan.real get network.ethernets.data0.dhcp4 >/tmp/l8k-owned-dhcp-proof || exit
 else mutate_owned_fragment;fi
 [[ $(/usr/bin/cat.real /tmp/l8k-owned-dhcp-proof) == true ]] || exit 99
 touch /tmp/l8k-normal-mutated /tmp/l8k-fault-normal-mutated
 rm -f /tmp/l8k-fault-normal-final-source /tmp/l8k-fault-normal-final-absent-owned
 exit 0
fi
if [[ "$*" == get && -f /tmp/l8k-fault-owned-fragment-capture ]]; then
 owned_get=0
 [[ ! -f /tmp/l8k-owned-get-count ]] || read -r owned_get </tmp/l8k-owned-get-count
 ((owned_get+=1));printf '%s\n' "$owned_get" >/tmp/l8k-owned-get-count
 if [[ "$owned_get" == 3 ]]; then /usr/sbin/netplan.real "$@" || exit;mutate_owned_fragment;rm /tmp/l8k-fault-owned-fragment-capture;exit 0;fi
fi
if [[ "$*" == 'get network.ethernets.data0.mtu' && -f /tmp/l8k-fault-owned-fragment-getter ]]; then
 /usr/sbin/netplan.real "$@" || exit
 mutate_owned_fragment
 rm /tmp/l8k-fault-owned-fragment-getter
 exit 0
fi
if [[ "$*" == get && -f /tmp/l8k-fault-source-configure-get ]]; then
 source_get=0
 [[ ! -f /tmp/l8k-source-configure-get-count ]] || read -r source_get </tmp/l8k-source-configure-get-count
 ((source_get+=1));printf '%s\n' "$source_get" >/tmp/l8k-source-configure-get-count
 if [[ "$source_get" == 2 ]]; then
  /usr/sbin/netplan.real "$@" || exit
  sed -i 's/      dhcp4: false/      dhcp4: true/' /etc/netplan/10-base.yaml
  touch /tmp/l8k-source-mutated
  rm /tmp/l8k-fault-source-configure-get
  exit 0
 fi
fi
if [[ "$*" == get && -f /tmp/l8k-fault-source-after-merged ]]; then
 /usr/sbin/netplan.real "$@" || exit
 sed -i 's/      dhcp4: false/      dhcp4: true/' /etc/netplan/10-base.yaml
 touch /tmp/l8k-source-mutated
 rm /tmp/l8k-fault-source-after-merged
 exit 0
fi
if [[ "$1" == generate ]]; then
 if [[ -f /tmp/l8k-fault-normal-mutated ]]; then printf 'PROHIBITED generation after concurrent DHCP source\n' >>/tmp/l8k-effects.log;exit 99;fi
 if [[ -f /tmp/l8k-fault-context-set-after-fragment ]]; then
  /usr/sbin/ip.real link show l8kctx0 >/dev/null 2>&1 && exit 99
  /usr/sbin/ip.real link add l8kctx0 type dummy || exit
  touch /tmp/l8k-context-link-created
 fi
 if [[ -f /tmp/l8k-fault-wait-owned-corrupt ]]; then
  /usr/sbin/netplan.real "$@" || exit
  python3 -c 'from pathlib import Path;p=Path("/run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf");prefix="ExecStart=/lib/systemd/systemd-networkd-wait-online --any -o routable";p.write_text("".join(line.replace(" -i test0", "") if line.startswith(prefix) else line for line in p.read_text().splitlines(keepends=True)))'
  rm /tmp/l8k-fault-wait-owned-corrupt
  exit 0
 fi
 if [[ -f /tmp/l8k-fault-generate ]]; then rm /tmp/l8k-fault-generate; exit 2; fi
 if [[ -f /tmp/l8k-fault-source-drift ]]; then printf '# concurrent admin\n' >>/etc/netplan/10-base.yaml; fi
 if [[ -f /tmp/l8k-fault-interrupt-generate ]]; then kill -TERM "$PPID"; exit 143; fi
fi
exec /usr/sbin/netplan.real "$@"
SH
chmod 755 /usr/sbin/dmidecode /usr/sbin/lspci /usr/sbin/ethtool /usr/sbin/ip /usr/sbin/netplan /usr/bin/cat /usr/bin/readlink /usr/bin/systemctl /usr/bin/networkctl /usr/bin/sync /usr/bin/udevadm /usr/bin/mktemp /usr/bin/sha256sum /usr/bin/jq
/usr/sbin/ip.real link show test0 >/dev/null 2>&1 || /usr/sbin/ip.real link add test0 type dummy
/usr/sbin/ip.real link set test0 up
/usr/sbin/ip.real link show test1 >/dev/null 2>&1 || /usr/sbin/ip.real link add test1 type dummy
/usr/sbin/ip.real link set test1 up
`

func prepareHostNetworkMACMatchedManagement(t *testing.T, container string) {
	t.Helper()
	containerShell(t, container, `IFS= read -r mac </sys/class/net/eth0/address
python3 -c 'from pathlib import Path;import sys;p=Path("/etc/netplan/10-base.yaml");p.write_text(p.read_text().replace("match: {name: eth0}", "match: {macaddress: "+sys.argv[1]+"}\n      set-name: eth0"))' "$mac"
/usr/sbin/netplan.real generate >/dev/null
probe=$(mktemp -d /tmp/l8k-empty-context-XXXXXX)
trap 'rm -rf -- "$probe"' EXIT
mkdir -p "$probe/etc/netplan"
cp /etc/netplan/10-base.yaml "$probe/etc/netplan/10-base.yaml"
(umask 022;/usr/libexec/netplan/generate.real --root-dir "$probe") >/dev/null
grep -F -- '-i eth0' /run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf >/dev/null
if grep -F -- '-i eth0' "$probe/run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf";then exit 1;fi`)
}

const hostNetworkContextUnitChecks = `
context_test=$(mktemp -d /tmp/l8k-context-unit-XXXXXX)
trap 'rm -rf -- "$context_test"' EXIT
context_fixture=$context_test/interfaces
mkdir -p "$context_fixture"/{lo,eth0,net@a+,.mgmt0,..x}
printf '\n' >"$context_fixture/lo/address"
for name in eth0 net@a+ .mgmt0 ..x;do printf '02:00:00:00:00:01\n' >"$context_fixture/$name/address";done
printf 'not an interface\n' >"$context_fixture/bonding_masters"
mkdir "$context_fixture/eth0/device"
ln -s /sys/bus/pci/drivers/mlx5_core "$context_fixture/eth0/device/driver"
capture_failure_code='' failure_reason=''
read_generator_context catalog
[[ "$catalog" == *$'lo\t-\t0\t-\n'* && "$catalog" == *$'.mgmt0\t02:00:00:00:00:01\t0\t-\n'* && "$catalog" == *$'..x\t02:00:00:00:00:01\t0\t-\n'* ]]
[[ "$catalog" == *$'eth0\t02:00:00:00:00:01\t1\tmlx5_core\n'* && "$catalog" != *bonding_masters* ]]
write_generator_context "$context_test/shadow" "$catalog"
[[ -f "$context_test/shadow/sys/class/net/lo/address" && -z $(cat "$context_test/shadow/sys/class/net/lo/address") ]]
[[ $(stat -c %a "$context_test/shadow/sys/class/net/lo/address") == 600 ]]
[[ $(readlink "$context_test/shadow/sys/class/net/eth0/device/driver") == mlx5_core ]]
[[ ! -e "$context_test/shadow/sys/class/net/lo/device/driver" ]]
[[ $(cat "$context_test/shadow/sys/class/net/.mgmt0/address") == 02:00:00:00:00:01 ]]
generator_context=$catalog
generator_context_fence
printf '02:00:00:00:00:02\n' >"$context_fixture/eth0/address"
if generator_context_fence;then exit 1;fi
[[ "$failure_reason" == HOST_NETWORK_GENERATOR_CONTEXT_CHANGED ]]
printf '02:00:00:00:00:01\n' >"$context_fixture/eth0/address"
rm "$context_fixture/eth0/device/driver"
ln -s ixgbe "$context_fixture/eth0/device/driver"
if generator_context_fence;then exit 1;fi
[[ "$failure_reason" == HOST_NETWORK_GENERATOR_CONTEXT_CHANGED ]]
rm "$context_fixture/eth0/device/driver"
read_generator_context no_driver
[[ "$no_driver" == *$'eth0\t02:00:00:00:00:01\t0\t-\n'* ]]
read() { return 7; }
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_READ_GENERATOR_CONTEXT_BASH_EXIT_7 ]]
unset -f read
ln -s mlx5_core "$context_fixture/eth0/device/driver"
readlink() { return 7; }
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_READ_GENERATOR_CONTEXT_READLINK_EXIT_7 ]]
unset -f readlink
readlink() { mkdir -p "$context_fixture/.late";printf '02:00:00:00:00:03\n' >"$context_fixture/.late/address";command readlink "$@"; }
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_CHANGED ]]
unset -f readlink
# Preserve a full InfiniBand address byte-for-byte without imposing the
# selected Ethernet PF's6-octet identity constraint on unrelated links.
ib_address=AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD
mkdir "$context_fixture/ib0"
printf '%s\n' "$ib_address" >"$context_fixture/ib0/address"
read_generator_context ib_catalog
[[ "$ib_catalog" == *"ib0"$'\t'"$ib_address"$'\t0\t-\n'* ]]
write_generator_context "$context_test/ib-shadow" "$ib_catalog"
[[ $(cat "$context_test/ib-shadow/sys/class/net/ib0/address") == "$ib_address" ]]
printf 'not-a-kernel-address\n' >"$context_fixture/ib0/address"
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED ]]
printf '%0200d\n' 0 >"$context_fixture/ib0/address"
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED ]]
printf '%s\n' "$ib_address" >"$context_fixture/ib0/address"
rm "$context_fixture/eth0/device/driver"
ln -s .. "$context_fixture/eth0/device/driver"
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED ]]
rm "$context_fixture/eth0/device/driver"
printf 'not-a-symlink\n' >"$context_fixture/eth0/device/driver"
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED ]]
rm "$context_fixture/eth0/device/driver"
mkdir "$context_fixture/bad:name"
printf '00:00:00:00:00:00\n' >"$context_fixture/bad:name/address"
if read_generator_context rejected;then exit 1;fi
[[ "$capture_failure_code" == HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED ]]
`

func independentWaitOnlineProjection(data []byte, iface string) []byte {
	const prefix = "ExecStart=/lib/systemd/systemd-networkd-wait-online --any -o routable"
	var output strings.Builder
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			needle := " -i " + iface
			for offset := 0; offset < len(line); {
				found := strings.Index(line[offset:], needle)
				if found < 0 {
					break
				}
				found += offset
				end := found + len(needle)
				if end == len(line) || line[end] == ' ' || line[end] == '\n' {
					line = line[:found] + line[end:]
					break
				}
				offset = end
			}
			if line == prefix+"\n" {
				continue
			}
		}
		output.WriteString(line)
	}
	return []byte(output.String())
}

const hostNetworkWaitOnlineUnitChecks = `
wait_test=$(mktemp -d /tmp/l8k-wait-unit-XXXXXX)
trap 'rm -rf -- "$wait_test"' EXIT
iface=test0 logical=data0
wait_path=$wait_test/run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf
mkdir -p "${wait_path%/*}"
header=$'[Unit]\nConditionPathIsSymbolicLink=/run/systemd/generator/network-online.target.wants/systemd-networkd-wait-online.service\n\n[Service]\nExecStart=\n'
local_line='ExecStart=/lib/systemd/systemd-networkd-wait-online -i test0:degraded -i eth0:degraded'
route_line='ExecStart=/lib/systemd/systemd-networkd-wait-online --any -o routable'
base="$header$local_line"$'\n'"$route_line -i eth0"$'\n'
printf '%s' "$base" >"$wait_path"
chmod 644 "$wait_path"
wait_online_content "$wait_test" project >"$wait_test/base"
for members in '-i test0 -i eth0' '-i eth0 -i test0';do
 printf '%s' "$header$local_line"$'\n'"$route_line $members"$'\n' >"$wait_path"
 wait_online_content "$wait_test" project >"$wait_test/candidate"
 cmp "$wait_test/base" "$wait_test/candidate"
 wait_online_content "$wait_test" owned 1
 if wait_online_content "$wait_test" owned 0;then exit 1;fi
done
# Protected leaf content cannot bypass protected directory ancestry.
wait_parent=${wait_path%/*}
chmod 777 "$wait_parent"
if wait_online_content "$wait_test" owned 1;then exit 1;fi
chmod 755 "$wait_parent"
mv "$wait_parent" "$wait_test/parent-owned"
ln -s "$wait_test/parent-owned" "$wait_parent"
if wait_online_content "$wait_test" owned 1;then exit 1;fi
rm "$wait_parent"
mv "$wait_test/parent-owned" "$wait_parent"
# Effective proof rejects commands in the wrong section or cleared later.
printf '%s' "$header$local_line"$'\n'"$route_line -i test0 -i eth0"$'\nExecStart=\n' >"$wait_path"
if wait_online_content "$wait_test" owned 1;then exit 1;fi
printf '%s' "$header$local_line"$'\n'"$route_line -i test0 -i eth0"$'\n' | sed 's/\[Service\]/[Unit]/' >"$wait_path"
if wait_online_content "$wait_test" owned 1;then exit 1;fi
printf '%s' "$header$local_line"$'\n'"$route_line -i test0 -i eth0"$'\n ExecStart=\n' >"$wait_path"
if wait_online_content "$wait_test" owned 1;then exit 1;fi
# Prefix-similar names and opaque lines remain exact nonselected bytes.
printf '%s' "$header$local_line"$'\n# opaque  nonselected condition \n'"$route_line -i test01 -i test0 -i eth0"$'\n\n' >"$wait_path"
wait_online_content "$wait_test" project >"$wait_test/candidate"
printf '%s' "$header$local_line"$'\n# opaque  nonselected condition \n'"$route_line -i test01 -i eth0"$'\n\n' >"$wait_test/expected"
cmp "$wait_test/expected" "$wait_test/candidate"
# When selected is the only routable member, omit that one command only.
printf '%s' "$header$local_line"$'\n'"$route_line -i test0"$'\n' >"$wait_path"
wait_online_content "$wait_test" project >"$wait_test/candidate"
printf '%s' "$header$local_line"$'\n' >"$wait_test/expected"
cmp "$wait_test/expected" "$wait_test/candidate"
# Optional or unmatched definitions need no selected contributions.
printf '%s' "$header""ExecStart=/lib/systemd/systemd-networkd-wait-online -i eth0:degraded"$'\n'"$route_line -i eth0"$'\n' >"$wait_path"
wait_online_content "$wait_test" owned 0
if wait_online_content "$wait_test" owned 1;then exit 1;fi
# A genuinely absent routable command remains absent and byte-identical.
printf '%s' "$header""ExecStart=/lib/systemd/systemd-networkd-wait-online -i eth0:degraded"$'\n' >"$wait_path"
wait_online_content "$wait_test" owned 0
wait_online_content "$wait_test" project >"$wait_test/absent-route"
cmp "$wait_path" "$wait_test/absent-route"
for malformed in "$route_line -i test0 -i test0" "$route_line -i test0 --unexpected value" "$route_line  -i test0";do
 printf '%s' "$header$local_line"$'\n'"$malformed"$'\n' >"$wait_path"
 if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
done
printf '%s' "$header$local_line"$'\n'"$route_line -i test0"$'\n'"$route_line -i eth0"$'\n' >"$wait_path"
if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
printf '%s' "$base" >"$wait_path";printf '\000\n' >>"$wait_path"
if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
printf '%s' "$base" >"$wait_path";printf '\t\n' >>"$wait_path"
if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
printf '%s' "$header$local_line" >"$wait_path"
if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
printf '%070000d\n' 0 >"$wait_path"
if wait_online_content "$wait_test" project >"$wait_test/rejected";then exit 1;fi
# Raw planned fences still detect a change to only the selected token.
printf '%s' "$base" >"$wait_path"
artifact_record "$wait_test" "$wait_path" all >"$wait_test/raw-before"
artifact_record "$wait_test" "$wait_path" nonselected >"$wait_test/project-before"
printf '%s' "$header$local_line"$'\n'"$route_line -i test0 -i eth0"$'\n' >"$wait_path"
artifact_record "$wait_test" "$wait_path" all >"$wait_test/raw-after"
artifact_record "$wait_test" "$wait_path" nonselected >"$wait_test/project-after"
if cmp -s "$wait_test/raw-before" "$wait_test/raw-after";then exit 1;fi
cmp "$wait_test/project-before" "$wait_test/project-after"
chmod 600 "$wait_path"
artifact_record "$wait_test" "$wait_path" nonselected >"$wait_test/metadata-after"
if cmp -s "$wait_test/project-after" "$wait_test/metadata-after";then exit 1;fi
expected_mac=02:00:00:00:00:01
wait_online_optional=false
generator_context=$'test0\t02:00:00:00:00:01\t0\t-\neth0\t04:00:00:00:00:01\t0\t-\n'
selected_wait_online_requirement expected
[[ "$expected" == 1 ]]
generator_context+=$'duplicate\t02:00:00:00:00:01\t0\t-\n'
selected_wait_online_requirement expected
[[ "$expected" == 1 ]]
generator_context=$'test0\t02:00:00:00:00:03\t0\t-\nother0\t02:00:00:00:00:01\t0\t-\n'
if selected_wait_online_requirement expected;then exit 1;fi
wait_online_optional=true
selected_wait_online_requirement expected
[[ "$expected" == 0 ]]
wait_online_optional=false
generator_context=$'test0\t02:00:00:00:00:03\t0\t-\n'
selected_wait_online_requirement expected
[[ "$expected" == 0 ]]
`
