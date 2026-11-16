// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestRunHostWorkloadNetworkStageUsesActualActivationAndLeaseChecks(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixture(t)
	for _, test := range []struct {
		name           string
		mode           string
		wantReason     string
		wantEffects    int
		wantResults    int
		wantReverified bool
	}{
		{name: "configured and fenced", mode: "success", wantEffects: 1, wantResults: 1, wantReverified: true},
		{name: "lease transport unavailable", mode: "unavailable", wantReason: "HOST_NETWORK_LEASE_UNAVAILABLE", wantEffects: 0, wantResults: 0},
		{name: "wrong tenant node binding", mode: "wrong-node", wantReason: "HOST_NETWORK_LEASE_UNAVAILABLE", wantEffects: 0, wantResults: 0},
		{name: "uncertain host acknowledgement", mode: "unknown-effect", wantReason: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", wantEffects: 1, wantResults: 1},
		{name: "incomplete host verification", mode: "incomplete-effect", wantReason: "HOST_NETWORK_RECEIPT_INCOMPLETE", wantEffects: 1, wantResults: 1},
		{name: "pool changed during host effect", mode: "changed-pool-rv", wantReason: "HOST_NETWORK_POST_FENCE_UNCERTAIN", wantEffects: 1, wantResults: 1},
		{name: "deployment replaced after host effect", mode: "changed-deployment", wantReason: "HOST_NETWORK_POST_FENCE_UNCERTAIN", wantEffects: 1, wantResults: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := testJournal(fixture.prepared.execution)
			deployment := fixture.client.deployment.DeepCopy()
			t.Cleanup(func() { fixture.client.deployment = deployment })
			reads, effects := 0, 0
			reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
				reads++
				if test.mode == "unavailable" {
					return hostWorkloadLeaseSnapshot{}, errors.New("API transport unavailable")
				}
				copy := cloneHostNetworkStageSnapshot(snapshot)
				if test.mode == "wrong-node" {
					copy.TenantNodes[0].SetLabels(map[string]string{hostLeaseDPUNodeNameLabel: "other-dpu-node", hostLeaseDPUNodeNamespaceLabel: activation.selected[0].dpuSet.GetNamespace()})
				}
				if test.mode == "changed-pool-rv" && reads > 1 {
					copy.CIDRPool.SetResourceVersion("changed-after-host-effect")
				}
				return copy, nil
			}
			configurer := func(_ context.Context, target config.Target, request sshprobe.HostNetworkRequest, timeout time.Duration) (sshprobe.HostNetworkReceipt, error) {
				effects++
				selected := activation.selected[0]
				if target.SSH != selected.hostTarget.SSH || target.Endpoint != selected.hostTarget.Endpoint || target.InputRef != selected.hostTarget.InputRef || timeout != time.Second {
					t.Fatal("host effect received a different SSH authority or timeout")
				}
				if request.Identity.SystemUUID != fixture.host.SystemUUID || request.Identity.AdapterSerial != selected.adapter.SerialNumber ||
					request.Identity.PCIAddress != "0000:21:00.0" || request.Identity.InterfaceName != "ens5f0np0" || request.Identity.PermanentMAC != "20:4d:52:3e:9a:b8" ||
					request.Identity.PhysicalPort != "p0" || request.MTU != fixture.prepared.execution.plan.Intent().Networking.WorkloadMTU ||
					request.WorkloadCIDR != fixture.prepared.execution.plan.Intent().Networking.WorkloadCIDR || !validSHA256Digest(request.LeaseBinding) {
					t.Fatalf("host effect request is not retained-profile/physical-identity bound: %+v", request)
				}
				encoded, _ := json.Marshal(request)
				digest := sha256.Sum256(encoded)
				receipt := sshprobe.HostNetworkReceipt{RequestID: hex.EncodeToString(digest[:]), State: "complete", RuntimeVerified: true, FileVerified: true, Blocking: false}
				switch test.mode {
				case "unknown-effect":
					receipt.State, receipt.RuntimeVerified, receipt.FileVerified, receipt.Blocking = "unknown", false, false, true
					return receipt, &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", Blocking: true}
				case "incomplete-effect":
					receipt.FileVerified = false
				case "changed-deployment":
					fixture.client.deployment.SetUID("replacement-deployment")
				}
				return receipt, nil
			}
			results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation, journal, time.Second, reader, configurer, nil)
			if test.wantReason == "" && err != nil || test.wantReason != "" && (err == nil || !strings.Contains(err.Error(), test.wantReason)) {
				t.Fatalf("result error=%v, want reason %q", err, test.wantReason)
			}
			if effects != test.wantEffects || len(results) != test.wantResults {
				t.Fatalf("effects=%d results=%d, want %d/%d", effects, len(results), test.wantEffects, test.wantResults)
			}
			if len(results) > 0 && results[0].LeaseReverified != test.wantReverified {
				t.Fatalf("lease reverified=%v, want %v", results[0].LeaseReverified, test.wantReverified)
			}
			report := journal.file.(*managementMemoryFile).String()
			if effects > 0 && (!strings.Contains(report, "HOST_NETWORK_CONFIGURATION_STARTED") || !strings.Contains(report, "HOST_NETWORK_LOCAL_RESULT") || !strings.Contains(report, `"hostNetwork":`)) {
				t.Fatal("durable report omitted the host attempt or partial transaction receipt")
			}
			if test.wantReverified && !strings.Contains(report, "HOST_NETWORK_CONFIGURATION_VERIFIED") {
				t.Fatal("durable report omitted the verified allocation fence")
			}
		})
	}
}

