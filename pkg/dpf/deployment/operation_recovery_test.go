// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Reject a chosen completion save after the controller actually consumed the
// ACK. Recovery must use physical/current-Arm observations, never another ACK.
type operationCompletionFailureStore struct {
	*operationMemoryStore
	lose bool
}

func (s *operationCompletionFailureStore) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	state, err := decodeOperation(object.Data["checkpoint.json"])
	if err != nil {
		return nil, err
	}
	for _, r := range state.Resets {
		if s.lose && r.Acknowledged {
			s.lose = false
			return nil, errors.New("completion checkpoint response lost")
		}
	}
	return s.operationMemoryStore.updateOperation(ctx, object)
}
func TestOperationLostACKControllerAdvanceObservesWithoutEffects(t *testing.T) {
	for _, mode := range []string{"complete", "missing ACK intent", "missing Arm proof", "changed node", "changed DPU", "missing claim", "replaced claim", "changed host boot", "changed Arm boot", "missing NodeEffectRemoved"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixture(t)
			op, memory := checkpointFixture(t, f)
			store := &operationCompletionFailureStore{operationMemoryStore: memory, lose: true}
			op.store = store
			f.prepared.operation = op
			client := recoveryActivationClient{f.client}
			testTenant(f.controlPlane.tenant.Client).source = client
			reboot := &recoveryRebooter{fixture: &f}
			run := func() error {
				_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, testJournal(f.prepared.execution), time.Millisecond, time.Second, reboot)
				return err
			}
			if err := run(); err == nil || store.lose {
				t.Fatalf("did not lose completion after ACK: %v", err)
			}
			f.prepared.operation = reopenOperation(t, memory)
			switch mode {
			case "missing ACK intent":
				for k, r := range f.prepared.operation.state.Resets {
					r.ACKIntent = false
					f.prepared.operation.state.Resets[k] = r
				}
			case "missing Arm proof":
				for k, r := range f.prepared.operation.state.Resets {
					r.ArmProofs = nil
					f.prepared.operation.state.Resets[k] = r
				}
			case "changed node":
				for _, selected := range f.client.activationSelected {
					node := f.client.base.resources[tenantStageKey(dpuNodeIdentity(f.prepared.execution.operatorNamespace, selected.nodeName))]
					node.SetUID("replaced")
				}
			case "changed DPU":
				f.client.dpus[0].SetUID("replaced")
			case "missing claim":
				f.client.guards = map[string]*unstructured.Unstructured{}
			case "replaced claim":
				for _, claim := range f.client.guards {
					claim.SetUID("bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff")
				}
			case "changed host boot":
				reboot.boot = "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff"
			case "changed Arm boot":
				_ = unstructured.SetNestedField(f.client.dpus[0].Object, "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff", "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
			case "missing NodeEffectRemoved":
				_ = unstructured.SetNestedSlice(f.client.dpus[0].Object, []any{conditionMap("NodeEffectReady", "True", "Success", 1), conditionMap("NodeEffectRemoved", "False", "Pending", 1), conditionMap("Ready", "True", "Success", 1)}, "status", "conditions")
			}
			patches, claims, submissions := len(f.client.patches), f.client.guardCalls, reboot.submissions
			err := run()
			if (mode == "complete") != (err == nil) {
				t.Fatalf("recovery mode %s: %v", mode, err)
			}
			if len(f.client.patches) != patches || f.client.guardCalls != claims || reboot.submissions != submissions {
				t.Fatal("recovery repeated an ACK, claim or reset")
			}
		})
	}
}

// Inject at the real host-stage checkpoint boundary, including a server commit
// whose response is lost. Each resumed known request must select inspection.
type operationHostFailureStore struct {
	*operationMemoryStore
	serial string
	result bool
	commit bool
	armed  bool
}

