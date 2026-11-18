// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

type prerequisiteDriftRebooter struct {
	base  *recoveryRebooter
	after func()
}

func (r prerequisiteDriftRebooter) RebootExternal(ctx context.Context, req ActivationRequest) (ActivationObservation, error) {
	got, err := r.base.RebootExternal(ctx, req)
	if err == nil {
		r.after()
	}
	return got, err
}
func (r prerequisiteDriftRebooter) ObserveExternal(ctx context.Context, req ActivationRequest, boot string) (ActivationObservation, error) {
	return r.base.ObserveExternal(ctx, req, boot)
}
func TestActivationACKRefreshesSharedCredentialAfterReboot(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	base := &recoveryRebooter{fixture: &f}
	reboot := prerequisiteDriftRebooter{base: base, after: func() {
		secret := f.client.base.resources["Secret/"+f.prepared.execution.operatorNamespace+"/"+enrollmentSecretName]
		if secret == nil {
			t.Fatal("no secret")
		}
		secret.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174099"))
	}}
	_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	t.Logf("result=%v patches=%v submissions=%d", err, f.client.patches, base.submissions)
	if err == nil || !strings.Contains(err.Error(), "ACTIVATION_ENROLLMENT_SECRET_CHANGED") {
		t.Fatalf("credential replacement was not rejected before ACK: %v", err)
	}
	if len(f.client.patches) != 1 || f.client.patches[0] != activationHoldKey || base.submissions != 1 {
		t.Fatalf("ACK issued after shared credential replacement: %v", f.client.patches)
	}
}

func TestActivationResumedACKRefreshesSharedCredential(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, store := checkpointFixture(t, f)
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f, loseResponse: true}
	run := func() error {
		_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil {
		t.Fatal("lost dispatch response succeeded")
	}
	f.prepared.operation = reopenOperation(t, store)
	reboot.loseResponse = false
	reboot.afterObservation = func() {
		secret := f.client.base.resources["Secret/"+f.prepared.execution.operatorNamespace+"/"+enrollmentSecretName]
		if secret == nil {
			t.Fatal("no secret")
		}
		secret.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174099"))
	}
	err := run()
	t.Logf("result=%v patches=%v submissions=%d observations=%d", err, f.client.patches, reboot.submissions, reboot.observations)
	if err == nil || !strings.Contains(err.Error(), "ACTIVATION_ENROLLMENT_SECRET_CHANGED") {
		t.Fatalf("credential replacement was not rejected before ACK: %v", err)
	}
	if len(f.client.patches) != 1 || f.client.patches[0] != activationHoldKey || reboot.submissions != 1 || reboot.observations != 1 {
		t.Fatalf("resumed reset/ACK proceeded after shared credential replacement: %v", f.client.patches)
	}
}

type prerequisiteDriftSafetyClient struct{ recoveryActivationClient }

func (c prerequisiteDriftSafetyClient) verifyActivationRebootBMCs(ctx context.Context, selected []activationRebootBMC, timeout time.Duration, journal *executionJournal) (time.Time, error) {
	expires, err := c.activationIntegrationFake.verifyActivationRebootBMCs(ctx, selected, timeout, journal)
	if err == nil {
		c.base.resources["Secret/"+c.execution.operatorNamespace+"/"+enrollmentSecretName].SetUID(types.UID("123e4567-e89b-12d3-a456-426614174099"))
	}
	return expires, err
}
func TestActivationResetRefreshesResourcesAfterSafetyRead(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	client := prerequisiteDriftSafetyClient{recoveryActivationClient{f.client}}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
	t.Logf("result=%v submissions=%d claims=%d patches=%v", err, reboot.submissions, f.client.guardCalls, f.client.patches)
	if err == nil || !strings.Contains(err.Error(), "ACTIVATION_ENROLLMENT_SECRET_CHANGED") {
		t.Fatalf("credential drift did not fail the reset boundary: %v", err)
	}
	if reboot.submissions != 0 || f.client.guardCalls != 0 || len(f.client.patches) != 1 || f.client.patches[0] != activationHoldKey {
		t.Fatalf("reset/claim/ACK proceeded after credential replacement: submissions=%d claims=%d patches=%v", reboot.submissions, f.client.guardCalls, f.client.patches)
	}
}
