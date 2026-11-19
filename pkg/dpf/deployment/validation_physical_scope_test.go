// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"gopkg.in/yaml.v3"
)

const physicalScopeSource = `
dpf: {useCase: hbn-bgp}
management:
  kubeconfigRef: {file: management-config}
credentials:
  bmc: {username: bmc-reader, passwordRef: {file: bmc-password}}
  host: {username: host-reader, passwordRef: {file: host-password}}
  leaf: {username: fabric-reader, passwordRef: {file: leaf-password}}
discovery:
  dpus:
    - bmcEndpoint: https://192.0.2.20
      credentialRef: bmc
      tls: {caFile: bmc-ca.pem}
  hosts:
    - ssh: {address: host.example.test, knownHostsFile: host-known-hosts}
      credentialRef: host
  switches:
    - name: leaf-a
      ssh: {address: leaf.example.test, knownHostsFile: leaf-known-hosts}
      credentialRef: leaf
      ports:
        - {name: swp1, dpuSerial: SERIAL-A, dpuInterface: p0}
        - {name: swp2, dpuSerial: SERIAL-A, dpuInterface: p1}
`

var physicalScopeBaselineCA struct {
	sync.Mutex
	pem []byte
}

// Baseline trust is the same fixture input, copied into each test's own files.
// Trust replacement scenarios still generate independent certificate bytes.
func physicalScopeCAPEM(t *testing.T) []byte {
	t.Helper()
	physicalScopeBaselineCA.Lock()
	defer physicalScopeBaselineCA.Unlock()
	if physicalScopeBaselineCA.pem == nil {
		physicalScopeBaselineCA.pem = softwareTestCAPEM(t)
	}
	return bytes.Clone(physicalScopeBaselineCA.pem)
}

func physicalScopeFixture(t *testing.T) (*managementDesired, config.Input) {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"bmc-password": []byte("test-password"), "host-password": []byte("test-password"), "leaf-password": []byte("test-password"),
		"bmc-ca.pem":       physicalScopeCAPEM(t),
		"host-known-hosts": []byte("host.example.test ssh-ed25519 AAAA\n"),
		"leaf-known-hosts": []byte("leaf.example.test ssh-ed25519 AAAA\n"),
	} {
		writePhysicalScopeFile(t, dir, name, data)
	}
	path := filepath.Join(dir, "site.yaml")
	input, err := config.Decode([]byte(physicalScopeSource), path)
	if err != nil {
		t.Fatal(err)
	}
	initial, issues := resolvePhysicalScope(t, input)
	if len(issues) != 0 || len(initial.Targets) != 2 || len(initial.Switches) != 1 {
		t.Fatal("initial access fixture did not fully resolve")
	}
	sw := initial.Switches[0]
	saved := &inventory.Inventory{
		FormatVersion: inventory.FormatVersion,
		Collection:    inventory.Collection{Mode: initial.Mode},
		HBN: &inventory.HBNInventory{
			Namespace: initial.DPFNamespace, Profile: *initial.HBN,
			Switches: []inventory.FabricSwitch{{Name: sw.Name, Endpoint: sw.Endpoint, Platform: sw.Platform, Ports: sw.Ports}},
		},
		DPUs: []inventory.Device{{
			ID: "dpu-identity", RecordKey: "dpu-record", InputRef: "/discovery/dpus/0", ObservationState: "observed",
			SerialNumber: "SERIAL-A", BMCEndpoint: "https://192.0.2.20",
		}},
		Hosts: []inventory.Device{{
			ID: "host-identity", RecordKey: "host-record", InputRef: "/discovery/hosts/0", ObservationState: "observed",
			SSHEndpoint: "ssh://host.example.test:22", Adapters: []inventory.Adapter{{Key: "associated-adapter", SerialNumber: "SERIAL-A"}},
		}},
		Associations: []inventory.Association{{DPUID: "dpu-identity", HostID: "host-identity", HostAdapterKey: "associated-adapter", SerialNumber: "SERIAL-A"}},
	}
	saved.ScopeDigest = physicalScopeTestDigest(initial, saved)
	inventoryYAML, err := yaml.Marshal(map[string]*inventory.Inventory{"inventory": saved})
	if err != nil {
		t.Fatal(err)
	}
	source := append([]byte(physicalScopeSource), inventoryYAML...)
	input, err = config.Decode(source, path)
	if err != nil {
		t.Fatal(err)
	}
	inputs := preflightGenerationInputs(t)
	inputs.SourceConfig = source
	retained, err := generateFixtureBundle(inputs)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareValidationFixtureDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	return desired, input
}

