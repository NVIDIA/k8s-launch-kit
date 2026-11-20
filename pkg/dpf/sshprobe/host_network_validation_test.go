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

func TestHostNetworkValidationOwnsItsReadDeadlineAndHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := config.Target{Role: "hosts", SSH: &config.SSHAccess{}, Endpoint: "invalid"}
	_, err := VerifyHostWorkloadNetwork(ctx, target, hostNetworkTestRequest(), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resource snapshot required an external deadline or attempted access: %v", err)
	}
}

func configuredHostNetworkSnapshot() hostNetworkSnapshot {
	s := hostNetworkTestSnapshot()
	s.Link = json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":9000,"min_mtu":68,"max_mtu":9700,"flags":["UP"]}]`)
	s.Addresses = json.RawMessage(`[{"addr_info":[{"family":"inet","local":"172.25.0.2","prefixlen":24}]}]`)
	s.Routes = json.RawMessage(`[{"dst":"default","gateway":"192.0.2.1","dev":"eth0"},{"dst":"172.25.0.0/16","gateway":"172.25.0.1","dev":"test0"}]`)
	s.Merged = base64.StdEncoding.EncodeToString([]byte("network:\n  version: 2\n  renderer: networkd\n  ethernets:\n    data0:\n      match: {macaddress: '02:00:00:00:00:01'}\n      set-name: test0\n      mtu: 9000\n      addresses: [172.25.0.2/24]\n      routes: [{to: 172.25.0.0/16, via: 172.25.0.1}]\n"))
	return s
}

func TestHostNetworkValidationRequiresOnlyCurrentState(t *testing.T) {
	f := &hostNetworkFakeRemote{snapshot: configuredHostNetworkSnapshot()}
	s := hostNetworkSession{remote: f}
	request := hostNetworkTestRequest()
	request.LeaseBinding = "" // Fresh validation has no earlier transaction.
	got, err := s.verifyConfiguration(context.Background(), request)
	if err != nil || !got.RuntimeVerified || !got.PersistentVerified || got.MTU != 9000 || got.BootID == "" {
		t.Fatalf("current configuration not verified: %+v %v", got, err)
	}
	if strings.Join(f.calls, ",") != "host-network-ssh-path,host-network-snapshot,host-network-ssh-path,host-network-snapshot" {
		t.Fatalf("validation requested a transaction action: %v", f.calls)
	}
}

func TestHostNetworkQualificationFencesAcrossFreshReadSessions(t *testing.T) {
	remote := &hostNetworkFakeRemote{snapshot: configuredHostNetworkSnapshot()}
	session := hostNetworkSession{remote: remote}
	request := hostNetworkTestRequest()
	request.LeaseBinding = ""
	first, err := session.verifyConfiguration(context.Background(), request)
	if err != nil || !first.Qualified() {
		t.Fatalf("entry qualification failed: %+v %v", first, err)
	}
	second, err := session.verifyConfiguration(context.Background(), request)
	if err != nil || !first.SameCurrent(second) {
		t.Fatalf("stable qualification changed: %+v %v", second, err)
	}
	for _, change := range []struct {
		name string
		edit func(*hostNetworkSnapshot)
	}{
		{"persistent source", func(s *hostNetworkSnapshot) { s.SourceDigest = strings.Repeat("e", 64) }},
		{"runtime", func(s *hostNetworkSnapshot) { s.RuntimeDigest = strings.Repeat("e", 64) }},
		{"boot", func(s *hostNetworkSnapshot) { s.BootID = "123e4567-e89b-12d3-a456-426614174002" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			remote.snapshot = configuredHostNetworkSnapshot()
			change.edit(&remote.snapshot)
			current, err := session.verifyConfiguration(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if first.SameCurrent(current) {
				t.Fatal("cross-phase source/runtime/boot drift was accepted")
			}
		})
	}
	remote.snapshot = configuredHostNetworkSnapshot()
	remote.snapshot.Merged = hostNetworkTestSnapshot().Merged
	current, err := session.verifyConfiguration(context.Background(), request)
	if Code(err) != "HOST_NETWORK_PERSISTENT_MISMATCH" || current.Qualified() || first.SameCurrent(current) {
		t.Fatalf("persistent desired configuration drift remained qualified: %+v %v", current, err)
	}
	public, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "source") || strings.Contains(string(public), "runtimeDigest") || strings.Contains(string(public), "current") {
		t.Fatalf("private qualification escaped serialization: %s", public)
	}
	if (HostNetworkValidation{RuntimeVerified: true, PersistentVerified: true, BootID: first.BootID, MTU: first.MTU}).Qualified() {
		t.Fatal("booleans manufactured a current qualification")
	}
}

