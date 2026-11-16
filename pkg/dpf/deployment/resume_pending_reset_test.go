// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/types"
)

func pendingResetStageFixture(t *testing.T, execution *managementExecution) (activationIntegrationFixture, *inventory.Inventory, ownedResumePrerequisites, []inventory.Check) {
	t.Helper()
	f := newActivationIntegrationFixtureFromExecution(t, execution, false)
	host := &f.inventory.Hosts[0]
	host.ObservationState = "observed"
	host.Runtime = &inventory.RuntimeObservation{BootID: operationOldBoot, SSHServerAddress: "192.0.2.10", Hostname: "worker-a"}
	host.Adapters[0].PCIAddress = "0000:21:00.0"
	host.Adapters[0].PCIFunctions = []inventory.PCIFunction{{Address: "0000:21:00.0", Driver: "mlx5_core", Netdevs: []string{"pf0"}}, {Address: "0000:21:00.1", Driver: "mlx5_core", Netdevs: []string{"pf1"}}}
	f.inventory.DPUs[0].ObservationState = "observed"
	f.inventory.HBN = &inventory.HBNInventory{Switches: []inventory.FabricSwitch{{Name: "leaf", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A"}, {Name: "swp2", DPUSerial: "SERIAL-A"}}}}}
	op, store := checkpointFixture(t, f)
	f.prepared.operation = op
	sources, err := tenantOperationSources(f.ctx, f.controlPlane.tenant.Client)
	if err != nil {
		t.Fatal(err)
	}
	op.state.TenantSources = sources
	checkpoint, err := encodeOperation(op.state)
	if err != nil {
		t.Fatal(err)
	}
	op.object.Data["checkpoint.json"] = checkpoint
	store.object = op.object.DeepCopy()
	f.client.resetSafetyError = errors.New("pause before a physical claim")
	_, err = runActivationStage(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
		t.Fatal("pause must stop before reboot submission")
		return ActivationObservation{}, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "pause before a physical claim") || f.client.guardCalls != 0 || len(op.state.Progress) != 1 {
		t.Fatalf("owned pending fixture stop=%v claims=%d", err, f.client.guardCalls)
	}
	type rawInventory inventory.Inventory
	encoded, err := json.Marshal((*rawInventory)(f.inventory))
	if err != nil {
		t.Fatal(err)
	}
	var original inventory.Inventory
	if err = json.Unmarshal(encoded, (*rawInventory)(&original)); err != nil {
		t.Fatal(err)
	}
	host = &f.inventory.Hosts[0]
	host.Runtime = &inventory.RuntimeObservation{BootID: operationOldBoot, SSHServerAddress: "192.0.2.10", Hostname: "worker-a", Interfaces: []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{"192.0.2.10/24"}}}, Routes: []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 2}, {Destination: "::/0", InterfaceIndex: 2}}, Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "mgmt0", PCIAddress: "0000:35:00.0", PhysicalPFRoots: []string{"0000:35:00.0"}}}}, Observations: map[string]inventory.Observation{"interfaces": {State: "observed"}, "network-links": {State: "observed"}, "network-link-details": {State: "observed"}, "routes": {State: "observed"}, "host-management-roots": {State: "observed"}}}
	for i := range host.Adapters[0].PCIFunctions {
		pf := &host.Adapters[0].PCIFunctions[i]
		pf.Netdevs = []string{}
		pf.NetdevsAbsent = true
		host.Runtime.Observations["host-pf:"+pf.Address+":driver"] = inventory.Observation{State: "observed"}
		host.Runtime.Observations["host-pf:"+pf.Address+":netdevs"] = inventory.Observation{State: "observed", ReasonCode: "HOST_PF_NETDEV_ABSENT"}
	}
	proof, err := verifyOwnedResumeResources(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal)
	if err != nil {
		t.Fatal(err)
	}
	checks := []inventory.Check{}
	add := func(id, subject, code string) {
		checks = append(checks, inventory.Check{ID: "dpf.hbn." + id, Subject: subject, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: code, Message: "current observation unavailable"})
	}
	add("host-pf", "SERIAL-A @ worker-a / 0000:21:00.0", "HOST_PF_NETDEV_ABSENT")
	add("host-pf", "SERIAL-A @ worker-a / 0000:21:00.1", "HOST_PF_NETDEV_ABSENT")
	add("host-management-separation", "SERIAL-A @ worker-a", "HOST_PF_NETDEV_UNAVAILABLE")
	add("host-network-backend", "SERIAL-A @ worker-a", "HOST_NETWORK_IDENTITY_UNAVAILABLE")
	add("cabling", "leaf/swp1", "HOST_LLDP_PORT_MAPPING_UNAVAILABLE")
	add("cabling", "leaf/swp2", "HOST_LLDP_PORT_MAPPING_UNAVAILABLE")
	checks = append(checks, inventory.Check{ID: "dpf.hbn.management.enrollments", Subject: "selected DPUs", Vantage: "executor", RequiredPhase: "deploy", State: "failed", ReasonCode: "DPU_ALREADY_ENROLLED", Message: "initial vacancy"}, inventory.Check{ID: "dpf.hbn.management.tenant-network", Subject: "tenant " + f.controlPlane.tenant.Client.expected.Name + " network selection", Vantage: "management-cluster", RequiredPhase: "deploy", State: "unknown", ReasonCode: "TENANT_EXISTING_NETWORK_UNVERIFIED", Message: "initial endpoint vacancy"})
	checks = append(checks, discovery.HostManagementPhysicalCheck(f.inventory, "SERIAL-A"))
	return f, &original, proof, checks
}

