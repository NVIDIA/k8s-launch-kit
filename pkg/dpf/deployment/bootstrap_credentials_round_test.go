// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBootstrapBeforeSubmitRejectsChangedResetRound(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, part := range []string{"startup", "kernel boot", "request timestamp", "source phase"} {
		t.Run(part, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			op, _ := checkpointFixture(t, f)
			f.prepared.operation = op
			item := f.client.activationSelected[0]
			dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU"}}
			dpu.SetName(item.dpuName)
			dpu.SetNamespace(f.prepared.execution.operatorNamespace)
			dpu.SetUID(activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial))
			m, state := bootstrapCredentialMaterials(t, dpu, f.client.sources.sources.secret.Data["super-admin.conf"])
			f.client.base.resources[tenantStageKey(m.join)] = m.join
			armed, due, changed := &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}
			f.client.bootstrapTenantHandler = func(w http.ResponseWriter, r *http.Request) bool {
				if armed.Load() && !changed.Load() && r.URL.Path == "/api/v1/namespaces/kube-public/configmaps/cluster-info" {
					due.Store(true)
				}
				return state.serve(w, r)
			}
			client := &bootstrapPolicyDriftClient{bootstrapRegistrationAfterACKClient: bootstrapRegistrationAfterACKClient{recoveryActivationClient: recoveryActivationClient{f.client}, state: state}, due: due, changed: changed, mutate: func() {
				object := f.client.dpus[0].Object
				switch part {
				case "startup":
					_ = unstructured.SetNestedField(object, "2026-11-13T13:30:00Z", "status", "agentStatus", "lastStartupTime")
				case "kernel boot":
					_ = unstructured.SetNestedField(object, "33333333-3333-4333-8333-333333333333", "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
				case "request timestamp":
					_ = unstructured.SetNestedField(object, "2026-11-13T13:30:00Z", "status", "rebootStatus", "lastTransitionTime")
				case "source phase":
					_ = unstructured.SetNestedField(object, "DPU Config", "status", "previousPhase")
				}
			}}
			testTenant(f.controlPlane.tenant.Client).source = client
			base := &recoveryRebooter{fixture: &f, beforeBaseline: func() { armed.Store(true) }}
			_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, base)
			acks, baselines := 0, 0
			for _, v := range f.prepared.operation.state.Resets {
				if v.Acknowledged {
					acks++
				}
				if v.Baseline != "" {
					baselines++
				}
			}
			if !changed.Load() {
				t.Fatal("round did not change inside BeforeSubmit credential inspection")
			}
			if err == nil || base.submissions != 0 || len(f.client.guards) != 1 || baselines != 0 || acks != 0 {
				t.Fatalf("part=%s claims=%d submissions=%d savedBaselines=%d acknowledged=%d err=%v", part, len(f.client.guards), base.submissions, baselines, acks, err)
			}
		})
	}
}
