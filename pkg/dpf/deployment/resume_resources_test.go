// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/types"
)

func TestOwnedResumeResourceProofRetainsCompleteActivationOwnership(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name   string
		parent bool
		edit   func(*activationIntegrationFixture, *deploymentOperation)
		pass   bool
	}{
		{"reserved before first parent", false, func(*activationIntegrationFixture, *deploymentOperation) {}, true},
		{"exact current parent and children", true, func(*activationIntegrationFixture, *deploymentOperation) {}, true},
		{"replaced device", true, func(f *activationIntegrationFixture, _ *deploymentOperation) {
			f.client.base.resources["DPUDevice/"+execution.operatorNamespace+"/serial-a"].SetUID("replacement")
		}, false},
		{"replaced node", true, func(f *activationIntegrationFixture, _ *deploymentOperation) {
			f.client.base.resources["DPUNode/"+execution.operatorNamespace+"/dpu-node-serial-a"].SetUID("replacement")
		}, false},
		{"replaced parent", true, func(f *activationIntegrationFixture, _ *deploymentOperation) {
			f.client.deployment.SetUID("replacement")
		}, false},
		{"foreign same serial DPU", true, func(f *activationIntegrationFixture, _ *deploymentOperation) {
			other := f.client.dpus[0].DeepCopy()
			other.SetName("foreign")
			other.SetNamespace("foreign-ns")
			other.SetUID("foreign")
			f.client.dpus = append(f.client.dpus, *other)
		}, false},
		{"shared credential changed", true, func(f *activationIntegrationFixture, _ *deploymentOperation) {
			f.client.base.resources["Secret/"+execution.operatorNamespace+"/"+enrollmentSecretName].SetUID("replacement")
		}, false},
		{"tenant sources differ", true, func(_ *activationIntegrationFixture, o *deploymentOperation) {
			o.state.TenantSources["Secret/foreign/source"] = "foreign"
		}, false},
		{"recorded child generation changed", true, func(f *activationIntegrationFixture, o *deploymentOperation) {
			child := f.client.sets[0]
			o.state.Children["DPUSet/"+child.GetName()] = activationObjectReceipt{Name: child.GetName(), UID: child.GetUID(), Generation: child.GetGeneration() + 1}
		}, false},
		{"missing recorded child", true, func(_ *activationIntegrationFixture, o *deploymentOperation) {
			o.state.Children["DPU/missing"] = activationObjectReceipt{Name: "missing", UID: types.UID("missing"), Generation: 1}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			op, _ := checkpointFixture(t, f)
			f.prepared.operation = op
			sources, err := f.controlPlane.tenant.Client.operationSources(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			op.state.TenantSources = sources
			if test.parent {
				desired, err := retainedDeploymentObject(execution.verificationInputs())
				if err != nil {
					t.Fatal(err)
				}
				desired, err = execution.reconciler.markedDesired(desired)
				if err != nil {
					t.Fatal(err)
				}
				created, err := f.client.Create(f.ctx, desired, false)
				if err != nil {
					t.Fatal(err)
				}
				op.state.DeploymentUID = created.GetUID()
			}
			test.edit(&f, op)
			patches, claims := len(f.client.patches), f.client.guardCalls
			proof, err := verifyOwnedResumeResources(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal)
			if (err == nil) != test.pass {
				t.Fatalf("proof=%+v err=%v", proof, err)
			}
			if test.pass && len(proof.selected) != len(f.prepared.selected) {
				t.Fatal("proof lost physical cohort")
			}
			raw := []inventory.Check{
				{ID: "dpf.hbn.management.enrollments", Subject: "selected DPUs", Vantage: "executor", RequiredPhase: "deploy", State: "failed", ReasonCode: "DPU_ALREADY_ENROLLED", Message: "Initial deployment requires unclaimed devices."},
				{ID: "dpf.hbn.management.tenant-network", Subject: "tenant " + testTenant(f.controlPlane.tenant.Client).expected.Name + " network selection", Vantage: "management-cluster", RequiredPhase: "deploy", State: "unknown", ReasonCode: "TENANT_EXISTING_NETWORK_UNVERIFIED", Message: "Initial endpoint vacancy is unmeasured."},
				{ID: "dpf.hbn.management.operator", Subject: execution.operatorNamespace, Vantage: "executor", RequiredPhase: "deploy", State: "failed", ReasonCode: "DPF_OPERATOR_NOT_READY", Message: "Current operator is not ready."},
				{ID: "dpf.hbn.management.storage-health", Subject: execution.operatorNamespace, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "STORAGE_HEALTH_UNMEASURED", Message: "Current storage is unmeasured."},
			}
			before, _ := json.Marshal(raw)
			got := proof.assessInitialChecks(raw, testTenant(f.controlPlane.tenant.Client).expected.Name)
			if test.pass {
				if got[0].ReasonCode != "OWNED_RESUME_ENROLLMENT_VERIFIED" || got[1].ReasonCode != "OWNED_RESUME_TENANT_NETWORK_VERIFIED" || got[0].State != "passed" || got[1].State != "passed" {
					t.Fatalf("real raw blocked predicate shape was not assessed from owned proof: %+v", got)
				}
			} else if !reflect.DeepEqual(raw, got) {
				t.Fatal("failed resource proof authorized initial-state exceptions")
			}
			if !reflect.DeepEqual(raw[2:], got[2:]) {
				t.Fatal("owned resume waived operator or storage readiness")
			}
			after, _ := json.Marshal(raw)
			if string(before) != string(after) {
				t.Fatal("assessment mutated original discovery checks")
			}
			if test.pass {
				for _, edit := range []func(*inventory.Check){func(c *inventory.Check) { c.Subject = "foreign" }, func(c *inventory.Check) { c.Vantage = "foreign" }, func(c *inventory.Check) { c.RequiredPhase = "generate" }, func(c *inventory.Check) { c.ReasonCode = "DPU_ENROLLMENT_IDENTITY_CONFLICT" }} {
					changed := raw[0]
					edit(&changed)
					checked := proof.assessInitialChecks([]inventory.Check{changed}, testTenant(f.controlPlane.tenant.Client).expected.Name)
					if !reflect.DeepEqual(checked[0], changed) {
						t.Fatal("wrong scope or identity conflict was waived")
					}
				}
			}

			if len(f.client.patches) != patches || f.client.guardCalls != claims {
				t.Fatal("prerequisite proof performed activation effects")
			}
		})
	}
}