func TestHostNetworkValidationSeparatesRuntimeAndPersistentMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*hostNetworkSnapshot)
		want string
	}{
		{"runtime MTU", func(s *hostNetworkSnapshot) { s.Link = hostNetworkTestSnapshot().Link }, "HOST_NETWORK_RUNTIME_MISMATCH"},
		{"missing address", func(s *hostNetworkSnapshot) { s.Addresses = hostNetworkTestSnapshot().Addresses }, "HOST_NETWORK_RUNTIME_MISMATCH"},
		{"missing route", func(s *hostNetworkSnapshot) { s.Routes = hostNetworkTestSnapshot().Routes }, "HOST_NETWORK_RUNTIME_MISMATCH"},
		{"persistent configuration", func(s *hostNetworkSnapshot) { s.Merged = hostNetworkTestSnapshot().Merged }, "HOST_NETWORK_PERSISTENT_MISMATCH"},
		{"missing tools", func(s *hostNetworkSnapshot) { s.Error = "HOST_NETWORK_TOOL_MISSING" }, "HOST_NETWORK_TOOL_MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &hostNetworkFakeRemote{snapshot: configuredHostNetworkSnapshot()}
			tc.edit(&f.snapshot)
			s := hostNetworkSession{remote: f}
			got, err := s.verifyConfiguration(context.Background(), hostNetworkTestRequest())
			if err == nil || err.Error() != tc.want || got.RuntimeVerified && got.PersistentVerified {
				t.Fatalf("mismatch accepted or lost reason: %+v %v", got, err)
			}
			for _, call := range f.calls {
				if call != "host-network-ssh-path" && call != "host-network-snapshot" {
					t.Fatalf("failure requested an effect: %s", call)
				}
			}
		})
	}
}

type changingHostNetworkRemote struct {
	hostNetworkFakeRemote
	change func(*hostNetworkSnapshot)
	reads  int
}

func (f *changingHostNetworkRemote) Run(ctx context.Context, stage, command string) (string, error) {
	if stage == "host-network-snapshot" {
		f.reads++
		if f.reads == 2 {
			f.change(&f.snapshot)
		}
	}
	return f.hostNetworkFakeRemote.Run(ctx, stage, command)
}

func TestHostNetworkValidationRejectsChangesBetweenSnapshots(t *testing.T) {
	for _, change := range []func(*hostNetworkSnapshot){
		func(s *hostNetworkSnapshot) { s.BootID = "123e4567-e89b-12d3-a456-426614174002" },
		func(s *hostNetworkSnapshot) { s.SourceDigest = strings.Repeat("e", 64) },
		func(s *hostNetworkSnapshot) { s.RuntimeDigest = strings.Repeat("e", 64) },
	} {
		f := &changingHostNetworkRemote{hostNetworkFakeRemote: hostNetworkFakeRemote{snapshot: configuredHostNetworkSnapshot()}, change: change}
		s := hostNetworkSession{remote: f}
		got, err := s.verifyConfiguration(context.Background(), hostNetworkTestRequest())
		if err == nil || err.Error() != "HOST_NETWORK_OBSERVATION_CHANGED" || got.RuntimeVerified || got.PersistentVerified {
			t.Fatalf("changed observation accepted: %+v %v", got, err)
		}
	}
}
