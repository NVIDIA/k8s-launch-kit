// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type bootstrapACKRebooter struct {
	base  *recoveryRebooter
	after func()
}

func (r bootstrapACKRebooter) RebootExternal(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
	observation, err := r.base.RebootExternal(ctx, request)
	if err == nil {
		r.after()
	}
	return observation, err
}
func (r bootstrapACKRebooter) ObserveExternal(ctx context.Context, request ActivationRequest, baseline string) (ActivationObservation, error) {
	return r.base.ObserveExternal(ctx, request, baseline)
}

func TestBootstrapCredentialsFencePhysicalAndACKEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, boundary := range []string{"before claim", "before submission", "fresh ACK", "resumed ACK"} {
		t.Run(boundary, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			operation, store := checkpointFixture(t, f)
			f.prepared.operation = operation
			client := recoveryActivationClient{f.client}
			testTenant(f.controlPlane.tenant.Client).source = client
			base := &recoveryRebooter{fixture: &f}
			var reboot ExternalRebooter = base
			run := func() error {
				_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
				return err
			}
			switch boundary {
			case "before claim":
				f.client.bootstrapRegistrationAbsent = true
			case "before submission":
				base.beforeBaseline = func() { f.client.bootstrapRegistrationAbsent = true }
			case "fresh ACK":
				reboot = bootstrapACKRebooter{base: base, after: func() { f.client.bootstrapRegistrationAbsent = true }}
			case "resumed ACK":
				base.loseResponse = true
				if err := run(); err == nil || base.submissions != 1 {
					t.Fatalf("initial lost response=%v submissions=%d", err, base.submissions)
				}
				f.prepared.operation = reopenOperation(t, store)
				base.loseResponse = false
				base.afterObservation = func() { f.client.bootstrapRegistrationAbsent = true }
			}
			err := run()
			if err == nil || !strings.Contains(err.Error(), "BOOTSTRAP_JOIN_SECRET_UNAVAILABLE") {
				t.Fatalf("boundary %s: %v", boundary, err)
			}
			wantSubmissions, wantClaims := 0, 0
			if boundary != "before claim" {
				wantClaims = 1
			}
			if boundary == "fresh ACK" || boundary == "resumed ACK" {
				wantSubmissions = 1
			}
			if base.submissions != wantSubmissions || len(f.client.guards) != wantClaims || len(f.client.patches) != 1 || f.client.patches[0] != activationHoldKey {
				t.Fatalf("effects: submissions=%d claims=%d patches=%v", base.submissions, len(f.client.guards), f.client.patches)
			}
			if boundary == "resumed ACK" && base.observations != 1 {
				t.Fatalf("resume failed to observe exactly once: %d", base.observations)
			}
			for _, reset := range f.prepared.operation.state.Resets {
				if reset.Acknowledged {
					t.Fatal("failed guard acknowledged reset")
				}
				if boundary == "before submission" && reset.Baseline != "" {
					t.Fatal("failed guard bypassed durable-baseline callback ordering")
				}
			}
			contents := f.journal.file.(*managementMemoryFile).String()
			if !strings.Contains(string(contents), "BOOTSTRAP_JOIN_SECRET_UNAVAILABLE") || strings.Contains(string(contents), bootstrapTestSecret) {
				t.Fatal("safe cause not retained in durable journal")
			}
		})
	}
}

func TestBootstrapCredentialsDiagnoseAbsentRegistrationDuringJoinWait(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	// No token checks run before controller-owned children exist. Once a current
	// selected DPU reaches the join phase, known absent credentials stop the wait.
	operation, _ := checkpointFixture(t, f)
	f.prepared.operation = operation
	client := bootstrapJoinWaitClient{recoveryActivationClient{f.client}}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	if err == nil || !strings.Contains(err.Error(), "BOOTSTRAP_JOIN_SECRET_UNAVAILABLE") || reboot.submissions != 0 || len(f.client.guards) != 0 || len(f.client.patches) != 0 {
		t.Fatalf("join wait err=%v submissions=%d guards=%d patches=%v", err, reboot.submissions, len(f.client.guards), f.client.patches)
	}
}

type bootstrapJoinWaitClient struct{ recoveryActivationClient }

func (c bootstrapJoinWaitClient) List(ctx context.Context, want *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	objects, err := c.recoveryActivationClient.List(ctx, want, selector)
	if want.GetKind() == "DPU" && err == nil && len(objects) > 0 {
		c.bootstrapRegistrationAbsent = true
		for i := range objects {
			_ = unstructured.SetNestedField(objects[i].Object, "DPU Cluster Config", "status", "phase")
		}
		for i := range c.dpus {
			_ = unstructured.SetNestedField(c.dpus[i].Object, "DPU Cluster Config", "status", "phase")
		}
	}
	return objects, err
}

