// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

func cloneValidationDevice(t *testing.T, device inventory.Device) inventory.Device {
	t.Helper()
	raw, err := json.Marshal(device)
	if err != nil {
		t.Fatal(err)
	}
	var result inventory.Device
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestValidationPhysicalUnavailableFactsDoNotProveReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		edit       func(*inventory.Device)
	}{
		{"missing permanent MAC", "host", func(d *inventory.Device) {
			for i := range d.Runtime.Network.Links {
				if d.Runtime.Network.Links[i].PhysicalPortName == "p0" {
					d.Runtime.Network.Links[i].PermanentMAC = ""
				}
			}
		}},
		{"unavailable network", "host", func(d *inventory.Device) { d.Runtime.Network = nil }},
		{"unavailable adapter serial", "host", func(d *inventory.Device) {
			d.Adapters[0].SerialNumber, d.Adapters[0].IdentityState = "", "unknown"
		}},
		{"unavailable PSID", "dpu", func(d *inventory.Device) {
			d.PSID = ""
			d.Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: "PSID_INVALID"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, bmcs, hosts := physicalValidationFixture(t)
			current := hosts
			if tc.kind == "dpu" {
				current = bmcs
			}
			for endpoint, device := range current {
				if device.SerialNumber == "SERIAL-A" || len(device.Adapters) > 0 && device.Adapters[0].SerialNumber == "SERIAL-A" {
					// Detach nested observation storage from the retained expectation.
					device = cloneValidationDevice(t, device)
					tc.edit(&device)
					current[endpoint] = device
				}
			}
			r := newValidationRecorder(o.checks, nil)
			_, err := o.observePhysicalDevices(context.Background(), r, true)
			if err != nil {
				t.Fatalf("unavailable current fact poisoned cohort: %v", err)
			}
			got := r.outcomes[physicalID(tc.kind, "SERIAL-A")]
			if got.State != validation.Unverified || got.Reason != validation.ReasonUnavailable {
				t.Fatalf("unavailable fact was not classified accurately: %+v", got)
			}
			if independent := r.outcomes[physicalID("dpu", "SERIAL-B")]; independent.State != validation.Passed {
				t.Fatalf("independent DPU observation was lost: %+v", independent)
			}
		})
	}
}

func TestValidationPhysicalKnownReplacementStillPoisonsCohort(t *testing.T) {
	prepared := preparePhysicalValidationFixture(t)
	for _, kind := range []string{"dpu", "host", "bmc-during-read", "host-during-read", "psid-during-read", "mac-binding-during-read", "changed-mac-with-partial-links", "renamed-interface-without-mac"} {
		t.Run(kind, func(t *testing.T) {
			o, bmcs, hosts := physicalValidationFixtureFromPreparation(t, prepared)
			switch kind {
			case "dpu":
				for endpoint, d := range bmcs {
					d.PSID = "MT_0000009999"
					bmcs[endpoint] = d
				}
			case "host":
				for endpoint, d := range hosts {
					d = cloneValidationDevice(t, d)
					d.Adapters[0].SerialNumber = "REPLACEMENT"
					d.Runtime.Network = nil // Missing optional facts cannot hide known replacement.
					hosts[endpoint] = d
				}
			case "bmc-during-read":
				o.physical.bmc = func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
					return inventory.Device{}, nil, &redfish.Error{Code: "IDENTITY_CHANGED"}
				}
			case "host-during-read":
				o.physical.host = func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
					return inventory.Device{}, nil, &sshprobe.Error{Code: "INVENTORY_CHANGED"}
				}
			case "psid-during-read":
				for endpoint, d := range bmcs {
					d = cloneValidationDevice(t, d)
					d.PSID = ""
					d.Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: "PSID_CHANGED"}
					bmcs[endpoint] = d
				}
			case "mac-binding-during-read", "changed-mac-with-partial-links", "renamed-interface-without-mac":
				for endpoint, d := range hosts {
					d = cloneValidationDevice(t, d)
					for i := range d.Runtime.Network.Links {
						link := &d.Runtime.Network.Links[i]
						switch link.PhysicalPortName {
						case "p0":
							switch kind {
							case "renamed-interface-without-mac":
								link.Name = "renamed-p0"
								link.PermanentMAC = ""
								d.Runtime.Observations["host-interface:"+link.Name+":permanent-mac"] = inventory.Observation{State: "unavailable", ReasonCode: "INVENTORY_CHANGED"}
							case "mac-binding-during-read":
								link.PermanentMAC = ""
								d.Runtime.Observations["host-interface:"+link.Name+":permanent-mac"] = inventory.Observation{State: "unavailable", ReasonCode: "INVENTORY_CHANGED"}
							default:
								link.PermanentMAC = "02:aa:bb:cc:dd:ee"
							}
						case "p1":
							link.PhysicalPortName = ""
						}
					}
					hosts[endpoint] = d
				}
			}
			r := newValidationRecorder(o.checks, nil)
			_, err := o.observePhysicalDevices(context.Background(), r, true)
			var fatal *validation.ObservationError
			if !errors.As(err, &fatal) || fatal.Category != validation.FatalIdentity {
				t.Fatalf("known identity replacement did not poison cohort: %v", err)
			}
		})
	}
}

