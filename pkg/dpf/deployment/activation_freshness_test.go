// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestActivationClaimRefusesExpiryDuringCreateAndCannotRefreshReplayIdentity(t *testing.T) {
	journal, path := newRebootGuardJournal(t)
	request := rebootGuardRequest()
	request.ResetSafetyExpiresAt = time.Now().Add(250 * time.Millisecond)
	client := &rebootGuardClient{response: func(desired *unstructured.Unstructured) *unstructured.Unstructured {
		<-time.After(time.Until(request.ResetSafetyExpiresAt) + time.Millisecond)
		desired.SetUID(rebootGuardUID)
		desired.SetResourceVersion("1")
		return desired
	}}
	executor := &rebootGuardExecutor{journal: path}
	_, err := claimAndRebootExternal(context.Background(), client, rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"), journal, executor)
	if !errors.Is(err, ErrActivationResetSafetyExpired) || executor.calls != 0 || client.created == nil {
		t.Fatalf("err=%v callback=%d claim=%v", err, executor.calls, client.created != nil)
	}
	// Expiry is per-attempt evidence; refreshing it must never mint a new claim.
	first, digest, err := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"))
	if err != nil {
		t.Fatal(err)
	}
	request.ResetSafetyExpiresAt = time.Now().Add(time.Minute)
	second, nextDigest, err := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"))
	if err != nil {
		t.Fatal(err)
	}
	if first.GetName() != second.GetName() || digest != nextDigest {
		t.Fatal("expiry refresh changed immutable replay identity")
	}
}

type activationExpiredAPIFenceClient struct {
	*activationIntegrationFake
	expiry  time.Time
	delayed bool
}

func (c *activationExpiredAPIFenceClient) verifyActivationRebootBMCs(ctx context.Context, selected []activationRebootBMC, timeout time.Duration, journal *executionJournal) (time.Time, error) {
	_, err := c.activationIntegrationFake.verifyActivationRebootBMCs(ctx, selected, timeout, journal)
	c.expiry = time.Now().Add(250 * time.Millisecond)
	return c.expiry, err
}
func (c *activationExpiredAPIFenceClient) List(ctx context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if !c.expiry.IsZero() && !c.delayed {
		c.delayed = true
		<-time.After(time.Until(c.expiry) + time.Millisecond)
	}
	return c.activationIntegrationFake.List(ctx, wanted, selector)
}
func TestActivationDelayedFinalAPIFenceExpiresProofBeforeClaimWithoutACK(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	client := &activationExpiredAPIFenceClient{activationIntegrationFake: fixture.client}
	testTenant(fixture.controlPlane.tenant.Client).source = client
	calls := 0
	_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
		calls++
		return ActivationObservation{}, errors.New("forbidden")
	}))
	if !errors.Is(err, ErrActivationResetSafetyExpired) || !client.delayed || calls != 0 || client.guardCalls != 0 {
		t.Fatalf("err=%v delayed=%v callback=%d claims=%d", err, client.delayed, calls, client.guardCalls)
	}
	for _, patch := range client.patches {
		if patch == activationExternalRebootKey {
			t.Fatal("expired proof acknowledged")
		}
	}
	if !strings.Contains(fixture.journal.file.(*managementMemoryFile).String(), "ACTIVATION_RESET_SAFETY_EXPIRED") {
		t.Fatal("expiry outcome was not durable")
	}
}

func TestActivationRequestRejectsMissingUnboundedOrExpiredProof(t *testing.T) {
	for _, expiry := range []time.Time{{}, time.Now().Add(time.Hour), time.Now().Add(-time.Second)} {
		if err := (ActivationRequest{ResetSafetyExpiresAt: expiry}).VerifyResetSafetyFresh(); err == nil {
			t.Fatalf("accepted invalid expiry %v", expiry)
		}
	}
}