func physicalScopeTestDigest(request config.Request, inv *inventory.Inventory) string {
	scopes := make([]inventory.ScopeEndpoint, 0, len(request.Targets)+len(request.Switches))
	for _, target := range request.Targets {
		scopes = append(scopes, config.Scope(target))
	}
	for _, sw := range request.Switches {
		scopes = append(scopes, config.Scope(config.Target{Role: "switches", Endpoint: sw.Endpoint, SSH: sw.Access}))
	}
	slices.SortFunc(scopes, func(a, b inventory.ScopeEndpoint) int {
		return strings.Compare(a.Role+a.Endpoint+a.SystemURI, b.Role+b.Endpoint+b.SystemURI)
	})
	copyInventory := *inv
	copyInventory.RequestedScope = scopes
	return discovery.ScopeDigest(&copyInventory)
}

func writePhysicalScopeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolvePhysicalScope(t *testing.T, input config.Input) (config.Request, []config.AccessIssue) {
	t.Helper()
	request, issues, err := config.ResolveForValidation(input)
	if err != nil {
		t.Fatal(err)
	}
	return request, issues
}

func TestValidationPhysicalScopeRejectsChangedResolvedTrust(t *testing.T) {
	for _, name := range []string{"host-known-hosts", "leaf-known-hosts", "bmc-ca.pem"} {
		t.Run(name, func(t *testing.T) {
			desired, input := physicalScopeFixture(t)
			initial, issues := resolvePhysicalScope(t, input)
			if len(issues) != 0 {
				t.Fatal("unchanged trust did not fully resolve")
			}
			if err := validateValidationPhysicalScope(desired, initial); err != nil {
				t.Fatalf("unchanged complete scope rejected: %v", err)
			}
			replacement := []byte("# replacement trust bytes\n")
			if name == "bmc-ca.pem" {
				replacement = softwareTestCAPEM(t)
			}
			writePhysicalScopeFile(t, filepath.Dir(input.Path), name, replacement)
			changed, issues := resolvePhysicalScope(t, input)
			if len(issues) != 0 || len(changed.Targets) != 2 || len(changed.Switches) != 1 {
				t.Fatal("changed trust did not fully resolve")
			}
			if err := validateValidationPhysicalScope(desired, changed); err == nil || !strings.Contains(err.Error(), "resolved physical access policy differs") {
				t.Fatalf("changed trust bytes did not invalidate the complete access scope: %v", err)
			}
			current := changed.Input.Site.Inventory
			current.ScopeDigest = physicalScopeTestDigest(changed, current)
			if err := validateValidationPhysicalScope(desired, changed); err == nil {
				t.Fatal("recomputed public digest replaced retained trust")
			}
		})
	}
}