func TestRunHostWorkloadNetworkStagePreservesPartialTwoHostResults(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	journal := testJournal(fixture.prepared.execution)
	effects, reads := 0, 0
	reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
		reads++
		return cloneHostNetworkStageSnapshot(snapshot), nil
	}
	configure := func(_ context.Context, target config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
		effects++
		if effects == 1 {
			if target.InputRef != "host-input-a" || request.Identity.AdapterSerial != "SERIAL-A" {
				t.Fatalf("first host request was not bound to host A: target=%+v request=%+v", target, request)
			}
			return sshprobe.HostNetworkReceipt{RequestID: "first-host", State: "complete", RuntimeVerified: true, FileVerified: true}, nil
		}
		if target.InputRef != "host-input-b" || request.Identity.AdapterSerial != "SERIAL-B" {
			t.Fatalf("second host request was not bound to host B: target=%+v request=%+v", target, request)
		}
		return sshprobe.HostNetworkReceipt{RequestID: "second-host", State: "unknown", Blocking: true, ReasonCode: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN"},
			&sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", Blocking: true}
	}

	results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation,
		journal, time.Second, reader, configure, nil)
	if err == nil || !strings.Contains(err.Error(), "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN") || effects != 2 || reads != 3 || len(results) != 2 {
		t.Fatalf("partial two-host stage: effects=%d reads=%d results=%#v err=%v", effects, reads, results, err)
	}
	if results[0].DPUSerial != "SERIAL-A" || !results[0].LeaseReverified || results[0].Configuration.RequestID != "first-host" ||
		results[1].DPUSerial != "SERIAL-B" || results[1].LeaseReverified || results[1].Configuration.RequestID != "second-host" {
		t.Fatalf("partial host receipts were not preserved in serial order: %#v", results)
	}
	report := journal.file.(*managementMemoryFile).String()
	confirmed, localResults := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(report), "\n") {
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Operation == "configure" && event.State == "confirmed" && event.HostNetwork != nil && event.HostNetwork.DPUSerial == "SERIAL-A" && event.HostNetwork.LeaseReverified {
			confirmed++
		}
		if event.Operation == "host-local-result" && event.HostNetwork != nil {
			localResults++
		}
	}
	if confirmed != 1 || localResults != 2 {
		t.Fatalf("journal did not retain the first host confirmation and both local results: confirmed=%d localResults=%d report=%s", confirmed, localResults, report)
	}
}