func (s *operationHostFailureStore) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	state, err := decodeOperation(object.Data["checkpoint.json"])
	if err != nil {
		return nil, err
	}
	host, known := state.Hosts[s.serial]
	if s.armed && known && (host.Result != nil) == s.result {
		s.armed = false
		s.fail, s.committedError = !s.commit, s.commit
		result, err := s.operationMemoryStore.updateOperation(ctx, object)
		s.fail, s.committedError = false, false
		return result, err
	}
	return s.operationMemoryStore.updateOperation(ctx, object)
}
func TestOperationPartialHostCheckpointFailureAndInspectOnlyRecovery(t *testing.T) {
	for _, resultFailure := range []bool{false, true} {
		for _, commit := range []bool{false, true} {
			name := map[bool]string{false: "second intent", true: "first result"}[resultFailure] + map[bool]string{false: " rejected", true: " committed response lost"}[commit]
			t.Run(name, func(t *testing.T) {
				f, activation, snapshot := hostNetworkStageFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
				op, memory := checkpointFixture(t, f)
				serial := "SERIAL-B"
				if resultFailure {
					serial = "SERIAL-A"
				}
				store := &operationHostFailureStore{operationMemoryStore: memory, serial: serial, result: resultFailure, commit: commit, armed: true}
				op.store = store
				f.prepared.operation = op
				configurations, inspections := 0, 0
				requests := map[string]sshprobe.HostNetworkRequest{}
				apply := func(_ context.Context, _ config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
					configurations++
					requests[request.Identity.AdapterSerial] = request
					return sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true, ChangedMTU: true}, nil
				}
				inspect := func(_ context.Context, _ config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
					inspections++
					if original, known := requests[request.Identity.AdapterSerial]; known && !sameOperationRequest(original, request) {
						t.Fatal("inspection changed original transaction")
					}
					return sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true, VerifiedRepeat: true}, nil
				}
				read := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
					return cloneHostNetworkStageSnapshot(snapshot), nil
				}
				run := func() ([]HostWorkloadNetworkResult, error) {
					return runHostWorkloadNetworkStage(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, activation, testJournal(f.prepared.execution), time.Second, read, apply, inspect)
				}
				results, err := run()
				if err == nil || store.armed || configurations != 1 || len(results) != 1 || !results[0].LeaseReverified {
					t.Fatalf("partial result/effect lost: results=%+v err=%v configs=%d", results, err, configurations)
				}
				updates := memory.updates
				if _, err := run(); err == nil || memory.updates != updates || configurations != 1 {
					t.Fatal("poisoned attempt continued host effects")
				}
				f.prepared.operation = reopenOperation(t, memory)
				before, _ := f.prepared.operation.hostTransaction("SERIAL-A")
				results, err = run()
				wantInspections := 1
				if commit && !resultFailure {
					wantInspections = 2
				}
				wantConfigurations := 2
				if commit && !resultFailure {
					wantConfigurations = 1
				}
				if err != nil || len(results) != 2 || inspections != wantInspections || configurations != wantConfigurations {
					t.Fatalf("recovery: results=%+v err=%v inspections=%d configs=%d", results, err, inspections, configurations)
				}
				after, _ := f.prepared.operation.hostTransaction("SERIAL-A")
				if before.Result != nil && (!reflect.DeepEqual(before.Result, after.Result) || after.Result.Configuration.VerifiedRepeat) {
					t.Fatal("repeat inspection overwrote original definitive provenance")
				}
			})
		}
	}
}

func TestOperationPhysicalSummarySurvivesObservationCheckpointFailure(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, memory := checkpointFixture(t, f)
	armed := true
	op.store = operationResponseStore{operationStore: memory, update: func(ctx context.Context, desired *corev1.ConfigMap) (*corev1.ConfigMap, error) {
		state, err := decodeOperation(desired.Data["checkpoint.json"])
		if err != nil {
			return nil, err
		}
		for _, reset := range state.Resets {
			if armed && reset.Observation.Reconnected {
				armed = false
				return nil, errors.New("physical observation checkpoint lost")
			}
		}
		return memory.updateOperation(ctx, desired)
	}}
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	receipt, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, testJournal(f.prepared.execution), time.Millisecond, time.Second, reboot)
	if err == nil || armed || reboot.submissions != 1 || len(receipt.Reboots) != 1 || receipt.Reboots[0].CurrentBoot != operationNewBoot || len(f.client.patches) != 1 {
		t.Fatalf("physical result lost or ACK continued: receipt=%+v err=%v submissions=%d patches=%d", receipt.Reboots, err, reboot.submissions, len(f.client.patches))
	}
}

// Fail the local confirmation after the conditional hold patch has succeeded.
// The returned proof must still reach the independent checkpoint owner.
type operationHoldConfirmationJournal struct{ bytes.Buffer }

func (f *operationHoldConfirmationJournal) Write(data []byte) (int, error) {
	var event executionEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, err
	}
	if event.Operation == "release-node-effect" && event.State == "confirmed" {
		return 0, errors.New("hold confirmation journal unavailable")
	}
	return f.Buffer.Write(data)
}
func (f *operationHoldConfirmationJournal) Sync() error  { return nil }
func (f *operationHoldConfirmationJournal) Close() error { return nil }
func TestOperationHoldConfirmationPersistsWhenLocalConfirmedJournalFails(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, memory := checkpointFixture(t, f)
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	journal := &executionJournal{file: &operationHoldConfirmationJournal{}, digest: f.prepared.execution.plan.Digest()}
	reboot := &recoveryRebooter{fixture: &f}
	receipt, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, journal, time.Millisecond, time.Second, reboot)
	saved := reopenOperation(t, memory).restoreProgress()
	if err == nil || journal.failed == nil || len(f.client.patches) != 1 || reboot.submissions != 0 || len(receipt.progress) != 1 || len(saved) != 1 {
		t.Fatalf("lost confirmed hold or continued effects: err=%v patches=%d resets=%d progress=%+v saved=%+v", err, len(f.client.patches), reboot.submissions, receipt.progress, saved)
	}
	for _, proof := range saved {
		if proof.releasedVersion == "" || proof.resetAcknowledged {
			t.Fatal("hold proof replaced by reset history")
		}
	}
}

