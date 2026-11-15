// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHBNRuntimePhysicalChecksArePhaseBoundaries(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d-hosts", n), func(t *testing.T) {
			serials := make([]string, n)
			for i := range serials {
				serials[i] = fmt.Sprintf("SERIAL-%c", 'A'+i)
			}
			f := newRuntimeStageFixture(t, serials)
			f.client.physicalChecks = 0
			f.client.physicalCohorts = nil
			assertObservation := func() {
				if f.client.physicalChecks != 1 {
					t.Fatalf("measurement repeated/preceded physical phase verification: %d", f.client.physicalChecks)
				}
			}
			f.commandHook = func(int, hbnRuntimeCommand) { assertObservation() }
			f.trafficHook = func(int, *sshprobe.HostTrafficResult) { assertObservation() }
			patches, claims, softwareCreates := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls, f.client.createSoftware
			result, err := f.run(testJournal(f.prepared.execution))
			if err != nil || len(result.BGP) != n || len(result.Routes) != n*(n-1) || len(result.Traffic) != n*(n-1) || result.TrafficValidated != (n > 1) {
				t.Fatalf("matrix=%+v err=%v", result, err)
			}
			if f.client.physicalChecks != 2 || len(f.client.physicalCohorts) != 2 {
				t.Fatalf("physical boundaries=%d", f.client.physicalChecks)
			}
			for _, cohort := range f.client.physicalCohorts {
				if len(cohort) != n {
					t.Fatalf("physical boundary narrowed cohort: %v", cohort)
				}
				seen := map[string]int{}
				for _, serial := range cohort {
					seen[serial]++
				}
				for _, serial := range serials {
					if seen[serial] != 1 {
						t.Fatalf("physical boundary changed selected cohort: %v", cohort)
					}
				}
			}
			m := 2*n*n - n
			if f.leaseReads != 4*m+4 {
				t.Fatalf("allocation observation coverage changed: %d want %d", f.leaseReads, 4*m+4)
			}
			if f.commandReads != n+n*(n-1) || f.trafficReads != n*(n-1) || len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims || f.client.createSoftware != softwareCreates {
				t.Fatal("read-only runtime omitted reads or introduced mutation")
			}
		})
	}
}

func TestHBNRuntimePhysicalPhaseFailureRevokesAcceptance(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, phase := range []int{1, 2} {
		t.Run(fmt.Sprintf("phase-%d", phase), func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
			f.client.physicalChecks = 0
			f.client.physicalCohorts = nil
			f.client.physicalHook = func(call int) error {
				if call == phase {
					return errors.New("physical identity/admin unavailable")
				}
				return nil
			}
			journal := testJournal(f.prepared.execution)
			result, err := f.run(journal)
			if err == nil || result.TrafficValidated || result.State != "failed" || strings.Contains(journal.file.(*managementMemoryFile).String(), "HBN_RUNTIME_COHORT_VALIDATED") {
				t.Fatalf("physical failure passed: %+v err=%v", result, err)
			}
			if f.client.physicalChecks != phase {
				t.Fatalf("physical calls=%d want %d", f.client.physicalChecks, phase)
			}
			if phase == 1 && (f.commandReads != 0 || f.trafficReads != 0 || f.leaseReads != 0) {
				t.Fatal("failed entry phase reached measurement")
			}
			if phase == 2 && (len(result.BGP) != 2 || len(result.Routes) != 2 || len(result.Traffic) != 2) {
				t.Fatal("exit test did not cross complete matrix")
			}
		})
	}
}

func TestHBNRuntimeFinalObserveFollowsPhysicalExit(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	f.client.physicalChecks = 0
	f.client.physicalCohorts = nil
	f.client.physicalHook = func(call int) error {
		if call == 2 {
			f.pods[1].UID = "replaced-during-final-physical-read"
		}
		return nil
	}
	result, err := f.run(testJournal(f.prepared.execution))
	if err == nil || result.TrafficValidated || result.ReasonCode != "HBN_RUNTIME_FINAL_FENCE_FAILED" || f.client.physicalChecks != 2 || len(result.Traffic) != 2 {
		t.Fatalf("final physical-read drift=%+v err=%v", result, err)
	}
}

