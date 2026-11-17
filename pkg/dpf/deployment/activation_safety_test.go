// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This adapter preserves the full activation stage but uses the concrete
// APIClient BMC boundary, including its real journal requirement and HTTP path.
// Kubernetes resources and the physical reboot callback remain local fixtures.
type activationNativeBMCClient struct {
	*activationIntegrationFake
	native *APIClient
}

func (c activationNativeBMCClient) verifyEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, timeout time.Duration, journal *executionJournal) error {
	return c.native.verifyEnrollmentBMCs(ctx, selected, timeout, journal)
}

func TestActivationBoundaryUsesRealAPIClientBMCJournalAndRetainsSafeDiagnostics(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	api := newAPIFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	native, err := NewAPIClient(fixture.ctx, api.request, api.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	bmc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected BMC effect %s", r.Method)
		}
		reads.Add(1)
		http.Error(w, "private-remote-body-canary", http.StatusForbidden)
	}))
	defer bmc.Close()
	// Explicit negative fixture: the runtime endpoint refuses even its public
	// root. Retained hardware/plan identity is not replaced to manufacture success.
	t.Setenv("ACTIVATION_TEST_PASSWORD", "private-password-canary")
	input, err := config.Decode([]byte("credentials:\n  a: {username: admin, passwordRef: {env: ACTIVATION_TEST_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{bmcEndpoint: 'https://192.0.2.3', credentialRef: a}]\n  dpus: [{bmcEndpoint: '"+bmc.URL+"', credentialRef: a}]\n  probes: {mode: inventory}\n"), "/site.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range resolved.Targets {
		if target.Role == "dpus" {
			fixture.prepared.selected[0].target = target
		}
	}
	client := activationNativeBMCClient{activationIntegrationFake: fixture.client, native: native}
	fixture.controlPlane.tenant.Client.source = client
	calls := 0
	_, err = runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
		calls++
		return ActivationObservation{}, errors.New("forbidden")
	}))
	if err == nil || reads.Load() == 0 {
		t.Fatalf("real BMC boundary not reached with owned journal: reads=%d err=%v", reads.Load(), err)
	}
	if calls != 0 || fixture.client.guardCalls != 0 || len(fixture.client.patches) != 0 {
		t.Fatalf("effect before real BMC check: calls=%d claims=%d patches=%v", calls, fixture.client.guardCalls, fixture.client.patches)
	}
	report := fixture.journal.file.(*managementMemoryFile).String()
	if !strings.Contains(report, "ACCESS_DENIED") || !strings.Contains(report, "HTTP 403") {
		t.Fatalf("safe request metadata missing: %s", report)
	}
	if strings.Contains(report, "canary") || strings.Contains(err.Error(), "canary") {
		t.Fatal("private response or credential leaked")
	}
}

func TestActivationResetSafetyFailurePreventsClaimRebootAndACK(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	fixture.client.resetSafetyError = errors.New("reset state unavailable")
	calls := 0
	_, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
		calls++
		return ActivationObservation{}, errors.New("forbidden")
	}))
	if err == nil || fixture.client.resetSafetyCalls != 1 || calls != 0 || fixture.client.guardCalls != 0 {
		t.Fatalf("err=%v checks=%d physical=%d claims=%d", err, fixture.client.resetSafetyCalls, calls, fixture.client.guardCalls)
	}
	for _, patch := range fixture.client.patches {
		if patch == activationExternalRebootKey {
			t.Fatal("ACK issued without reset safety")
		}
	}
}

func TestActivationDoesNotACKWithoutNewCurrentArmBoot(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mode := range []string{"unchanged-agent", "same-kernel", "replacement", "new-request"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
			ctx, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			callbacks := 0
			_, err := runActivationStage(ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
				callbacks++
				for _, node := range request.DPUNodes {
					fixture.client.completeReboot(node.NodeName)
				}
				dpu := &fixture.client.dpus[0]
				switch mode {
				case "unchanged-agent":
					_ = unstructured.SetNestedField(dpu.Object, "2026-11-13T12:00:00Z", "status", "agentStatus", "lastStartupTime")
				case "same-kernel":
					_ = unstructured.SetNestedField(dpu.Object, "11111111-1111-4111-8111-111111111111", "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
				case "replacement":
					dpu.SetUID("replacement")
				case "new-request":
					_ = unstructured.SetNestedField(dpu.Object, "2026-11-13T13:30:00Z", "status", "rebootStatus", "lastTransitionTime")
				}
				if mode == "unchanged-agent" || mode == "same-kernel" {
					time.AfterFunc(30*time.Millisecond, cancel)
				}
				return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
			}))
			if (mode == "unchanged-agent" || mode == "same-kernel") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation classification lost: %v", err)
			}
			if err == nil || callbacks != 1 {
				t.Fatalf("err=%v callbacks=%d", err, callbacks)
			}
			for _, patch := range fixture.client.patches {
				if patch == activationExternalRebootKey {
					t.Fatal("ACK issued without same-round new current kernel proof")
				}
			}
			report := fixture.journal.file.(*managementMemoryFile).String()
			if !strings.Contains(report, "EXTERNAL_HOST_REBOOT_VERIFIED") || strings.Contains(report, "EXTERNAL_REBOOT_ACKNOWLEDGED") {
				t.Fatal("physical outcome lost or ACK falsely claimed")
			}
		})
	}
}

