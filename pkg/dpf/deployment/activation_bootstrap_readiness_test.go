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

func TestBootstrapFirstReadHardFailureCannotBecomePending(t *testing.T) {
	execution, base, receipt, _, _ := enrollmentStageFixture(t)
	for _, tc := range []struct{ name, code string }{
		{"terminal", "RESOURCE_TERMINAL_FAILURE"},
		{"ahead generation", "RESOURCE_STATUS_INVALID"},
		{"malformed observed generation", "RESOURCE_STATUS_INVALID"},
		{"negative observed generation", "RESOURCE_STATUS_INVALID"},
		{"malformed condition", "RESOURCE_STATUS_INVALID"},
		{"ambiguous", "RESOURCE_STATUS_AMBIGUOUS"},
		{"malformed metadata", "RESOURCE_METADATA_INVALID"},
		{"missing generation", "RESOURCE_GENERATION_MISSING"},
		{"missing condition generation", "CONDITION_GENERATION_MISSING"},
		{"missing Ready condition", "READINESS_CONDITION_MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			client := &bootstrapReadinessTestClient{tenantStageFake: base.base}
			client.get = func(wanted, current *unstructured.Unstructured) {
				if wanted.GetKind() != "DPFOperatorConfig" {
					return
				}
				reads++
				setBootstrapTestReadiness(t, current, "True", "Success")
				if reads != 1 {
					return
				}
				switch tc.name {
				case "terminal":
					setBootstrapTestReadiness(t, current, "False", "Failure")
				case "ahead generation":
					_ = unstructured.SetNestedField(current.Object, int64(2), "status", "observedGeneration")
				case "malformed observed generation":
					_ = unstructured.SetNestedField(current.Object, "1", "status", "observedGeneration")
				case "negative observed generation":
					_ = unstructured.SetNestedField(current.Object, int64(-1), "status", "observedGeneration")
				case "malformed condition":
					_ = unstructured.SetNestedField(current.Object, "not-conditions", "status", "conditions")
				case "ambiguous":
					cs, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
					_ = unstructured.SetNestedSlice(current.Object, append(cs, cs[0]), "status", "conditions")
				case "malformed metadata":
					_ = unstructured.SetNestedField(current.Object, "invalid-timestamp", "metadata", "deletionTimestamp")
				case "missing generation":
					current.SetGeneration(0)
				case "missing condition generation":
					_ = unstructured.SetNestedSlice(current.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Success"}}, "status", "conditions")
				case "missing Ready condition":
					_ = unstructured.SetNestedSlice(current.Object, []any{map[string]any{"type": "Other", "status": "True", "reason": "Success", "observedGeneration": int64(1)}}, "status", "conditions")
				}
			}
			err := waitManagementBootstrap(tenantStageContext(t), client, execution.verificationInputs(), receipt, testJournal(execution), time.Millisecond)
			if err == nil || errors.Is(err, errBootstrapConfigNotReady) || !strings.Contains(err.Error(), tc.code) || reads != 1 {
				t.Fatalf("first hard failure was lost: reads=%d err=%v", reads, err)
			}
			check, _ := operationResumeCheck(tenantStageContext(t), execution.verificationInputs(), "OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", err)
			if check.ReasonCode != tc.code || bootstrapFailureDiagnostic(err) == "" {
				t.Fatalf("hard failure lost finite producer diagnostic: %+v", check)
			}
		})
	}
}