func TestRunHostWorkloadNetworkStageStopsOnChangedCohortBeforeSecondHost(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	journal := testJournal(fixture.prepared.execution)
	effects, reads := 0, 0
	reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
		reads++
		copy := cloneHostNetworkStageSnapshot(snapshot)
		if reads == 3 {
			copy.TenantNodes[1].SetUID(types.UID("replacement-tenant-node-b"))
		}
		return copy, nil
	}
	configure := func(_ context.Context, _ config.Target, _ sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
		effects++
		return sshprobe.HostNetworkReceipt{RequestID: "first-host", State: "complete", RuntimeVerified: true, FileVerified: true}, nil
	}
	results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation,
		journal, time.Second, reader, configure, nil)
	if err == nil || !strings.Contains(err.Error(), "HOST_NETWORK_COHORT_ALLOCATION_CHANGED") || effects != 1 || reads != 3 || len(results) != 1 ||
		!results[0].LeaseReverified || results[0].DPUSerial != "SERIAL-A" {
		t.Fatalf("changed cohort did not stop before host B: effects=%d reads=%d results=%#v err=%v", effects, reads, results, err)
	}
}

func TestRunHostWorkloadNetworkStageRechecksHostBActivationMetadataAfterLeaseRead(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, test := range []struct {
		name       string
		mutate     func(*activationIntegrationFixture, activationSelectedDevice)
		wantReason string
	}{
		{
			name: "external reboot becomes pending",
			mutate: func(fixture *activationIntegrationFixture, selected activationSelectedDevice) {
				node := fixture.client.base.resources["DPUNode/"+selected.dpuSet.GetNamespace()+"/"+selected.nodeName]
				annotations := node.GetAnnotations()
				annotations[activationExternalRebootKey] = "true"
				node.SetAnnotations(annotations)
				node.SetResourceVersion("99")
			},
			wantReason: "HOST_NETWORK_REBOOT_PENDING",
		},
		{
			name: "maintenance hold is reasserted",
			mutate: func(fixture *activationIntegrationFixture, selected activationSelectedDevice) {
				maintenance := returnedActivationMaintenance(t, fixture.client, selected.nodeName)
				annotations := maintenance.GetAnnotations()
				annotations[activationHoldKey] = "true"
				maintenance.SetAnnotations(annotations)
				maintenance.SetResourceVersion("99")
				fixture.client.maint = append(fixture.client.maint, *maintenance)

			},
			wantReason: "HOST_NETWORK_ACTIVATION_SCOPE_UNAVAILABLE",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, activation, snapshot := hostNetworkStageFixtureFromExecution(t, execution)
			var selectedB *activationSelectedDevice
			for i := range activation.selected {
				if activation.selected[i].host.InputRef == "host-input-b" {
					selectedB = &activation.selected[i]
				}
			}
			if selectedB == nil {
				t.Fatal("retained two-host fixture has no selected host B")
			}
			nodeKey := "DPUNode/" + selectedB.dpuSet.GetNamespace() + "/" + selectedB.nodeName
			nodeBefore := fixture.client.base.resources[nodeKey].DeepCopy()
			var maintenanceBefore *unstructured.Unstructured
			for i := range fixture.client.removedMaintenance {
				maintenance := &fixture.client.removedMaintenance[i]
				if maintenance.GetNamespace() == selectedB.dpuSet.GetNamespace() && maintenance.GetLabels()[activationDPUNodeLabel] == selectedB.nodeName {
					maintenanceBefore = maintenance.DeepCopy()
					break
				}
			}
			var dpuBefore *unstructured.Unstructured
			for i := range fixture.client.dpus {
				if fixture.client.dpus[i].GetName() == selectedB.dpuName && fixture.client.dpus[i].GetNamespace() == selectedB.dpuSet.GetNamespace() {
					dpuBefore = fixture.client.dpus[i].DeepCopy()
					break
				}
			}
			if nodeBefore == nil || dpuBefore == nil || maintenanceBefore == nil {
				t.Fatal("selected host B fixture is missing its DPUNode, DPU or maintenance object")
			}

			journal := testJournal(fixture.prepared.execution)
			reads, effects := 0, 0
			reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
				reads++
				copy := cloneHostNetworkStageSnapshot(snapshot)
				if reads == 3 {
					test.mutate(&fixture, *selectedB)
				}
				return copy, nil
			}
			configure := func(_ context.Context, target config.Target, _ sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
				effects++
				if target.InputRef != "host-input-a" {
					t.Fatalf("host B was configured after its activation metadata changed: target=%+v", target)
				}
				return sshprobe.HostNetworkReceipt{RequestID: "host-a", State: "complete", RuntimeVerified: true, FileVerified: true}, nil
			}

			results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
				activation, journal, time.Second, reader, configure, nil)
			if err == nil || !strings.Contains(err.Error(), "HOST_NETWORK_LEASE_UNAVAILABLE") || effects != 1 || reads != 3 || len(results) != 1 {
				t.Fatalf("metadata drift should stop before host B: effects=%d reads=%d results=%#v err=%v", effects, reads, results, err)
			}
			if results[0].HostInputRef != "host-input-a" || results[0].DPUSerial != "SERIAL-A" || !results[0].LeaseReverified {
				t.Fatalf("host A verified receipt was not retained: %#v", results[0])
			}
			if node := fixture.client.base.resources[nodeKey]; node == nil || node.GetUID() != nodeBefore.GetUID() || node.GetGeneration() != nodeBefore.GetGeneration() {
				t.Fatal("fixture mutation changed host B DPUNode identity or generation")
			}
			dpuFound := false
			for i := range fixture.client.dpus {
				if fixture.client.dpus[i].GetName() == dpuBefore.GetName() && fixture.client.dpus[i].GetNamespace() == dpuBefore.GetNamespace() {
					dpuFound = true
					if fixture.client.dpus[i].GetUID() != dpuBefore.GetUID() || fixture.client.dpus[i].GetGeneration() != dpuBefore.GetGeneration() {
						t.Fatal("fixture mutation changed host B DPU identity or generation")
					}
					break
				}
			}
			if !dpuFound {
				t.Fatal("selected host B DPU disappeared")
			}
			maintenanceFound := false
			for i := range fixture.client.maint {
				maintenance := &fixture.client.maint[i]
				if maintenance.GetNamespace() == maintenanceBefore.GetNamespace() && maintenance.GetName() == maintenanceBefore.GetName() {
					maintenanceFound = true
					if maintenance.GetUID() != maintenanceBefore.GetUID() || maintenance.GetGeneration() != maintenanceBefore.GetGeneration() {
						t.Fatal("fixture mutation changed host B maintenance identity or generation")
					}
					break
				}
			}
			if test.name == "maintenance hold is reasserted" && !maintenanceFound {
				t.Fatal("injected selected host B maintenance object disappeared")
			}

			var matchingReason int
			for _, line := range strings.Split(strings.TrimSpace(journal.file.(*managementMemoryFile).String()), "\n") {
				var event executionEvent
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event.Stage == hostWorkloadNetworkStage && event.Operation == "activation-readiness" && event.ReasonCode == test.wantReason {
					matchingReason++
				}
			}
			if matchingReason != 1 {
				t.Fatalf("journal has %d activation fence events with reason %s, want one", matchingReason, test.wantReason)
			}
		})
	}
}