// Change only the DPU request after the new-boot proof was durably recorded,
// specifically while the final DPUNode read is in progress before its CAS ACK.
type activationACKRaceClient struct {
	*activationIntegrationFake
	journal *executionJournal
	changed bool
}

func (c *activationACKRaceClient) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	result, err := c.activationIntegrationFake.Get(ctx, wanted)
	if wanted.GetKind() == "DPUNode" && !c.changed && strings.Contains(c.journal.file.(*managementMemoryFile).String(), "EXTERNAL_REBOOT_AGENT_VERIFIED") {
		c.changed = true
		_ = unstructured.SetNestedField(c.dpus[0].Object, "2026-11-13T13:30:00Z", "status", "rebootStatus", "lastTransitionTime")
	}
	return result, err
}
func TestActivationFencesDPURequestAgainImmediatelyBeforeACK(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	client := &activationACKRaceClient{activationIntegrationFake: fixture.client, journal: fixture.journal}
	fixture.controlPlane.tenant.Client.source = client
	_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		for _, node := range request.DPUNodes {
			client.completeReboot(node.NodeName)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
	}))
	if !client.changed || err == nil || !strings.Contains(err.Error(), "EXTERNAL_REBOOT_ACK_ROUND_CHANGED") {
		t.Fatalf("changed=%v err=%v", client.changed, err)
	}
	for _, patch := range client.patches {
		if patch == activationExternalRebootKey {
			t.Fatal("changed request acknowledged")
		}
	}
}

func TestActivationVerifiesEverySameHostDPUAndACKsBothAfterOneReboot(t *testing.T) {
	fixture := newActivationIntegrationCohortFixture(t, []string{"SERIAL-A", "SERIAL-B"}, true)
	calls := 0
	receipt, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		calls++
		if len(request.DPUNodes) != 2 {
			t.Fatalf("same-host cohort omitted node: %#v", request.DPUNodes)
		}
		if len(fixture.client.resetSafetySerials) != 2 {
			t.Fatal("physical action before both BMC checks")
		}
		for _, node := range request.DPUNodes {
			fixture.client.completeReboot(node.NodeName)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
	}))
	if err != nil || calls != 1 || len(receipt.Reboots) != 1 {
		t.Fatalf("err=%v calls=%d reboots=%#v", err, calls, receipt.Reboots)
	}
	acks := 0
	for _, patch := range fixture.client.patches {
		if patch == activationExternalRebootKey {
			acks++
		}
	}
	if acks != 2 {
		t.Fatalf("ACKs=%d want both same-host DPU nodes", acks)
	}
}

func TestActivationRefusesUnmeasuredPreOSResetAndChangedRequestBeforeClaim(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mode := range []string{"agent-less-firmware", "request-changed-during-BMC"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
			fixture.client.omitAgentBaseline = mode == "agent-less-firmware"
			fixture.client.changeRoundDuringResetSafety = mode == "request-changed-during-BMC"
			calls := 0
			_, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
				calls++
				return ActivationObservation{}, errors.New("forbidden")
			}))
			reason := "ACTIVATION_REBOOT_REQUEST_CHANGED"
			if mode == "agent-less-firmware" {
				reason = "ACTIVATION_PRE_OS_RESET_UNSUPPORTED"
			}
			if err == nil || !strings.Contains(err.Error(), reason) || calls != 0 || fixture.client.guardCalls != 0 {
				t.Fatalf("err=%v calls=%d claims=%d", err, calls, fixture.client.guardCalls)
			}
			for _, patch := range fixture.client.patches {
				if patch == activationExternalRebootKey {
					t.Fatal("unsupported/changed round acknowledged")
				}
			}
		})
	}
}