func TestBootstrapPendingWaitDoesNotDiscardHardIntermediateStatus(t *testing.T) {
	execution, base, receipt, _, _ := enrollmentStageFixture(t)
	for _, tc := range []struct{ name, code string }{
		{"terminal", "RESOURCE_TERMINAL_FAILURE"},
		{"malformed observed generation", "RESOURCE_STATUS_INVALID"},
		{"negative observed generation", "RESOURCE_STATUS_INVALID"},
		{"ambiguous", "RESOURCE_STATUS_AMBIGUOUS"},
		{"missing generation", "RESOURCE_GENERATION_MISSING"},
		{"missing condition generation", "CONDITION_GENERATION_MISSING"},
		{"wrong version", "BOOTSTRAP_CONFIG_VERSION_CHANGED"},
		{"wrong UID", "BOOTSTRAP_CONFIG_CHANGED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			client := &bootstrapReadinessTestClient{tenantStageFake: base.base}
			client.get = func(wanted, current *unstructured.Unstructured) {
				if wanted.GetKind() != "DPFOperatorConfig" {
					return
				}
				reads++
				setBootstrapTestReadiness(t, current, "True", "Success")
				if reads == 1 {
					setBootstrapTestReadiness(t, current, "False", "Error")
				}
				if reads != 2 {
					return
				}
				switch tc.name {
				case "terminal":
					setBootstrapTestReadiness(t, current, "False", "Failure")
				case "malformed observed generation":
					_ = unstructured.SetNestedField(current.Object, "1", "status", "observedGeneration")
				case "negative observed generation":
					_ = unstructured.SetNestedField(current.Object, int64(-1), "status", "observedGeneration")
				case "ambiguous":
					cs, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
					_ = unstructured.SetNestedSlice(current.Object, append(cs, cs[0]), "status", "conditions")
				case "missing generation":
					current.SetGeneration(0)
				case "missing condition generation":
					_ = unstructured.SetNestedSlice(current.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Success"}}, "status", "conditions")
				case "wrong version":
					_ = unstructured.SetNestedField(current.Object, "v0.0.1", "status", "version")
				case "wrong UID":
					current.SetUID("replaced")
				}
			}
			err := waitManagementBootstrap(tenantStageContext(t), client, execution.verificationInputs(), receipt, testJournal(execution), time.Millisecond)
			if err == nil || bootstrapConfigPending(err) || !strings.Contains(err.Error(), tc.code) || reads != 2 {
				t.Fatalf("intermediate hard failure disappeared: reads=%d err=%v", reads, err)
			}
		})
	}
}

func TestBootstrapPendingRecognitionDoesNotHideJoinedFailures(t *testing.T) {
	pending := reconcileError("RESOURCE_NOT_READY", "pending", errBootstrapConfigNotReady)
	journalErr := errors.New("private-journal-error")
	for _, err := range []error{errors.Join(pending, journalErr), reconcileError("BOOTSTRAP_CONFIG_READ_FAILED", "read failed", pending), errors.Join(pending)} {
		if bootstrapConfigPending(err) {
			t.Fatal("unowned or joined error was classified as pending")
		}
		if got := deferActivationBootstrap(context.Background(), nil, nil, nil, 0, err); got != err {
			t.Fatal("nonpending error entered a readiness wait")
		}
	}
}

func TestActivationBootstrapPendingAfterClaimCannotWaitOrReplay(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, boundary := range []string{"BeforeSubmit", "ACK"} {
		t.Run(boundary, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			op, _ := checkpointFixture(t, f)
			f.prepared.operation = op
			life := &activationLifecycleClient{activationIntegrationFake: f.client}
			c := &pendingActivationClient{activationLifecycleClient: life}
			bindActivationLifecycleClient(&f, life)
			testTenant(f.controlPlane.tenant.Client).source = c
			active, pendingReads, journalOffset := false, 0, 0
			activate := func() {
				active = true
				journalOffset = f.journal.file.(*managementMemoryFile).Len()
			}
			life.onGet = func(client *activationIntegrationFake, wanted *unstructured.Unstructured) {
				if active && wanted.GetKind() == "DPFOperatorConfig" {
					pendingReads++
					status, reason := "False", "Error"
					if pendingReads > 1 {
						status, reason = "True", "Success"
					}
					setBootstrapTestReadiness(t, client.base.resources[tenantStageKey(wanted)], status, reason)
				}
			}
			base := &recoveryRebooter{fixture: &f}
			var reboot ExternalRebooter = base
			if boundary == "BeforeSubmit" {
				base.beforeBaseline = func() {
					if base.submissions == 1 {
						activate()
					}
				}
			} else {
				reboot = bootstrapACKRebooter{base: base, after: func() {
					if base.submissions == 2 {
						activate()
					}
				}}
			}
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
			wantSubmissions := 1
			if boundary == "ACK" {
				wantSubmissions = 2
			}
			if err == nil || pendingReads != 1 || c.guardCalls != 2 || base.submissions != wantSubmissions {
				t.Fatalf("post-claim %s recovered: reads=%d claims=%d submissions=%d err=%v", boundary, pendingReads, c.guardCalls, base.submissions, err)
			}
			acknowledged := 0
			for _, reset := range op.state.Resets {
				if reset.ClaimUID == "" {
					t.Fatal("post-claim failure lost claim")
				}
				if reset.Acknowledged {
					acknowledged++
				}
			}
			if len(op.state.Resets) != 2 || acknowledged != 1 {
				t.Fatal("post-claim failure changed the completed peer ACK or acknowledged the pending host")
			}
			if strings.Contains(f.journal.file.(*managementMemoryFile).String()[journalOffset:], "DPF_CONFIG_READINESS_STARTED") {
				t.Fatal("post-claim policy entered readiness wait")
			}
			// A later explicit resume may inspect the existing claim; this invocation
			// must never submit a replacement physical action.
			_, _ = runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
			if c.guardCalls != 2 || base.submissions != wantSubmissions {
				t.Fatal("retry replayed a claimed physical action")
			}
		})
	}
}