func TestPendingResetAssessmentIsOwnedExactAndBeforeActivationOnly(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name  string
		phase PrerequisitePhase
		edit  func(*activationIntegrationFixture, *ownedResumePrerequisites, []inventory.Check)
		pass  bool
	}{
		{"exact owned transition", BeforeActivation, func(*activationIntegrationFixture, *ownedResumePrerequisites, []inventory.Check) {}, true},
		{"After remains strict", AfterActivation, func(*activationIntegrationFixture, *ownedResumePrerequisites, []inventory.Check) {}, false},
		{"no release history", BeforeActivation, func(f *activationIntegrationFixture, _ *ownedResumePrerequisites, _ []inventory.Check) {
			f.prepared.operation.state.Progress = map[string]operationProgress{}
		}, false},
		{"node replaced", BeforeActivation, func(f *activationIntegrationFixture, p *ownedResumePrerequisites, _ []inventory.Check) {
			f.client.base.resources["DPUNode/"+execution.operatorNamespace+"/"+p.selected[0].nodeName].SetUID(types.UID("replacement"))
		}, false},
		{"request completed", BeforeActivation, func(f *activationIntegrationFixture, p *ownedResumePrerequisites, _ []inventory.Check) {
			node := f.client.base.resources["DPUNode/"+execution.operatorNamespace+"/"+p.selected[0].nodeName]
			node.SetAnnotations(map[string]string{})
		}, false},
		{"unknown boot", BeforeActivation, func(f *activationIntegrationFixture, p *ownedResumePrerequisites, _ []inventory.Check) {
			f.inventory.Hosts[0].Runtime.BootID = ""
			p.selected[0].host = f.inventory.Hosts[0]
		}, false},
		{"same-card management VF", BeforeActivation, func(f *activationIntegrationFixture, p *ownedResumePrerequisites, _ []inventory.Check) {
			f.inventory.Hosts[0].Runtime.Network.Links[0].PCIAddress = "0000:43:00.0"
			f.inventory.Hosts[0].Runtime.Network.Links[0].PhysicalPFRoots = []string{"0000:21:00.0"}
			p.selected[0].host = f.inventory.Hosts[0]
		}, false},
		{"raw SSH failure not absent", BeforeActivation, func(f *activationIntegrationFixture, p *ownedResumePrerequisites, c []inventory.Check) {
			for _, pf := range f.inventory.Hosts[0].Adapters[0].PCIFunctions {
				f.inventory.Hosts[0].Runtime.Observations["host-pf:"+pf.Address+":netdevs"] = inventory.Observation{State: "unavailable", ReasonCode: "SSH_COMMAND_FAILED"}
			}
			p.selected[0].host = f.inventory.Hosts[0]
			c[0].ReasonCode = "SSH_COMMAND_FAILED"
			c[1].ReasonCode = "SSH_COMMAND_FAILED"
		}, false},
		{"foreign cabling port", BeforeActivation, func(_ *activationIntegrationFixture, _ *ownedResumePrerequisites, c []inventory.Check) {
			c[4].Subject = "foreign/swp99"
		}, false},
		{"conflicting LLDP", BeforeActivation, func(_ *activationIntegrationFixture, _ *ownedResumePrerequisites, c []inventory.Check) {
			c[4].State = "failed"
			c[4].ReasonCode = "CABLING_CONFLICT"
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, original, proof, raw := pendingResetStageFixture(t, execution)
			test.edit(&f, &proof, raw)
			before, _ := json.Marshal(raw)
			checks := proof.assessInitialChecks(raw, f.controlPlane.tenant.Client.expected.Name)
			patches, claims := len(f.client.patches), f.client.guardCalls
			got, err := assessPendingResetHosts(f.ctx, f.client, f.prepared, proof, original, f.inventory, checks, test.phase)
			all := err == nil
			for _, check := range got {
				all = all && check.State == "passed"
			}
			if all != test.pass {
				t.Fatalf("checks=%+v err=%v", got, err)
			}
			after, _ := json.Marshal(raw)
			if string(before) != string(after) {
				t.Fatal("raw checks mutated")
			}
			if len(f.client.patches) != patches || f.client.guardCalls != claims {
				t.Fatal("prerequisite proof performed an activation effect")
			}
			if test.phase == AfterActivation && !reflect.DeepEqual(got, checks) {
				t.Fatal("AfterActivation changed pending predicates")
			}
		})
	}
}