func TestRunHostWorkloadNetworkStageJournalSyncFailuresBracketEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name           string
		failSyncAt     int
		wantEffects    int
		wantReads      int
		wantReceipts   int
		wantReverified bool
	}{
		{name: "attempt is not durable", failSyncAt: 1, wantEffects: 0, wantReads: 1},
		{name: "local result is not durable", failSyncAt: 2, wantEffects: 1, wantReads: 1, wantReceipts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, activation, snapshot := hostNetworkStageFixtureFromExecution(t, execution)
			journal := testJournal(fixture.prepared.execution)
			memory := journal.file.(*managementMemoryFile)
			file := &hostNetworkStageFailingJournalFile{managementMemoryFile: memory, failSyncAt: test.failSyncAt}
			journal.file = file
			reads, effects := 0, 0
			reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
				reads++
				return cloneHostNetworkStageSnapshot(snapshot), nil
			}
			configure := func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkReceipt, error) {
				effects++
				return sshprobe.HostNetworkReceipt{RequestID: "host", State: "complete", RuntimeVerified: true, FileVerified: true}, nil
			}
			results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
				activation, journal, time.Second, reader, configure, nil)
			if err == nil || effects != test.wantEffects || reads != test.wantReads || len(results) != test.wantReceipts ||
				(len(results) > 0 && results[0].LeaseReverified != test.wantReverified) {
				t.Fatalf("journal failure effects=%d reads=%d results=%#v err=%v", effects, reads, results, err)
			}
			if journal.failed == nil || file.syncs != test.failSyncAt {
				t.Fatalf("journal did not retain expected sync failure: failed=%v syncs=%d", journal.failed, file.syncs)
			}
		})
	}
}