func TestValidationPhysicalScopeRejectsChangedPartialDeclarations(t *testing.T) {
	desired, original := physicalScopeFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(original.Path), "host-password")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*config.Input)
	}{
		{name: "same scope", change: func(*config.Input) {}},
		{name: "unavailable host endpoint", change: func(in *config.Input) { in.Site.Discovery.Hosts[0].SSH.Address = "other.example.test" }},
		{name: "unavailable host privilege", change: func(in *config.Input) {
			sudo := true
			in.Site.Discovery.Hosts[0].SSH.Sudo = &sudo
		}},
		{name: "available BMC TLS", change: func(in *config.Input) {
			insecure := true
			in.Site.Discovery.DPUs[0].TLS = &config.TLS{InsecureSkipVerify: &insecure}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := config.Decode(original.Source, original.Path)
			if err != nil {
				t.Fatal(err)
			}
			test.change(&input)
			request, issues := resolvePhysicalScope(t, input)
			if len(issues) != 1 || issues[0].Role != "hosts" || len(request.Targets) != 1 || len(request.Switches) != 1 {
				t.Fatal("fixture did not keep exactly one unavailable host")
			}
			err = validateValidationPhysicalScope(desired, request)
			if test.name == "same scope" {
				if err != nil {
					t.Fatalf("unchanged partial scope rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "declared physical endpoints or security policy differ") {
				t.Fatalf("changed declaration passed partial access guard: %v", err)
			}
		})
	}
}

func TestValidationPhysicalScopeRejectsUndeclaredResolvedSources(t *testing.T) {
	desired, original := physicalScopeFixture(t)
	request, issues := resolvePhysicalScope(t, original)
	if len(issues) != 0 {
		t.Fatal("complete source fixture did not resolve")
	}
	for _, test := range []struct {
		name   string
		change func(*config.Request)
	}{
		{name: "undeclared endpoint", change: func(r *config.Request) { r.Switches[0].Endpoint = "ssh://other.example.test:22" }},
		{name: "switch name", change: func(r *config.Request) { r.Switches[0].Name = "another-leaf" }},
		{name: "switch platform", change: func(r *config.Request) { r.Switches[0].Platform = "other" }},
		{name: "switch ports", change: func(r *config.Request) { r.Switches[0].Ports[0].Name = "swp9" }},
		{name: "switch sudo", change: func(r *config.Request) { r.Switches[0].Access.Sudo = true }},
		{name: "undeclared BMC system", change: func(r *config.Request) { r.Targets[0].SystemURI = "/redfish/v1/Systems/other" }},
		{name: "BMC trust removed", change: func(r *config.Request) { r.Targets[0].Access = config.Access{} }},
		{name: "mismatched BMC source reference", change: func(r *config.Request) { r.Targets[0].InputRef = "/discovery/dpus/1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, partial := range []bool{false, true} {
				current, err := config.SnapshotRequestForValidation(request)
				if err != nil {
					t.Fatal(err)
				}
				if partial {
					current.Targets = slices.DeleteFunc(current.Targets, func(target config.Target) bool { return target.Role == "hosts" })
				}
				test.change(&current)
				if err := validateValidationPhysicalScope(desired, current); err == nil {
					t.Fatalf("resolved source bypassed declaration with partial=%v", partial)
				}
			}
		})
	}
}

func TestValidationPhysicalScopeKeepsUnavailableSwitchInCoverage(t *testing.T) {
	desired, input := physicalScopeFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(input.Path), "leaf-password")); err != nil {
		t.Fatal(err)
	}
	request, issues := resolvePhysicalScope(t, input)
	if len(request.Switches) != 0 || len(issues) != 1 || issues[0].InputRef != "/discovery/switches/0" {
		t.Fatal("fixture did not lose only the selected switch access")
	}
	if err := validateValidationPhysicalScope(desired, request); err != nil {
		t.Fatalf("unchanged partial scope rejected: %v", err)
	}
	o := &validationObserver{
		desired: desired, request: request, unavailable: issues, mode: validation.ModeFull,
		physical: validationPhysicalReaders{
			bmc: func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
				return inventory.Device{}, nil, nil
			},
			host: func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
				return inventory.Device{}, nil, nil
			},
			fabric: func(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot {
				t.Fatal("unresolved switch access reached the collector")
				return fabric.Snapshot{}
			},
		},
	}
	o.physicalScope, _ = compileValidationPhysicalScope(desired, request)
	checks := o.physicalValidationChecks()
	id := physicalID("switch", "leaf-a")
	if !slices.ContainsFunc(checks, func(check validation.CheckSpec) bool { return check.ID == id }) {
		t.Fatal("unavailable switch vanished from expected coverage")
	}
	recorder := newValidationRecorder(checks, nil)
	_ = o.observePhysical(context.Background(), recorder)
	if got := recorder.outcomes[id]; got.State != validation.Unverified || !strings.Contains(got.Summary, "cannot resolve password file") {
		t.Fatalf("missing switch scope did not retain its unavailable access reason: %+v", got)
	}
}

func TestValidationPhysicalScopePlansSwitchesFromVerifiedDeclarations(t *testing.T) {
	desired, input := physicalScopeFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(input.Path), "leaf-password")); err != nil {
		t.Fatal(err)
	}
	request, issues := resolvePhysicalScope(t, input)
	if len(request.Switches) != 0 || len(issues) != 1 {
		t.Fatal("fixture did not lose only the selected switch access")
	}
	request.Input.Site.Inventory.HBN.Switches[0] = inventory.FabricSwitch{
		Name: "replacement-leaf", Endpoint: "ssh://replacement.example.test:22", Platform: "unsupported",
		Ports: []inventory.SwitchPort{{Name: "swp9", DPUSerial: "SERIAL-A", DPUInterface: "p0"}},
	}
	if err := validateValidationPhysicalScope(desired, request); err != nil {
		t.Fatalf("verified declarations should remain valid despite changed saved switch facts: %v", err)
	}
	o := &validationObserver{desired: desired, request: request, unavailable: issues, mode: validation.ModeFull}
	expected := o.physicalInventory().HBN.Switches
	if len(expected) != 1 || expected[0].Name != "leaf-a" || expected[0].Endpoint != "ssh://leaf.example.test:22" ||
		expected[0].Platform != "cumulus-nvue" || len(expected[0].Ports) != 2 || expected[0].Ports[0].Name != "swp1" {
		t.Fatalf("saved switch facts altered physical coverage: %+v", expected)
	}
}