type bootstrapFailingJournal struct {
	journalFile
	fail  bool
	cause error
}

func (f *bootstrapFailingJournal) Sync() error {
	if f.fail {
		return f.cause
	}
	return f.journalFile.Sync()
}

func TestActivationBootstrapPendingJournalFailureStopsBeforeClaim(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	life := &activationLifecycleClient{activationIntegrationFake: f.client}
	c := &pendingActivationClient{activationLifecycleClient: life}
	bindActivationLifecycleClient(&f, life)
	testTenant(f.controlPlane.tenant.Client).source = c
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	diskErr := errors.New("journal-sync-failed")
	file := &bootstrapFailingJournal{journalFile: f.journal.file, cause: errors.Join(reconcileError("RESOURCE_NOT_READY", "pending", errBootstrapConfigNotReady), diskErr)}
	f.journal.file = file
	life.onGet = func(client *activationIntegrationFake, wanted *unstructured.Unstructured) {
		if client.resetSafetyCalls > 0 && wanted.GetKind() == "DPFOperatorConfig" {
			setBootstrapTestReadiness(t, client.base.resources[tenantStageKey(wanted)], "False", "Error")
			file.fail = true
		}
	}
	base := &recoveryRebooter{fixture: &f}
	_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, base)
	if !errors.Is(err, diskErr) || bootstrapConfigPending(err) || c.guardCalls != 0 || base.submissions != 0 {
		t.Fatalf("pending swallowed journal failure: claims=%d submissions=%d err=%v", c.guardCalls, base.submissions, err)
	}
}

func TestActivationFinalBootstrapWaitRechecksCompleteBoundaryWithoutReset(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	life := &activationLifecycleClient{activationIntegrationFake: f.client}
	c := &bootstrapPhysicalReadClient{pendingActivationClient: &pendingActivationClient{activationLifecycleClient: life}}
	bindActivationLifecycleClient(&f, life)
	testTenant(f.controlPlane.tenant.Client).source = c
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	reads, physicalBeforeWait := 0, 0
	life.onGet = func(client *activationIntegrationFake, wanted *unstructured.Unstructured) {
		if wanted.GetKind() != "DPFOperatorConfig" || len(client.removedMaintenance) == 0 {
			return
		}
		reads++
		if reads == 1 {
			physicalBeforeWait = c.inspections
		}
		status, reason := "False", "Error"
		if reads > 2 {
			status, reason = "True", "Success"
		}
		setBootstrapTestReadiness(t, client.base.resources[tenantStageKey(wanted)], status, reason)
	}
	base := &recoveryRebooter{fixture: &f}
	result, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, base)
	if err != nil || len(result.DPUs) != 1 || reads < 4 || c.guardCalls != 1 || base.submissions != 1 {
		t.Fatalf("final convergence failed: reads=%d claims=%d submissions=%d err=%v", reads, c.guardCalls, base.submissions, err)
	}
	if c.inspections < physicalBeforeWait+2 {
		t.Fatal("final readiness recovery skipped the interrupted full physical fence or the final return fence")
	}
}

type bootstrapPhysicalReadClient struct {
	*pendingActivationClient
	inspections int
}

func (c *bootstrapPhysicalReadClient) verifyEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, timeout time.Duration, journal *executionJournal) error {
	c.inspections++
	return c.pendingActivationClient.verifyEnrollmentBMCs(ctx, selected, timeout, journal)
}