type completedResetReappearanceRebooter struct {
	*recoveryRebooter
	pendingDPUs        []unstructured.Unstructured
	pendingMaintenance []unstructured.Unstructured
	pendingNode        *unstructured.Unstructured
}

func (r *completedResetReappearanceRebooter) RebootExternal(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
	proof, err := r.recoveryRebooter.RebootExternal(ctx, request)
	if err != nil {
		return proof, err
	}
	f := r.fixture
	r.pendingDPUs = deepCopyUnstructuredList(f.client.dpus)
	r.pendingMaintenance = deepCopyUnstructuredList(f.client.maint)
	r.pendingNode = f.client.base.resources[tenantStageKey(dpuNodeIdentity(f.prepared.execution.operatorNamespace, request.DPUNodes[0].NodeName))].DeepCopy()
	return proof, nil
}
func TestOperationAcknowledgedResetReappearanceCannotRepeatACK(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	op, memory := checkpointFixture(t, f)
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &completedResetReappearanceRebooter{recoveryRebooter: &recoveryRebooter{fixture: &f}}
	run := func() error {
		_, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, testJournal(f.prepared.execution), time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	for _, reset := range op.state.Resets {
		if !reset.Acknowledged {
			t.Fatal("history is not acknowledged")
		}
	}
	patches, claims, submissions := len(f.client.patches), f.client.guardCalls, reboot.submissions
	updates := memory.updates
	// Restore only the same controller pending tuple/maintenance participation,
	// keeping all immutable UIDs/generations, checkpoint, claim and physical boot.
	f.client.dpus = deepCopyUnstructuredList(reboot.pendingDPUs)
	f.client.maint = deepCopyUnstructuredList(reboot.pendingMaintenance)
	for i := range f.client.dpus {
		f.client.dpus[i].SetResourceVersion("100")
	}
	for i := range f.client.maint {
		f.client.maint[i].SetResourceVersion("100")
	}
	reboot.pendingNode.SetResourceVersion("100")
	f.client.base.resources[tenantStageKey(reboot.pendingNode)] = reboot.pendingNode.DeepCopy()
	f.prepared.operation = reopenOperation(t, memory)
	err := run()
	t.Logf("reappeared acknowledged tuple: err=%v patch delta=%d claim delta=%d reset delta=%d observations=%d", err, len(f.client.patches)-patches, f.client.guardCalls-claims, reboot.submissions-submissions, reboot.observations)
	if err == nil || !strings.Contains(err.Error(), "reset request already acknowledged") || len(f.client.patches) != patches || f.client.guardCalls != claims || reboot.submissions != submissions || reboot.observations != 0 || memory.updates != updates {
		t.Fatal("completed reset reappearance acquired observation/ACK/checkpoint authority")
	}
}

func TestOperationRejectedPhysicalObservationNeverReportsVerifiedReboot(t *testing.T) {
	for _, mode := range []string{"wrong saved baseline", "invalid previous boot UUID", "invalid current boot UUID"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationIntegrationFixture(t)
			op, _ := checkpointFixture(t, f)
			f.prepared.operation = op
			client := recoveryActivationClient{f.client}
			testTenant(f.controlPlane.tenant.Client).source = client
			physical := &recoveryRebooter{fixture: &f}
			reboot := RebooterFunc(func(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
				proof, err := physical.RebootExternal(ctx, request)
				switch mode {
				case "wrong saved baseline":
					proof.PreviousBootID = "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff"
				case "invalid previous boot UUID":
					proof.PreviousBootID = "invalid"
				case "invalid current boot UUID":
					proof.CurrentBootID = "invalid"
				}
				return proof, err
			})
			receipt, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, testJournal(f.prepared.execution), time.Millisecond, time.Second, reboot)
			summary := activationSummary(receipt)
			if err == nil || len(receipt.Reboots) != 0 || summary == nil || len(summary.RebootedHosts) != 0 || physical.submissions != 1 || len(f.client.patches) != 1 {
				t.Fatalf("rejected observation was published or ACKed: err=%v receipt=%+v summary=%+v resets=%d patches=%d", err, receipt.Reboots, summary, physical.submissions, len(f.client.patches))
			}
		})
	}
}