type hostNetworkStageFailingJournalFile struct {
	*managementMemoryFile
	failSyncAt int
	syncs      int
}

func (f *hostNetworkStageFailingJournalFile) Sync() error {
	f.syncs++
	if f.syncs == f.failSyncAt {
		return errors.New("simulated report sync failure")
	}
	return nil
}

func TestHostNetworkAllocationBindingSeparatesSemanticIdentityFromReadFences(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixture(t)
	deployment, err := retainedDeploymentObject(fixture.prepared.execution)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := observeActivationScope(fixture.ctx, fixture.client, fixture.prepared.execution, deployment, activation.DeploymentUID, activation.selected, activation.progress)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := resolveHostWorkloadLeases(fixture.prepared.execution.plan, fixture.prepared.execution.reconciler, activation.selected, observed, fixture.workload, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(fixture.host, activation.selected[0].adapter.Key, activation.selected[0].adapter.SerialNumber)
	if err != nil {
		t.Fatal(err)
	}
	before := hostNetworkLeaseFence{snapshot: snapshot, leases: leases}
	intent := fixture.prepared.execution.plan.Intent()
	first, err := hostNetworkRequestForLease(activation, identity, before, leases[0].Serial, intent.Networking.WorkloadMTU, intent.Networking.WorkloadCIDR)
	if err != nil {
		t.Fatal(err)
	}
	after := hostNetworkLeaseFence{snapshot: cloneHostNetworkStageSnapshot(snapshot), leases: append([]hostWorkloadLease(nil), leases...)}
	after.snapshot.DPUServiceIPAM.SetResourceVersion("status-refresh")
	after.snapshot.CIDRPool.SetResourceVersion("status-refresh")
	second, err := hostNetworkRequestForLease(activation, identity, after, leases[0].Serial, intent.Networking.WorkloadMTU, intent.Networking.WorkloadCIDR)
	if err != nil || !reflect.DeepEqual(first, second) || !sameHostNetworkAllocation(before, after) || sameHostLeasePoolObservation(before.snapshot.CIDRPool, after.snapshot.CIDRPool) {
		t.Fatalf("semantic operation identity and within-effect RV fence were conflated: %v", err)
	}
	after.leases[0].DPUUID = "replacement-dpu"
	third, err := hostNetworkRequestForLease(activation, identity, after, leases[0].Serial, intent.Networking.WorkloadMTU, intent.Networking.WorkloadCIDR)
	if err != nil || first.LeaseBinding == third.LeaseBinding || sameHostNetworkAllocation(before, after) {
		t.Fatal("changed allocation identity reused an earlier operation binding")
	}
}

func TestRunHostWorkloadNetworkStageRequiresSettledActivationAroundEffects(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixture(t)
	selected := activation.selected[0]
	nodeKey := "DPUNode/" + selected.dpuSet.GetNamespace() + "/" + selected.nodeName
	for _, change := range []string{"parent-not-ready", "maintenance-reheld", "maintenance-not-applied", "node-effect-not-ready", "external-reboot-requested"} {
		for _, afterEffect := range []bool{false, true} {
			phase := "before"
			if afterEffect {
				phase = "after"
			}
			t.Run(change+"-"+phase, func(t *testing.T) {
				parent := fixture.client.deployment.DeepCopy()
				node := fixture.client.base.resources[nodeKey].DeepCopy()
				maintenance := deepCopyUnstructuredList(fixture.client.maint)
				dpus := deepCopyUnstructuredList(fixture.client.dpus)
				t.Cleanup(func() {
					fixture.client.deployment = parent
					fixture.client.base.resources[nodeKey] = node
					fixture.client.maint = maintenance
					fixture.client.dpus = dpus
				})
				mutate := func() {
					switch change {
					case "parent-not-ready":
						_ = unstructured.SetNestedSlice(fixture.client.deployment.Object, []any{conditionMap("Ready", "False", "Progressing", 1)}, "status", "conditions")
					case "maintenance-reheld":
						fixture.client.maint = []unstructured.Unstructured{*returnedActivationMaintenance(t, fixture.client, selected.nodeName)}
						annotations := fixture.client.maint[0].GetAnnotations()
						annotations[activationHoldKey] = "true"
						fixture.client.maint[0].SetAnnotations(annotations)
					case "maintenance-not-applied":
						fixture.client.maint = []unstructured.Unstructured{*returnedActivationMaintenance(t, fixture.client, selected.nodeName)}
						_ = unstructured.SetNestedSlice(fixture.client.maint[0].Object, []any{conditionMap("NodeEffectApplied", "False", "Progressing", 1)}, "status", "conditions")
					case "node-effect-not-ready":
						_ = unstructured.SetNestedSlice(fixture.client.dpus[0].Object, []any{conditionMap("Ready", "True", "Success", 1), conditionMap("NodeEffectReady", "False", "Progressing", 1)}, "status", "conditions")
					case "external-reboot-requested":
						current := fixture.client.base.resources[nodeKey]
						annotations := current.GetAnnotations()
						if annotations == nil {
							annotations = map[string]string{}
						}
						annotations[activationExternalRebootKey] = "true"
						current.SetAnnotations(annotations)
					}
				}
				if !afterEffect {
					mutate()
				}
				effects := 0
				results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation, testJournal(fixture.prepared.execution), time.Second,
					func(context.Context) (hostWorkloadLeaseSnapshot, error) {
						return cloneHostNetworkStageSnapshot(snapshot), nil
					},
					func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkReceipt, error) {
						effects++
						mutate()
						return sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true, Blocking: false}, nil
					}, nil)
				wantEffects := 0
				wantReason := "HOST_NETWORK_LEASE_UNAVAILABLE"
				if afterEffect {
					wantEffects, wantReason = 1, "HOST_NETWORK_POST_FENCE_UNCERTAIN"
				}
				if err == nil || !strings.Contains(err.Error(), wantReason) || effects != wantEffects || len(results) != wantEffects {
					t.Fatalf("unsettled activation must stop the stage: error=%v effects=%d results=%d, want %s/%d", err, effects, len(results), wantReason, wantEffects)
				}
				if len(results) > 0 && results[0].LeaseReverified {
					t.Fatal("post-effect activation drift was reported as verified")
				}
			})
		}
	}
}

