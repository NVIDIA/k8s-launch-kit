// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateNetplanInterfaceRequiresNoLeaseAndPreservesSource(t *testing.T) {
	data := []byte(netplanFixture)
	original := bytes.Clone(data)
	id, err := NetplanInterfaceID("ens5f0np0", "02:00:00:00:00:01", data)
	if err != nil || id != "data-pf0" {
		t.Fatalf("selected interface ID without a lease: id=%q error=%v", id, err)
	}
	if !bytes.Equal(data, original) {
		t.Fatal("validation modified the merged configuration")
	}
}

func TestValidateNetplanInterfaceAndPlanningShareSupportRules(t *testing.T) {
	for _, test := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "DHCP", old: "      dhcp4: false", new: "      dhcp4: true"},
		{name: "renderer", old: "  renderer: networkd", new: "  renderer: NetworkManager"},
		{name: "name binding", old: "      set-name: ens5f0np0", new: "      set-name: other0"},
		{name: "MAC binding", old: "02:00:00:00:00:01", new: "02:00:00:00:00:02"},
		{name: "invalid MTU", old: "      mtu: 9000", new: "      mtu: invalid"},
		{name: "unknown field", old: "      mtu: 9000", new: "      mtu: 9000\n      arbitrary-setting: true"},
		{name: "default route", old: "      mtu: 9000", new: "      mtu: 9000\n      routes: [{to: default, via: 192.0.2.1}]"},
		{name: "IPv4 default route", old: "      mtu: 9000", new: "      mtu: 9000\n      routes: [{to: 0.0.0.0/0, via: 192.0.2.1}]"},
		{name: "IPv6 default route", old: "      mtu: 9000", new: "      mtu: 9000\n      routes: [{to: '::/0', via: '2001:db8::1'}]"},
		{name: "invalid route destination", old: "      mtu: 9000", new: "      mtu: 9000\n      routes: [{to: invalid}]"},
		{name: "invalid address", old: "      mtu: 9000", new: "      mtu: 9000\n      addresses: [invalid]"},
		{name: "complex address", old: "      mtu: 9000", new: "      mtu: 9000\n      addresses: [{192.0.2.2/24: {label: selected}}]"},
		{name: "virtual membership", old: "  ethernets:", new: "  bridges:\n    bridge0:\n      interfaces: [data-pf0]\n  ethernets:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, test.old, test.new, 1))
			if _, err := NetplanInterfaceID(desired.InterfaceName, desired.PermanentMAC, desired.MergedNetplanYAML); err == nil {
				t.Fatal("lease-independent validation accepted an unsupported backend definition")
			}
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatal("planning accepted an unsupported backend definition")
			}
		})
	}
}

func TestValidateNetplanInterfaceDoesNotClaimAddressLeaseCompatibility(t *testing.T) {
	desired := desiredForTest()
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "      dhcp4: false", "      dhcp4: false\n      addresses: [10.0.8.2/29]", 1))
	if _, err := NetplanInterfaceID(desired.InterfaceName, desired.PermanentMAC, desired.MergedNetplanYAML); err != nil {
		t.Fatalf("supported interface definition: %v", err)
	}
	if _, err := PlanNetplan(desired); err == nil || !strings.Contains(err.Error(), "conflicting IPv4 address") {
		t.Fatalf("planning must reject an address differing from the actual lease: %v", err)
	}
}

func TestValidateNetplanInterfaceRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		mac  string
		data []byte
	}{
		{name: "", mac: "02:00:00:00:00:01", data: []byte(netplanFixture)},
		{name: "ens5f0np0", mac: "01:00:00:00:00:01", data: []byte(netplanFixture)},
		{name: "ens5f0np0", mac: "02:00:00:00:00:01", data: nil},
		{name: "ens5f0np0", mac: "02:00:00:00:00:01", data: bytes.Repeat([]byte(" "), (1<<20)+1)},
	} {
		if _, err := NetplanInterfaceID(test.name, test.mac, test.data); err == nil {
			t.Fatal("invalid interface identity or configuration was accepted")
		}
	}
}