func TestValidationPhysicalAllowsQualifiedPSIDFallback(t *testing.T) {
	for _, code := range []string{"PSID_NOT_EXPOSED", "PSID_READER_UNSUPPORTED"} {
		t.Run(code, func(t *testing.T) {
			o, bmcs, _ := physicalValidationFixture(t)
			for endpoint, d := range bmcs {
				d = cloneValidationDevice(t, d)
				d.PSID = ""
				d.Observations["psid"] = inventory.Observation{State: "unknown", ReasonCode: code}
				bmcs[endpoint] = d
			}
			r := newValidationRecorder(o.checks, nil)
			_, err := o.observePhysicalDevices(context.Background(), r, true)
			if err != nil || r.outcomes[physicalID("dpu", "SERIAL-A")].State != validation.Passed {
				t.Fatalf("qualified non-Redfish PSID fallback rejected: %v %+v", err, r.outcomes[physicalID("dpu", "SERIAL-A")])
			}
		})
	}
}

func preparePhysicalValidationFixture(t *testing.T) validationFixturePreparation {
	t.Helper()
	return prepareValidationFixture(t, []string{"SERIAL-A", "SERIAL-B"})
}

func physicalValidationFixture(t *testing.T) (*validationObserver, map[string]inventory.Device, map[string]inventory.Device) {
	t.Helper()
	return physicalValidationFixtureFromPreparation(t, preparePhysicalValidationFixture(t))
}

func physicalValidationFixtureFromPreparation(t *testing.T, prepared validationFixturePreparation) (*validationObserver, map[string]inventory.Device, map[string]inventory.Device) {
	t.Helper()
	f := newActivationIntegrationFixtureFromExecution(t, prepared.execution, false)
	enrichHostNetworkFixtureIdentities(t, &f)
	desired := prepared.desired
	f.request.Input.Site.Inventory = f.inventory
	bmcs, hosts := map[string]inventory.Device{}, map[string]inventory.Device{}
	for i := range f.inventory.DPUs {
		d := &f.inventory.DPUs[i]
		d.BMCEndpoint = "https://" + desired.plan.Intent().Devices[i].BMCIP
		d.RecordKey = "dpu-source-" + d.SerialNumber
		d.ObservationState = "observed"
		d.PSID = "MT_0000001774"
		d.Model = "B4240"
		d.Observations = map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}, "psid": {State: "observed", Values: []string{d.PSID}}}
		bmcs[d.BMCEndpoint] = *d
	}
	for i := range f.inventory.Hosts {
		h := &f.inventory.Hosts[i]
		h.ObservationState = "observed"
		h.Runtime.VirtualMachine = new(bool)
		h.Runtime.SSHServerAddress = "192.0.2.10"
		h.Runtime.Interfaces = []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{"192.0.2.10/24"}}}
		h.Runtime.Routes = []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 2}}
		h.Runtime.Network.Links = append(h.Runtime.Network.Links, inventory.NetworkLink{Name: "mgmt0", PhysicalPFRoots: []string{"0000:35:00.0"}})
		h.Runtime.Observations = map[string]inventory.Observation{"interfaces": {State: "observed"}, "routes": {State: "observed"}, "network-links": {State: "observed"}, "host-management-roots": {State: "observed"}}
		for j := range h.Adapters[0].PCIFunctions {
			pf := &h.Adapters[0].PCIFunctions[j]
			group := 10 + j
			pf.IOMMUGroup = &group
			for _, stage := range []string{"driver", "netdevs"} {
				h.Runtime.Observations["host-pf:"+pf.Address+":"+stage] = inventory.Observation{State: "observed"}
			}
			h.Runtime.Observations["host-pf:"+pf.Address+":iommu-group"] = inventory.Observation{State: "observed", Values: []string{fmt.Sprintf("group=%d", group)}}
		}
		h.Runtime.BootID = fmt.Sprintf("12345678-1234-1234-1234-123456789ab%d", i)
		hosts[h.SSHEndpoint] = *h
	}
	o := &validationObserver{physicalScope: validationPhysicalScope{expected: f.inventory, accessVerified: true}, desired: desired, request: f.request, mode: validation.ModeFull}
	o.physical = validationPhysicalReaders{
		bmc: func(_ context.Context, target config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
			d, ok := bmcs[target.Endpoint]
			if !ok {
				return d, nil, errors.New("unselected BMC")
			}
			return d, nil, nil
		},
		host: func(_ context.Context, target config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
			h, ok := hosts[target.Endpoint]
			if !ok {
				return h, nil, errors.New("unselected host")
			}
			return h, nil, nil
		},
	}
	o.checks = o.physicalValidationChecks()
	return o, bmcs, hosts
}