func hostNetworkStageFixture(t *testing.T) (activationIntegrationFixture, activationStageReceipt, hostWorkloadLeaseSnapshot) {
	return hostNetworkStageFixtureForSerials(t, []string{"SERIAL-A"})
}

func hostNetworkStageFixtureForSerials(t *testing.T, serials []string) (activationIntegrationFixture, activationStageReceipt, hostWorkloadLeaseSnapshot) {
	t.Helper()
	return hostNetworkStageFixtureFromExecution(t, prepareSoftwareStageExecution(t, serials))
}

func hostNetworkStageFixtureFromExecution(t *testing.T, execution *managementExecution) (activationIntegrationFixture, activationStageReceipt, hostWorkloadLeaseSnapshot) {
	t.Helper()
	fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	fixture.ctx = ctx
	for i := range fixture.hosts {
		host := fixture.hosts[i]
		pciBus := 0x21 + i
		interfaceP0, interfaceP1 := "ens5f0np0", "ens5f1np1"
		if i > 0 {
			interfaceP0, interfaceP1 = "ens6f0np0", "ens6f1np1"
		}
		macP0 := fmt.Sprintf("20:4d:52:3e:9a:%02x", 0xb8+i*2)
		macP1 := fmt.Sprintf("20:4d:52:3e:9a:%02x", 0xb9+i*2)
		pciP0, pciP1 := fmt.Sprintf("0000:%02x:00.0", pciBus), fmt.Sprintf("0000:%02x:00.1", pciBus)
		host.Adapters[0].PCIAddress = pciP0
		host.Adapters[0].PCIFunctions = []inventory.PCIFunction{
			{Address: pciP0, Driver: "mlx5_core", Netdevs: []string{interfaceP0}},
			{Address: pciP1, Driver: "mlx5_core", Netdevs: []string{interfaceP1}},
		}
		host.Runtime = &inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{
			{Name: interfaceP0, PCIAddress: pciP0, PhysicalPortName: "p0", MAC: fmt.Sprintf("02:00:00:00:00:%02x", i*2+1), PermanentMAC: macP0},
			{Name: interfaceP1, PCIAddress: pciP1, PhysicalPortName: "p1", MAC: fmt.Sprintf("02:00:00:00:00:%02x", i*2+2), PermanentMAC: macP1},
		}}}
		fixture.hosts[i] = host
		fixture.inventory.Hosts[i] = host
	}
	fixture.host = fixture.hosts[0]
	rebooter := RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		for _, node := range request.DPUNodes {
			fixture.client.completeReboot(node.NodeName)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
	})
	activation, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter)
	if err != nil {
		t.Fatalf("actual fixture activation: %v\nreport: %s", err, fixture.journal.file.(*managementMemoryFile).String())
	}
	plan := fixture.prepared.execution.plan
	desiredIPAM, err := retainedWorkloadIPAM(plan)
	if err != nil {
		t.Fatal(err)
	}
	ipam, err := fixture.client.base.Get(fixture.ctx, desiredIPAM)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := fixture.client.getTenantWorkloadChild(fixture.ctx, fixture.client.tenant, ipam)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(pool.Object, hostLeaseAllocationRows(t, plan, activation.selected), "status", "allocations"); err != nil {
		t.Fatal(err)
	}
	pool.SetResourceVersion("12")
	nodes := make([]unstructured.Unstructured, 0, len(activation.selected))
	for i, selected := range activation.selected {
		suffix := string(rune('a' + i))
		nodes = append(nodes, unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{
			"name": "tenant-node-" + suffix, "uid": "tenant-node-uid-" + suffix, "resourceVersion": "7", "labels": map[string]any{
				hostLeaseDPUNodeNameLabel: selected.nodeName, hostLeaseDPUNodeNamespaceLabel: selected.dpuSet.GetNamespace()}},
			"status": map[string]any{"conditions": []any{conditionMap("Ready", "True", "Success", 1)}},
		}})
	}
	return fixture, activation, hostWorkloadLeaseSnapshot{DPUServiceIPAM: ipam, CIDRPool: pool, TenantNodes: nodes}
}

func cloneHostNetworkStageSnapshot(source hostWorkloadLeaseSnapshot) hostWorkloadLeaseSnapshot {
	return hostWorkloadLeaseSnapshot{DPUServiceIPAM: source.DPUServiceIPAM.DeepCopy(), CIDRPool: source.CIDRPool.DeepCopy(), TenantNodes: deepCopyUnstructuredList(source.TenantNodes)}
}

// Reintroduce archived metadata only to model an unexpected post-completion
// maintenance object. It is never used to create activation progress evidence.
func returnedActivationMaintenance(t *testing.T, client *activationIntegrationFake, nodeName string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range client.removedMaintenance {
		if object.GetLabels()[activationDPUNodeLabel] == nodeName {
			return object.DeepCopy()
		}
	}
	t.Fatalf("no removed maintenance fixture for %s", nodeName)
	return nil
}