// This uses the real activation driver and bootstrap HTTP/API reader, initially
// with no registration, and exercises valid token/JWS at all effect boundaries.
func TestActivationReadsValidBootstrapCredentialsBeforeLaterRegistration(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	operation, _ := checkpointFixture(t, f)
	f.prepared.operation = operation
	item := f.client.activationSelected[0]
	dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU"}}
	dpu.SetName(item.dpuName)
	dpu.SetNamespace(f.prepared.execution.operatorNamespace)
	dpu.SetUID(activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial))
	materials, state := bootstrapCredentialMaterials(t, dpu, f.client.sources.sources.secret.Data["super-admin.conf"])
	f.client.base.resources[tenantStageKey(materials.join)] = materials.join
	f.client.bootstrapTenantHandler = state.serve
	client := bootstrapRegistrationAfterACKClient{recoveryActivationClient: recoveryActivationClient{f.client}, state: state}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	if err != nil || reboot.submissions != 1 || len(f.client.guards) != 1 || len(f.client.patches) != 2 {
		t.Fatalf("valid credential path err=%v submissions=%d guards=%d patches=%v", err, reboot.submissions, len(f.client.guards), f.client.patches)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.reads["/api/v1/namespaces/kube-system/secrets/bootstrap-token-"+bootstrapTestID] < 6 || state.reads["/api/v1/namespaces/kube-public/configmaps/cluster-info"] < 6 || state.node == nil {
		t.Fatalf("real reader or later registration not exercised: reads=%v", state.reads)
	}
}

type bootstrapRegistrationAfterACKClient struct {
	recoveryActivationClient
	state *bootstrapTenantFixture
}

func (c bootstrapRegistrationAfterACKClient) patchActivationAnnotation(ctx context.Context, object *unstructured.Unstructured, uid types.UID, rv, key string, value *string) (*unstructured.Unstructured, error) {
	updated, err := c.recoveryActivationClient.patchActivationAnnotation(ctx, object, uid, rv, key, value)
	if err == nil && key == activationExternalRebootKey && value == nil {
		c.state.mu.Lock()
		c.state.node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: c.state.nodeName, UID: "new-registration", ResourceVersion: "1"}}
		c.state.mu.Unlock()
	}
	return updated, err
}

func TestBootstrapBeforeSubmitUnrelatedSignerUpdateDoesNotBurnClaim(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	operation, _ := checkpointFixture(t, f)
	f.prepared.operation = operation
	item := f.client.activationSelected[0]
	dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU"}}
	dpu.SetName(item.dpuName)
	dpu.SetNamespace(f.prepared.execution.operatorNamespace)
	dpu.SetUID(activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial))
	materials, state := bootstrapCredentialMaterials(t, dpu, f.client.sources.sources.secret.Data["super-admin.conf"])
	f.client.base.resources[tenantStageKey(materials.join)] = materials.join
	f.client.bootstrapTenantHandler = state.serve
	state.beforeRead = func(path string, n int) {
		if strings.Contains(path, "/configmaps/") && n == 4 {
			state.info.ResourceVersion = "other-token-updated"
			state.info.Data["jws-kubeconfig-other1"] = "other-token-signature"
		}
	}
	client := bootstrapRegistrationAfterACKClient{recoveryActivationClient: recoveryActivationClient{f.client}, state: state}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	if err != nil || reboot.submissions != 1 || len(f.client.guards) != 1 || len(f.client.patches) != 2 {
		t.Fatalf("unrelated signer update burned claim: err=%v submissions=%d guards=%d patches=%v", err, reboot.submissions, len(f.client.guards), f.client.patches)
	}
	if len(f.prepared.operation.state.Resets) != 1 {
		t.Fatal("missing exact reset receipt")
	}
	for _, reset := range f.prepared.operation.state.Resets {
		if reset.Baseline != operationOldBoot || !reset.Acknowledged {
			t.Fatal("durable baseline or completed ACK lost")
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.info.Data["jws-kubeconfig-other1"] != "other-token-signature" || state.reads["/api/v1/namespaces/kube-public/configmaps/cluster-info"] < 6 {
		t.Fatal("BeforeSubmit churn or complete real reader not exercised")
	}
}