func TestHBNRuntimeResourceFenceKeepsFullCohortDriftChecks(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	cases := []struct {
		name   string
		mutate func(*testing.T, *runtimeStageFixture)
	}{
		{"management-manager", func(_ *testing.T, f *runtimeStageFixture) {
			f.client.base.resources[tenantStageKey(f.prepared.execution.manager)].SetUID("replaced-manager")
		}},
		{"tenant-source", func(_ *testing.T, f *runtimeStageFixture) {
			f.client.sources.sources.observed.Service.SetUID("replaced-tenant-service")
		}},
		{"software", func(t *testing.T, f *runtimeStageFixture) {
			replaceHoldFixtureObject(t, &f.activationIntegrationFixture, "BlueFieldSoftware")
		}},
		{"registry", func(_ *testing.T, f *runtimeStageFixture) { f.client.managerImage = "foreign.example/manager:tag" }},
		{"credential-secret", func(_ *testing.T, f *runtimeStageFixture) {
			identity := secretIdentity(f.prepared.execution.operatorNamespace)
			f.client.base.resources[tenantStageKey(identity)].SetUID("replaced-credential")
		}},
		{"workload", func(_ *testing.T, f *runtimeStageFixture) {
			r := f.workload.Resources[0]
			identity := &unstructured.Unstructured{}
			identity.SetKind(r.Kind)
			identity.SetNamespace(r.Namespace)
			identity.SetName(r.Name)
			f.client.base.resources[tenantStageKey(identity)].SetUID("replaced-workload")
		}},
		{"parent", func(_ *testing.T, f *runtimeStageFixture) { f.client.deployment.SetUID("replaced-parent") }},
		{"other-selected-set", func(_ *testing.T, f *runtimeStageFixture) { f.client.sets[1].SetUID("replaced-set") }},
		{"other-selected-dpu", func(_ *testing.T, f *runtimeStageFixture) { f.client.dpus[1].SetUID("replaced-dpu") }},
		{"other-selected-node", func(_ *testing.T, f *runtimeStageFixture) {
			selected := f.activation.selected[1]
			f.client.base.resources["DPUNode/"+selected.dpuSet.GetNamespace()+"/"+selected.nodeName].SetUID("replaced-node")
		}},
		{"other-selected-reboot", func(_ *testing.T, f *runtimeStageFixture) {
			selected := f.activation.selected[1]
			node := f.client.base.resources["DPUNode/"+selected.dpuSet.GetNamespace()+"/"+selected.nodeName]
			annotations := node.GetAnnotations()
			annotations[activationExternalRebootKey] = "true"
			node.SetAnnotations(annotations)
		}},
		{"other-selected-reheld", func(t *testing.T, f *runtimeStageFixture) {
			selected := f.activation.selected[1]
			f.client.maint = []unstructured.Unstructured{*returnedActivationMaintenance(t, f.activationIntegrationFixture.client, selected.nodeName)}
			annotations := f.client.maint[0].GetAnnotations()
			annotations[activationHoldKey] = "true"
			f.client.maint[0].SetAnnotations(annotations)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
			f.client.physicalChecks = 0
			f.client.physicalCohorts = nil
			f.commandHook = func(read int, _ hbnRuntimeCommand) {
				if read == 1 {
					test.mutate(t, f)
				}
			}
			patches, claims := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls
			result, err := f.run(testJournal(f.prepared.execution))
			if err == nil || result.TrafficValidated || len(result.BGP) != 1 || result.BGP[0].Fenced || f.commandReads != 1 || f.trafficReads != 0 || f.client.physicalChecks != 1 {
				t.Fatalf("resource drift accepted/depended on BMC: %+v reads=%d physical=%d err=%v", result, f.commandReads, f.client.physicalChecks, err)
			}
			if len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims {
				t.Fatal("negative runtime case mutated activation")
			}
		})
	}
}

func TestHostConfigurationKeepsFullPhysicalBoundary(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked-%v", block), func(t *testing.T) {
			f := newRuntimeStageFixture(t, []string{"SERIAL-A"})
			f.client.physicalChecks = 0
			f.client.physicalCohorts = nil
			if block {
				f.client.physicalHook = func(int) error { return errors.New("physical verification unavailable") }
			}
			effects := 0
			_, err := runHostWorkloadNetworkStage(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.activation, testJournal(f.prepared.execution), time.Second,
				func(context.Context) (hostWorkloadLeaseSnapshot, error) {
					return cloneHostNetworkStageSnapshot(f.snapshot), nil
				},
				func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkReceipt, error) {
					effects++
					if f.client.physicalChecks != 1 {
						t.Fatal("host effect did not have full physical pre-fence")
					}
					return f.hosts[0].Configuration, nil
				}, nil)
			if block && (err == nil || effects != 0 || f.client.physicalChecks != 1) {
				t.Fatalf("physical failure reached host effect: %v effects=%d", err, effects)
			}
			if !block && (err != nil || effects != 1 || f.client.physicalChecks != 2) {
				t.Fatalf("host mutation boundary changed: %v effects=%d physical=%d", err, effects, f.client.physicalChecks)
			}
		})
	}
}

func TestHBNRuntimePhysicalExitRechecksSelectedScopeAfterBMCRead(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	f.client.physicalChecks = 0
	f.client.physicalCohorts = nil
	f.client.physicalHook = func(call int) error {
		if call == 2 {
			selected := f.activation.selected[1]
			f.client.base.resources["DPUDevice/"+selected.dpuSet.GetNamespace()+"/"+selected.deviceName()].SetUID("replaced-during-bmc")
		}
		return nil
	}
	result, err := f.run(testJournal(f.prepared.execution))
	if err == nil || result.TrafficValidated || result.ReasonCode != "HBN_RUNTIME_PHYSICAL_EXIT_FAILED" || f.client.physicalChecks != 2 || len(result.Traffic) != 2 {
		t.Fatalf("post-BMC scope verification changed: %+v err=%v", result, err)
	}
}
