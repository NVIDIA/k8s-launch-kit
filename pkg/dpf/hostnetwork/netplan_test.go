// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const netplanFixture = `network:
  version: 2
  renderer: networkd
  ethernets:
    data-pf0:
      match:
        macaddress: "02:00:00:00:00:01"
      set-name: ens5f0np0
      mtu: 9000
      dhcp4: false
    management:
      dhcp4: true
      addresses:
        - 192.0.2.20/24:
            label: mgmt
      routes:
        - to: default
          via: 192.0.2.1
`

func desiredForTest() Desired {
	return Desired{
		InterfaceName:     "ens5f0np0",
		PermanentMAC:      "02:00:00:00:00:01",
		MTU:               9000,
		HostAddressPrefix: "10.0.0.2/29",
		Gateway:           "10.0.0.1",
		WorkloadCIDR:      "10.0.0.0/16",
		MergedNetplanYAML: []byte(netplanFixture),
	}
}

func TestPlanNetplanRendersOnlyMissingFieldsAndPreservesMergedSource(t *testing.T) {
	desired := desiredForTest()
	original := append([]byte(nil), desired.MergedNetplanYAML...)
	plan, err := PlanNetplan(desired)
	if err != nil {
		t.Fatalf("PlanNetplan(): %v", err)
	}
	if plan.LogicalID != "data-pf0" || !plan.MTUAlreadyDeclared || !plan.MTUMatchesDesired || plan.AddressAlreadyDeclared || plan.RouteAlreadyDeclared || plan.ReasonCode != "NETPLAN_FRAGMENT_REQUIRED" {
		t.Fatalf("unexpected plan metadata: %#v", plan)
	}
	if !bytes.Equal(desired.MergedNetplanYAML, original) {
		t.Fatal("planner mutated source Netplan bytes")
	}
	merged := mergeYAML(t, desired.MergedNetplanYAML, plan.DesiredFragment)
	network := merged["network"].(map[string]any)
	interfaces := network["ethernets"].(map[string]any)
	selected := interfaces["data-pf0"].(map[string]any)
	if selected["set-name"] != "ens5f0np0" || selected["mtu"] != 9000 || selected["dhcp4"] != false {
		t.Fatalf("source match/settings were not preserved: %#v", selected)
	}
	if len(selected["addresses"].([]any)) != 1 || selected["addresses"].([]any)[0] != "10.0.0.2/29" {
		t.Fatalf("unexpected merged address list: %#v", selected["addresses"])
	}
	routes := selected["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("unexpected selected route list: %#v", routes)
	}
	route := routes[0].(map[string]any)
	if route["to"] != "10.0.0.0/16" || route["via"] != "10.0.0.1" {
		t.Fatalf("unexpected workload route: %#v", route)
	}
	management := interfaces["management"].(map[string]any)
	managementAddress := management["addresses"].([]any)[0].(map[string]any)["192.0.2.20/24"].(map[string]any)
	if management["routes"].([]any)[0].(map[string]any)["to"] != "default" || managementAddress["label"] != "mgmt" {
		t.Fatalf("unrelated management network was changed: %#v", management)
	}
	if strings.Contains(string(plan.DesiredFragment), "default") || strings.Contains(string(plan.DesiredFragment), "management") {
		t.Fatalf("fragment contains unrelated networking: %s", plan.DesiredFragment)
	}
}

func TestPlanNetplanClassifiesAlreadyDeclaredStateWithoutFragment(t *testing.T) {
	desired := desiredForTest()
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture,
		"      dhcp4: false\n", "      dhcp4: false\n      addresses: [10.0.0.2/29]\n      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.1\n", 1))
	plan, err := PlanNetplan(desired)
	if err != nil {
		t.Fatalf("PlanNetplan(): %v", err)
	}
	if !plan.MTUAlreadyDeclared || !plan.MTUMatchesDesired || !plan.AddressAlreadyDeclared || !plan.RouteAlreadyDeclared || len(plan.DesiredFragment) != 0 || plan.ReasonCode != "NETPLAN_DESIRED_STATE_DECLARED" {
		t.Fatalf("expected declarative no-op classification, got %#v", plan)
	}
}

func TestPlanNetplanEmitsOnlyMissingEntry(t *testing.T) {
	desired := desiredForTest()
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture,
		"      dhcp4: false\n", "      dhcp4: false\n      addresses: [10.0.0.2/29]\n", 1))
	plan, err := PlanNetplan(desired)
	if err != nil {
		t.Fatalf("PlanNetplan(): %v", err)
	}
	if !plan.MTUAlreadyDeclared || !plan.MTUMatchesDesired || !plan.AddressAlreadyDeclared || plan.RouteAlreadyDeclared {
		t.Fatalf("unexpected declaration facts: %#v", plan)
	}
	fragment := string(plan.DesiredFragment)
	if strings.Contains(fragment, "addresses:") || !strings.Contains(fragment, "routes:") {
		t.Fatalf("fragment should contain only the missing route: %s", fragment)
	}
}

func TestPlanNetplanReconcilesOnlySelectedInterfaceMTU(t *testing.T) {
	tests := []struct {
		name        string
		mtu         int
		selectedMTU string
		wantMTU     int
		declared    bool
		matches     bool
	}{
		{name: "1500 to 9000", mtu: 9000, selectedMTU: "1500", wantMTU: 9000, declared: true},
		{name: "missing to desired", mtu: 9000, wantMTU: 9000},
		{name: "already matching", mtu: 9000, selectedMTU: "9000", declared: true, matches: true},
		{name: "custom desired", mtu: 4000, selectedMTU: "1500", wantMTU: 4000, declared: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			desired.MTU = test.mtu
			base := strings.Replace(netplanFixture, "      dhcp4: false\n", "      dhcp4: false\n      addresses: [10.0.0.2/29]\n      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.1\n", 1)
			if test.selectedMTU == "" {
				base = strings.Replace(base, "      mtu: 9000\n", "", 1)
			} else {
				base = strings.Replace(base, "mtu: 9000", "mtu: "+test.selectedMTU, 1)
			}
			desired.MergedNetplanYAML = []byte(base)
			plan, err := PlanNetplan(desired)
			if err != nil {
				t.Fatalf("PlanNetplan(): %v", err)
			}
			if plan.MTUAlreadyDeclared != test.declared {
				t.Fatalf("MTUAlreadyDeclared=%v, want %v", plan.MTUAlreadyDeclared, test.declared)
			}
			if plan.MTUMatchesDesired != test.matches {
				t.Fatalf("MTUMatchesDesired=%v, want %v", plan.MTUMatchesDesired, test.matches)
			}
			if test.matches {
				if len(plan.DesiredFragment) != 0 {
					t.Fatalf("matching MTU and address/route should produce no fragment: %s", plan.DesiredFragment)
				}
				return
			}
			fragment := string(plan.DesiredFragment)
			var decoded map[string]any
			if err := yaml.Unmarshal(plan.DesiredFragment, &decoded); err != nil {
				t.Fatalf("decode fragment: %v", err)
			}
			selected := decoded["network"].(map[string]any)["ethernets"].(map[string]any)["data-pf0"].(map[string]any)
			if selected["mtu"] != test.wantMTU {
				t.Fatalf("fragment MTU=%v, want %d: %s", selected["mtu"], test.wantMTU, fragment)
			}
			if _, ok := selected["addresses"]; ok || selected["routes"] != nil {
				t.Fatalf("MTU-only update should not duplicate matching address/route: %#v", selected)
			}
		})
	}
}

func TestPlanNetplanAcceptsAllAddressRouteDeclarationCombinationsWithMatchingMTU(t *testing.T) {
	base := strings.Replace(netplanFixture, "      dhcp4: false\n", "      dhcp4: false\n", 1)
	declarations := []struct {
		name              string
		entries           string
		addressDeclared   bool
		routeDeclared     bool
		fragmentAddresses bool
		fragmentRoutes    bool
	}{
		{name: "neither", fragmentAddresses: true, fragmentRoutes: true},
		{name: "address only", entries: "      addresses: [10.0.0.2/29]\n", addressDeclared: true, fragmentRoutes: true},
		{name: "route only", entries: "      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.1\n", routeDeclared: true, fragmentAddresses: true},
		{name: "both", entries: "      addresses: [10.0.0.2/29]\n      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.1\n", addressDeclared: true, routeDeclared: true},
	}
	for _, test := range declarations {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			desired.MergedNetplanYAML = []byte(strings.Replace(base, "      dhcp4: false\n", "      dhcp4: false\n"+test.entries, 1))
			plan, err := PlanNetplan(desired)
			if err != nil {
				t.Fatalf("PlanNetplan(): %v", err)
			}
			if !plan.MTUAlreadyDeclared || !plan.MTUMatchesDesired {
				t.Fatal("matching MTU was not retained as declared desired state")
			}
			if plan.AddressAlreadyDeclared != test.addressDeclared || plan.RouteAlreadyDeclared != test.routeDeclared {
				t.Fatalf("unexpected address/route declaration state: %#v", plan)
			}
			var fragment map[string]any
			if len(plan.DesiredFragment) > 0 {
				if err := yaml.Unmarshal(plan.DesiredFragment, &fragment); err != nil {
					t.Fatalf("decode fragment: %v", err)
				}
			}
			var selected map[string]any
			if len(fragment) > 0 {
				selected = fragment["network"].(map[string]any)["ethernets"].(map[string]any)["data-pf0"].(map[string]any)
			}
			_, hasAddresses := selected["addresses"]
			_, hasRoutes := selected["routes"]
			if hasAddresses != test.fragmentAddresses || hasRoutes != test.fragmentRoutes {
				t.Fatalf("unexpected missing-field fragment: %#v", selected)
			}
		})
	}
}

func TestPlanNetplanRejectsInvalidDesiredOrExistingMTU(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Desired)
	}{
		{name: "desired zero", change: func(d *Desired) { d.MTU = 0 }},
		{name: "desired above Linux range", change: func(d *Desired) { d.MTU = 65536 }},
		{name: "existing zero", change: func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "mtu: 9000", "mtu: 0", 1))
		}},
		{name: "existing too large", change: func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "mtu: 9000", "mtu: 65536", 1))
		}},
		{name: "existing string", change: func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "mtu: 9000", `mtu: "9000"`, 1))
		}},
		{name: "existing null", change: func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "mtu: 9000", "mtu: null", 1))
		}},
		{name: "existing boolean", change: func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "mtu: 9000", "mtu: true", 1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			test.change(&desired)
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatal("PlanNetplan() unexpectedly accepted invalid MTU")
			}
		})
	}
}

func TestPlanNetplanRejectsUnprovenSelectorsAndTopology(t *testing.T) {
	tests := []struct {
		name   string
		change string
	}{
		{"selected driver selector", `      match:
        macaddress: "02:00:00:00:00:01"
        driver: ixgbe`},
		{"overlapping driver-only selector", `    broad-driver:
      match:
        driver: mlx5_core`},
		{"selected VLAN link by logical ID", `  vlans:
    vlan10:
      id: 10
      link: data-pf0`},
		{"selected VLAN link by interface name", `  vlans:
    vlan10:
      id: 10
      link: ens5f0np0`},
		{"selected legacy gateway", `      gateway4: 10.0.0.1`},
		{"selected legacy IPv6 gateway", `      gateway6: 2001:db8::1`},
		{"selected Open vSwitch", `      openvswitch: {}`},
		{"unknown selected setting", `      accept-ra: true`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			switch test.name {
			case "selected driver selector":
				desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture,
					"        macaddress: \"02:00:00:00:00:01\"",
					"        macaddress: \"02:00:00:00:00:01\"\n        driver: ixgbe", 1))
			case "selected legacy gateway", "selected legacy IPv6 gateway", "selected Open vSwitch", "unknown selected setting":
				desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "      dhcp4: false\n", "      dhcp4: false\n"+test.change+"\n", 1))
			case "overlapping driver-only selector":
				desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n", test.change+"\n    management:\n", 1))
			default:
				desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "  ethernets:\n", test.change+"\n  ethernets:\n", 1))
			}
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatal("PlanNetplan() unexpectedly accepted an unproven selector or unsupported topology")
			}
		})
	}
}

func TestPlanNetplanRejectsNoncanonicalAndDefaultRoutes(t *testing.T) {
	for _, destination := range []string{"0.0.0.1/0", "10.0.0.1/16", "::/0", "default", "10.0.0.0/16|host"} {
		t.Run(destination, func(t *testing.T) {
			desired := desiredForTest()
			parts := strings.SplitN(destination, "|", 2)
			destination = parts[0]
			route := "      routes:\n        - to: " + destination + "\n          via: 10.0.0.1"
			if len(parts) == 2 {
				route += "\n          scope: " + parts[1]
			}
			desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "      dhcp4: false", "      dhcp4: false\n"+route, 1))
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatalf("PlanNetplan() accepted noncanonical route destination %q", destination)
			}
		})
	}
}

func TestPlanNetplanRejectsTypedDecodeInformationLoss(t *testing.T) {
	cases := []struct {
		name    string
		find    string
		replace string
	}{
		{
			name:    "MAC selector sequence",
			find:    `macaddress: "02:00:00:00:00:01"`,
			replace: `macaddress: ["02:00:00:00:00:01"]`,
		},
		{
			name:    "null route entry",
			find:    "      dhcp4: false",
			replace: "      dhcp4: false\n      routes: [null]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := desiredForTest()
			desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, tc.find, tc.replace, 1))
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatal("PlanNetplan() accepted selected Netplan syntax whose semantics were lost by typed decoding")
			}
		})
	}
}

func TestPlanNetplanRejectsComplexSelectedAddressMapping(t *testing.T) {
	desired := desiredForTest()
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "      dhcp4: false", "      dhcp4: false\n      addresses:\n        - 10.0.0.2/29:\n            label: data", 1))
	if _, err := PlanNetplan(desired); err == nil {
		t.Fatal("PlanNetplan() accepted an unsupported complex selected-interface address entry")
	}
}

func TestPlanNetplanPreservesUnrelatedNameMatchedInterface(t *testing.T) {
	desired := desiredForTest()
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n",
		"    unrelated-name-match:\n      match:\n        name: enp9s0\n      set-name: enp9s0\n      dhcp4: true\n    management:\n", 1))
	if _, err := PlanNetplan(desired); err != nil {
		t.Fatalf("unrelated name-matched interface blocked the selected PF plan: %v", err)
	}
	desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n",
		"    unrelated-driver-match:\n      match:\n        macaddress: \"02:00:00:00:00:02\"\n        driver: mlx5_core\n      set-name: ens2f0\n      dhcp4: true\n    management:\n", 1))
	if _, err := PlanNetplan(desired); err != nil {
		t.Fatalf("driver selector with a provably unrelated exact MAC blocked the selected PF plan: %v", err)
	}
}

func TestPlanNetplanRejectsUnsafeOrConflictingInputs(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Desired)
	}{
		{"glob MAC selector", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "02:00:00:00:00:01", "02:00:00:00:00:*", 1))
		}},
		{"wrong hardware binding", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "02:00:00:00:00:01", "02:00:00:00:00:02", 1))
		}},
		{"duplicate MAC definition", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n", "    duplicate:\n      match:\n        macaddress: \"02:00:00:00:00:01\"\n      set-name: ens5f0np0\n      dhcp4: false\n    management:\n", 1))
		}},
		{"duplicate interface name", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n", "    duplicate:\n      match:\n        macaddress: \"02:00:00:00:00:02\"\n      set-name: ens5f0np0\n      dhcp4: false\n    management:\n", 1))
		}},
		{"name selector collides with selected interface", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "    management:\n",
				"    unrelated-name-match:\n      match:\n        name: ens5f0np0\n      set-name: ens5f0np0\n      dhcp4: true\n    management:\n", 1))
		}},
		{"DHCPv4 enabled", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: true", 1))
		}},
		{"address conflict", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: false\n      addresses: [10.0.0.3/29]", 1))
		}},
		{"default route on selected PF", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: false\n      routes:\n        - to: default\n          via: 10.0.0.1", 1))
		}},
		{"conflicting workload route", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: false\n      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.6", 1))
		}},
		{"more specific workload route", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: false\n      routes:\n        - to: 10.0.1.0/24\n          via: 10.0.0.1", 1))
		}},
		{"non-main overlapping route", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "dhcp4: false", "dhcp4: false\n      routes:\n        - to: 10.0.0.0/16\n          via: 10.0.0.1\n          table: 100", 1))
		}},
		{"bridge membership", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "  ethernets:\n", "  bridges:\n    br0:\n      interfaces: [data-pf0]\n  ethernets:\n", 1))
		}},
		{"unsupported renderer", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "renderer: networkd", "renderer: NetworkManager", 1))
		}},
		{"missing network version", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "  version: 2\n", "", 1))
		}},
		{"unsupported network version", func(d *Desired) {
			d.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "  version: 2\n", "  version: 1\n", 1))
		}},
		{"invalid host address", func(d *Desired) { d.HostAddressPrefix = "10.0.0.0/29" }},
		{"gateway outside lease", func(d *Desired) { d.Gateway = "10.0.0.9" }},
		{"invalid workload network", func(d *Desired) { d.WorkloadCIDR = "10.0.0.1/16" }},
		{"default workload network", func(d *Desired) { d.WorkloadCIDR = "0.0.0.0/0" }},
		{"lease outside workload network", func(d *Desired) { d.WorkloadCIDR = "192.0.2.0/24" }},
		{"multicast permanent MAC", func(d *Desired) { d.PermanentMAC = "03:00:00:00:00:01" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := desiredForTest()
			test.change(&desired)
			if _, err := PlanNetplan(desired); err == nil {
				t.Fatal("PlanNetplan() unexpectedly accepted unsafe input")
			}
		})
	}
}

func mergeYAML(t *testing.T, original, fragment []byte) map[string]any {
	t.Helper()
	var base, overlay map[string]any
	if err := yaml.Unmarshal(original, &base); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(fragment, &overlay); err != nil {
		t.Fatal(err)
	}
	mergeMap(base, overlay)
	return base
}

func mergeMap(base, overlay map[string]any) {
	for key, value := range overlay {
		if next, ok := value.(map[string]any); ok {
			if current, ok := base[key].(map[string]any); ok {
				mergeMap(current, next)
				continue
			}
			base[key] = next
			continue
		}
		if next, ok := value.([]any); ok {
			if current, ok := base[key].([]any); ok {
				base[key] = append(current, next...)
				continue
			}
		}
		base[key] = value
	}
}

func TestPlanNetplanPreservesSelectedOptionalValue(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{name: "omitted", value: "", want: false}, {name: "explicit false", value: "false", want: false},
		{name: "explicit true", value: "true", want: true}, {name: "null default", value: "null", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := desiredForTest()
			if tc.value != "" {
				desired.MergedNetplanYAML = []byte(strings.Replace(netplanFixture, "      dhcp4: false\n", "      dhcp4: false\n      optional: "+tc.value+"\n", 1))
			}
			original := append([]byte(nil), desired.MergedNetplanYAML...)
			plan, err := PlanNetplan(desired)
			if err != nil || plan.WaitOnlineOptional != tc.want {
				t.Fatalf("optional=%v want=%v err=%v", plan.WaitOnlineOptional, tc.want, err)
			}
			selected, err := InspectNetplanInterface(desired.InterfaceName, desired.PermanentMAC, desired.MergedNetplanYAML)
			logicalID, idErr := NetplanInterfaceID(desired.InterfaceName, desired.PermanentMAC, desired.MergedNetplanYAML)
			if err != nil || idErr != nil || selected.LogicalID != logicalID || selected.WaitOnlineOptional != tc.want {
				t.Fatalf("lease-free inspection differs: %+v %v %v", selected, err, idErr)
			}
			if !bytes.Equal(original, desired.MergedNetplanYAML) || strings.Contains(string(plan.DesiredFragment), "optional:") {
				t.Fatal("optional source was rewritten")
			}
		})
	}
}
