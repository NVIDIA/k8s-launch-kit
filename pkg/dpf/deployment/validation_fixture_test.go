// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

// A table prepares these two read-only snapshots once for the same retained
// plan. Each subtest builds its own observer, clients and live state.
type validationFixturePreparation struct {
	execution *managementExecution
	desired   *managementDesired
}

func prepareValidationFixture(t *testing.T, serials []string) validationFixturePreparation {
	t.Helper()
	execution := prepareSoftwareStageExecution(t, serials)
	desired, err := prepareManagementDesiredFromPlan(execution.retained, execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	return validationFixturePreparation{execution: execution, desired: desired}
}

func TestValidationRuntimeFixturePreparationKeepsChildStateIsolated(t *testing.T) {
	prepared := prepareValidationFixture(t, []string{"SERIAL-A"})
	const childKey = "NodeServiceInterfaces/selected-node-sfc"
	const changed = "first-child-only"
	var first *n3cRuntimeAdapter
	if !t.Run("mutate live state and close first fixture", func(t *testing.T) {
		adapter, children := n3cRuntimeAdapterFixtureFromPreparation(t, prepared)
		first = adapter
		if first.native.desired != prepared.desired {
			t.Fatal("first observer did not receive the prepared desired state")
		}
		if _, err := first.ObserveResources(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		children[childKey].SetUID(changed)
		first.pods[0].Spec.Containers[0].Image = changed
		first.native.systemArtifacts = &systemArtifacts{}
		first.before = func(int) { t.Error("first fixture callback ran after cleanup") }
	}) {
		return
	}

	second, children := n3cRuntimeAdapterFixtureFromPreparation(t, prepared)
	if second.native.desired != prepared.desired || second.native == first.native ||
		second.management == first.management || second.tenant.api == first.tenant.api {
		t.Fatal("observers must share only the prepared desired state")
	}
	if second.calls != 0 || len(second.snapshots) != 0 || second.before != nil ||
		second.native.systemArtifacts != nil || children[childKey].GetUID() == changed ||
		second.pods[0].Spec.Containers[0].Image == changed {
		t.Fatal("first fixture state reached the second fixture")
	}
	snapshot, err := second.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range snapshot.Checks {
		if row.State != validation.Passed {
			t.Errorf("fresh runtime observation after first cleanup: %+v", row)
		}
	}
}

func TestValidationPhysicalFixturePreparationKeepsChildStateIsolated(t *testing.T) {
	prepared := preparePhysicalValidationFixture(t)
	const changed = "first-child-only"
	var first *validationObserver
	if !t.Run("mutate physical and request state", func(t *testing.T) {
		observer, bmcs, hosts := physicalValidationFixtureFromPreparation(t, prepared)
		first = observer
		if first.desired != prepared.desired {
			t.Fatal("first observer did not receive the prepared desired state")
		}
		for endpoint, bmc := range bmcs {
			bmc.PSID = changed
			bmcs[endpoint] = bmc
		}
		for _, host := range hosts {
			host.Runtime.BootID = changed
		}
		first.request.Targets[0].Endpoint = changed
		first.physical.host = nil
	}) {
		return
	}

	second, _, hosts := physicalValidationFixtureFromPreparation(t, prepared)
	if second.desired != prepared.desired || second == first ||
		second.physicalScope.expected == first.physicalScope.expected ||
		second.request.Targets[0].Endpoint == changed || second.physical.host == nil {
		t.Fatal("physical observers must share only the prepared desired state")
	}
	for _, host := range hosts {
		if host.Runtime.BootID == changed {
			t.Fatal("first fixture changed the second fixture's host boot identity")
		}
	}
	r := newValidationRecorder(second.checks, nil)
	if _, err := second.observePhysicalDevices(context.Background(), r, true); err != nil {
		t.Fatal(err)
	}
	for _, serial := range []string{"SERIAL-A", "SERIAL-B"} {
		for _, kind := range []string{"dpu", "host"} {
			if row := r.outcomes[physicalID(kind, serial)]; row.State != validation.Passed {
				t.Errorf("fresh physical observation after first cleanup: %+v", row)
			}
		}
	}
}