func validateValidationPhysicalScope(desired *managementDesired, request config.Request) error {
	_, err := compileValidationPhysicalScope(desired, request)
	return err
}

func TestValidationPhysicalScopeChecksTrustDespiteMissingAuthentication(t *testing.T) {
	for _, changedFile := range []string{"host-known-hosts", "leaf-known-hosts", "bmc-ca.pem"} {
		t.Run(changedFile, func(t *testing.T) {
			desired, input := physicalScopeFixture(t)
			dir := filepath.Dir(input.Path)
			if err := os.Remove(filepath.Join(dir, "host-password")); err != nil {
				t.Fatal(err)
			}
			initial, issues := resolvePhysicalScope(t, input)
			scope, err := compileValidationPhysicalScope(desired, initial)
			if err != nil || !scope.accessVerified || len(issues) != 1 {
				t.Fatalf("missing auth lost complete trust proof: %v", err)
			}
			replacement := []byte("# changed known hosts\n")
			if changedFile == "bmc-ca.pem" {
				replacement = softwareTestCAPEM(t)
			}
			writePhysicalScopeFile(t, dir, changedFile, replacement)
			changed, _ := resolvePhysicalScope(t, input)
			if _, err := compileValidationPhysicalScope(desired, changed); err == nil {
				t.Fatal("missing auth hid changed trust")
			}
		})
	}
}

func TestValidationPhysicalScopeUnknownTrustPreventsAllPhysicalReads(t *testing.T) {
	desired, input := physicalScopeFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(input.Path), "host-known-hosts")); err != nil {
		t.Fatal(err)
	}
	request, issues := resolvePhysicalScope(t, input)
	scope, err := compileValidationPhysicalScope(desired, request)
	if err != nil || scope.accessVerified {
		t.Fatalf("unreadable trust scope=%+v err=%v", scope, err)
	}
	o := &validationObserver{desired: desired, request: request, physicalScope: scope, unavailable: issues, mode: validation.ModeFull}
	o.physical = validationPhysicalReaders{
		bmc: func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
			t.Fatal("unproven trust reached BMC")
			return inventory.Device{}, nil, nil
		},
		host: func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
			t.Fatal("unproven trust reached host")
			return inventory.Device{}, nil, nil
		},
		fabric: func(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot {
			t.Fatal("unproven trust reached switch")
			return fabric.Snapshot{}
		},
	}
	recorder := newValidationRecorder(o.physicalValidationChecks(), nil)
	_ = o.observePhysical(context.Background(), recorder)
	if got := recorder.outcomes[physicalID("switch", "leaf-a")]; got.State != validation.Unverified || !strings.Contains(got.Summary, "trust files") {
		t.Fatalf("trust unavailable=%+v", got)
	}
}

func TestValidationPhysicalExpectationsRemainOwnedByRetainedBundle(t *testing.T) {
	desired, input := physicalScopeFixture(t)
	request, _ := resolvePhysicalScope(t, input)
	request.Input.Site.Inventory.Hosts[0].Runtime = &inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "pf0", PermanentMAC: "02:00:00:00:00:99"}}}}
	scope, err := compileValidationPhysicalScope(desired, request)
	if err != nil {
		t.Fatal(err)
	}
	if scope.expected == request.Input.Site.Inventory || scope.expected.Hosts[0].Runtime != nil {
		t.Fatal("caller observations redefined retained physical expectations")
	}
	before := scope.expected.Hosts[0].Adapters[0].SerialNumber
	request.Input.Site.Inventory.Hosts[0].Adapters[0].SerialNumber = "other"
	if scope.expected.Hosts[0].Adapters[0].SerialNumber != before {
		t.Fatal("retained snapshot aliased caller inventory")
	}
}
