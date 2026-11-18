// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// The HTTP response publishes a pending drift; the next management GET inside
// the same credential helper applies it synchronously. This locates the change
// after the earlier policy fence and avoids a fixture-only concurrent map write.
type bootstrapPolicyDriftClient struct {
	bootstrapRegistrationAfterACKClient
	due, changed *atomic.Bool
	mutate       func()
}

func (c bootstrapPolicyDriftClient) Get(ctx context.Context, want *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if c.due.CompareAndSwap(true, false) {
		c.mutate()
		c.changed.Store(true)
	}
	return c.recoveryActivationClient.Get(ctx, want)
}

func TestBootstrapCredentialsRecheckSharedPolicyAfterReads(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, boundary := range []string{"preclaim", "BeforeSubmit", "fresh ACK", "resumed ACK"} {
		t.Run(boundary, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			op, store := checkpointFixture(t, f)
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
				secret := f.client.base.resources["Secret/"+f.prepared.execution.operatorNamespace+"/"+enrollmentSecretName]
				if secret == nil {
					t.Fatal("missing shared enrollment credential fixture")
				}
				secret.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174099"))
			}}
			testTenant(f.controlPlane.tenant.Client).source = client
			base := &recoveryRebooter{fixture: &f}
			var reboot ExternalRebooter = base
			run := func() error {
				_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
				return err
			}
			switch boundary {
			case "preclaim":
				armed.Store(true)
			case "BeforeSubmit":
				base.beforeBaseline = func() { armed.Store(true) }
			case "fresh ACK":
				reboot = bootstrapACKRebooter{base: base, after: func() { armed.Store(true) }}
			case "resumed ACK":
				base.loseResponse = true
				if err := run(); err == nil || base.submissions != 1 {
					t.Fatalf("lost-response setup: %v submissions=%d", err, base.submissions)
				}
				f.prepared.operation = reopenOperation(t, store)
				base.loseResponse = false
				base.afterObservation = func() { armed.Store(true) }
			}
			err := run()
			acks := 0
			baselines := 0
			for _, v := range f.prepared.operation.state.Resets {
				if v.Acknowledged {
					acks++
				}
				if v.Baseline != "" {
					baselines++
				}
			}
			wantClaims, wantSubmits := 1, 1
			if boundary == "preclaim" {
				wantClaims, wantSubmits = 0, 0
			}
			if boundary == "BeforeSubmit" {
				wantSubmits = 0
			}
			if !changed.Load() {
				t.Fatal("shared prerequisite drift never occurred during credential read")
			}
			if err == nil || !strings.Contains(err.Error(), "ACTIVATION_ENROLLMENT_SECRET_CHANGED") || len(f.client.guards) != wantClaims || base.submissions != wantSubmits || acks != 0 {
				t.Fatalf("boundary=%s claims=%d submissions=%d savedBaselines=%d acknowledged=%d patches=%v observations=%d err=%v", boundary, len(f.client.guards), base.submissions, baselines, acks, f.client.patches, base.observations, err)
			}
		})
	}
}