func TestValidationPhysicalChecksUseCurrentIdentityAndAdministrator(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*validationObserver, map[string]inventory.Device, map[string]inventory.Device)
		kind string
		want validation.State
	}{
		{"matching identities", func(*validationObserver, map[string]inventory.Device, map[string]inventory.Device) {}, "dpu", validation.Passed},
		{"replaced BMC", func(_ *validationObserver, b map[string]inventory.Device, _ map[string]inventory.Device) {
			for k, d := range b {
				if d.SerialNumber == "SERIAL-A" {
					d.ID = "replacement"
					b[k] = d
				}
			}
		}, "dpu", validation.Failed},
		{"non administrator", func(_ *validationObserver, b map[string]inventory.Device, _ map[string]inventory.Device) {
			for k, d := range b {
				if d.SerialNumber == "SERIAL-A" {
					d.Observations = map[string]inventory.Observation{"psid": {State: "observed", Values: []string{d.PSID}}}
					b[k] = d
				}
			}
		}, "administrator", validation.Failed},
		{"changed host UUID", func(_ *validationObserver, _ map[string]inventory.Device, h map[string]inventory.Device) {
			for k, d := range h {
				if d.Adapters[0].SerialNumber == "SERIAL-A" {
					d.SystemUUID = "12345678-1234-1234-1234-123456789999"
					h[k] = d
				}
			}
		}, "host", validation.Failed},
		{"missing host credential", func(o *validationObserver, _ map[string]inventory.Device, _ map[string]inventory.Device) {
			for i := range o.request.Targets {
				if o.request.Targets[i].Endpoint == "ssh://host-a.example.test:22" {
					o.request.Targets[i].SSH = nil
				}
			}
		}, "host", validation.Unverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, b, h := physicalValidationFixture(t)
			tc.edit(o, b, h)
			r := newValidationRecorder(o.checks, nil)
			_, _ = o.observePhysicalDevices(context.Background(), r, true)
			if got := r.outcomes[physicalID(tc.kind, "SERIAL-A")]; got.State != tc.want {
				t.Fatalf("selected observation=%+v want %s", got, tc.want)
			}
			if got := r.outcomes[physicalID("dpu", "SERIAL-B")]; got.State != validation.Passed {
				t.Fatalf("independent second device was lost: %+v", got)
			}
		})
	}
}

func TestValidationPhysicalPhaseFencesRejectUnavailableSources(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	fence, err := o.openValidationPhysicalFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o.physical.bmc = func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		return inventory.Device{}, nil, fmt.Errorf("private transport body")
	}
	err = fence(context.Background())
	var category *validation.ObservationError
	if !errors.As(err, &category) || category.Category != validation.FatalAccess || strings.Contains(err.Error(), "private") {
		t.Fatalf("unavailable source passed or leaked: %v", err)
	}
}

