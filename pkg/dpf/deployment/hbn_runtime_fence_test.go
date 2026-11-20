// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHBNRuntimePhysicalChecksArePhaseBoundaries(t *testing.T) {
	for _, serials := range [][]string{{"SERIAL-A"}, {"SERIAL-A", "SERIAL-B"}} {
		t.Run(fmt.Sprint(len(serials)), func(t *testing.T) {
			f := newRuntimeStageFixture(t, serials)
			f.client.physicalChecks, f.client.physicalCohorts = 0, nil
			fences := fixtureAcceptanceFences(t, f, f.client, testJournal(f.prepared.execution))
			if err := fences.physical(f.ctx); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if err := fences.receipts(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if f.client.physicalChecks != 1 {
				t.Fatal("per-measurement receipt checks repeated physical collection")
			}
			if err := fences.physical(f.ctx); err != nil {
				t.Fatal(err)
			}
			if f.client.physicalChecks != 2 {
				t.Fatal("missing physical closure")
			}
			for _, cohort := range f.client.physicalCohorts {
				if len(cohort) != len(serials) {
					t.Fatalf("narrowed physical cohort: %v", cohort)
				}
				seen := map[string]int{}
				for _, serial := range cohort {
					seen[serial]++
				}
				for _, serial := range serials {
					if seen[serial] != 1 {
						t.Fatalf("changed physical cohort: %v", cohort)
					}
				}
			}
			if f.commandReads != 0 || f.trafficReads != 0 {
				t.Fatal("receipt decorator measured runtime")
			}
		})
	}
}

func TestHBNRuntimePhysicalPhaseFailureRevokesAcceptance(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, phase := range []int{1, 2} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
			f.client.physicalChecks = 0
			f.client.physicalHook = func(call int) error {
				if call == phase {
					return errors.New("physical identity/admin unavailable")
				}
				return nil
			}
			fences := fixtureAcceptanceFences(t, f, f.client, testJournal(execution))
			observer := &deploymentAcceptanceObserver{receipts: fences.receipts, physical: fences.physical, output: func() error { return nil }}
			if phase == 2 {
				if err := observer.fence(f.ctx, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := observer.fence(f.ctx, true); err == nil || f.client.physicalChecks != phase {
				t.Fatalf("physical phase %d accepted: %v", phase, err)
			}
			if err := observer.fence(f.ctx, false); err == nil {
				t.Fatal("poisoned phase re-anchored")
			}
		})
	}
}

func TestHBNRuntimeFinalObserveFollowsPhysicalExit(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	f.client.physicalChecks = 0
	fences := fixtureAcceptanceFences(t, f, f.client, testJournal(f.prepared.execution))
	observer := &deploymentAcceptanceObserver{receipts: fences.receipts, physical: fences.physical, output: func() error { return nil }}
	if err := observer.fence(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	f.client.physicalHook = func(call int) error {
		if call == 2 {
			f.snapshot.CIDRPool.SetUID("replaced-during-final-physical-read")
		}
		return nil
	}
	if err := observer.fence(f.ctx, true); err == nil || f.client.physicalChecks != 2 {
		t.Fatalf("allocation change during physical closure accepted: %v", err)
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
			fences := fixtureAcceptanceFences(t, f, f.client, testJournal(execution))
			if err := fences.physical(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err := fences.receipts(f.ctx); err != nil {
				t.Fatal(err)
			}
			patches, claims := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls
			test.mutate(t, f)
			if err := fences.receipts(f.ctx); err == nil {
				t.Fatal("changed operation receipt accepted")
			}
			if f.client.physicalChecks != 1 || f.commandReads != 0 || f.trafficReads != 0 {
				t.Fatal("receipt checks repeated BMC or measured traffic")
			}
			if len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims {
				t.Fatal("receipt observation mutated activation")
			}
		})
	}
}

func TestHostConfigurationKeepsFullPhysicalBoundary(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, block := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked-%v", block), func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
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
	fences := fixtureAcceptanceFences(t, f, f.client, testJournal(f.prepared.execution))
	if err := fences.physical(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.client.physicalHook = func(call int) error {
		if call == 2 {
			selected := f.activation.selected[1]
			f.client.base.resources["DPUDevice/"+selected.dpuSet.GetNamespace()+"/"+selected.deviceName()].SetUID("replaced-during-bmc")
		}
		return nil
	}
	if err := fences.physical(f.ctx); err == nil || f.client.physicalChecks != 2 {
		t.Fatalf("post-BMC selected device drift accepted: %v", err)
	}
}

func fixtureAcceptanceFences(t *testing.T, f *runtimeStageFixture, client activationBoundaryReader, journal *executionJournal) deploymentAcceptanceFences {
	t.Helper()
	fences, err := newDeploymentAcceptanceFences(testAcceptanceReceiptSource{client}, newControlPlaneVerificationInputs(f.prepared, f.controlPlane, f.workload), f.controlPlane.tenant.Client, f.activation, f.hosts, journal, time.Second, fixtureAcceptanceLeases(f.transports.readLeases))
	if err != nil {
		t.Fatal(err)
	}
	return fences
}

// Preserve the exact fake/diagnostic wrapper supplied by each test. Native API
// composition and boundary behavior are exercised separately through Run.
type testAcceptanceReceiptSource struct{ activationBoundaryReader }

func (s testAcceptanceReceiptSource) withAcceptanceReceiptReads(ctx context.Context, tenant acceptanceTenantReadSource, collect func(activationControlReader, acceptanceTenantReceiptReader) error) error {
	return collect(s.activationBoundaryReader, testAcceptanceTenantReads{tenant})
}

func (f *runtimeStageFake) withAcceptanceReceiptReads(ctx context.Context, tenant acceptanceTenantReadSource, collect func(activationControlReader, acceptanceTenantReceiptReader) error) error {
	return collect(f, testAcceptanceTenantReads{tenant})
}

func fixtureAcceptanceLeases(read hostLeaseSnapshotReader) acceptanceLeaseSnapshotReader {
	return func(ctx context.Context, _ resourceReader, _ currentHostLeaseTenantReader) (hostWorkloadLeaseSnapshot, error) {
		return read(ctx)
	}
}

// Existing fixture collection owns its lease snapshot; preserve its overridden
// tenant child reads instead of recovering an embedded concrete native client.
type testAcceptanceTenantReads struct{ tenantVerificationReader }

func (testAcceptanceTenantReads) listCompleteNodes(context.Context) ([]unstructured.Unstructured, error) {
	return nil, fmt.Errorf("unexpected bounded fixture Node read")
}