func TestValidationPhysicalPhaseFenceRejectsInterveningReboot(t *testing.T) {
	o, _, hosts := physicalValidationFixture(t)
	fence, err := o.openValidationPhysicalFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for key, host := range hosts {
		copyRuntime := *host.Runtime
		copyRuntime.BootID = "12345678-1234-1234-1234-123456789999"
		host.Runtime = &copyRuntime
		hosts[key] = host
		break
	}
	var failure *validation.ObservationError
	if err := fence(context.Background()); !errors.As(err, &failure) || failure.Category != validation.FatalIdentity {
		t.Fatalf("host reboot did not invalidate runtime: %v", err)
	}
}

func TestValidationResourceModeDoesNotReadPhysicalSources(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	o.mode = validation.ModeResources
	o.physical.bmc = func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		t.Fatal("resource mode accessed BMC")
		return inventory.Device{}, nil, nil
	}
	o.physical.host = o.physical.bmc
	r := newValidationRecorder(o.checks, nil)
	if err := o.observePhysical(context.Background(), r); err != nil || len(r.outcomes) != 0 {
		t.Fatalf("resource mode observed physical source: %v", err)
	}
}

func TestValidationDeclaredScopeDoesNotDependOnCredentialAvailability(t *testing.T) {
	desired, input := physicalScopeFixture(t)
	request := config.Request{Input: input}
	if err := validateValidationPhysicalScope(desired, request); err != nil {
		t.Fatalf("unavailable material rejected unchanged scope: %v", err)
	}
	request.Input.Site.Discovery.Hosts[0].SSH.Address = "other.example.test"
	if err := validateValidationPhysicalScope(desired, request); err == nil {
		t.Fatal("missing credentials hid changed host endpoint")
	}
	request.Input.Site.Discovery.Hosts[0].SSH.Address = "host.example.test"
	insecure := true
	request.Input.Site.Discovery.DPUs[0].TLS = &config.TLS{InsecureSkipVerify: &insecure}
	if err := validateValidationPhysicalScope(desired, request); err == nil {
		t.Fatal("missing credentials hid degraded TLS policy")
	}
}

func TestValidationPhysicalCallerMACCannotRedefineExpectedHardware(t *testing.T) {
	o, _, hosts := physicalValidationFixture(t)
	request, err := config.SnapshotRequestForValidation(o.request)
	if err != nil {
		t.Fatal(err)
	}
	o.request = request
	expected := o.physicalScope.expected.Hosts[0]
	changedMAC := "02:00:00:00:00:99"
	o.request.Input.Site.Inventory.Hosts[0].Runtime.Network.Links[0].PermanentMAC = changedMAC
	current := hosts[expected.SSHEndpoint]
	runtimeCopy := *current.Runtime
	network := *runtimeCopy.Network
	network.Links = append([]inventory.NetworkLink(nil), network.Links...)
	network.Links[0].PermanentMAC = changedMAC
	runtimeCopy.Network = &network
	current.Runtime = &runtimeCopy
	hosts[expected.SSHEndpoint] = current
	recorder := newValidationRecorder(o.physicalValidationChecks(), nil)
	_, _ = o.observePhysicalDevices(context.Background(), recorder, true)
	serial := expected.Adapters[0].SerialNumber
	if got := recorder.outcomes[physicalID("host", serial)]; got.State != validation.Failed || got.Reason != validation.ReasonIdentityDrift {
		t.Fatalf("caller and live MAC replaced retained expectation: %+v", got)
	}
}

func TestValidationPhysicalRuntimeFenceRequiresCurrentHostPrerequisites(t *testing.T) {
	o, _, hosts := physicalValidationFixture(t)
	fence, err := o.openValidationPhysicalFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for endpoint, host := range hosts {
		runtimeCopy := *host.Runtime
		runtimeCopy.Observations = map[string]inventory.Observation{}
		for key, value := range host.Runtime.Observations {
			runtimeCopy.Observations[key] = value
		}
		stage := "host-pf:" + host.Adapters[0].PCIFunctions[0].Address + ":iommu-group"
		runtimeCopy.Observations[stage] = inventory.Observation{State: "unavailable", ReasonCode: "IOMMU_GROUP_ABSENT"}
		host.Runtime = &runtimeCopy
		hosts[endpoint] = host
		break
	}
	if err := fence(context.Background()); err == nil {
		t.Fatal("runtime fence accepted lost host IOMMU prerequisite")
	}
}
