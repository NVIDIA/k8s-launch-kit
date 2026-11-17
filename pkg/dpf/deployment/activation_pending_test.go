// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Recovery storage must expose claims already created by the actual invocation;
// all other reads and transition hooks remain the existing lifecycle fixture.
type pendingActivationClient struct {
	*activationLifecycleClient
	listFailure error
}

func (c *pendingActivationClient) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if desired.GetKind() == "ConfigMap" && strings.HasPrefix(desired.GetName(), activationRebootGuardName) {
		return (recoveryActivationClient{c.activationIntegrationFake}).Get(ctx, desired)
	}
	return c.activationLifecycleClient.Get(ctx, desired)
}

func (c *pendingActivationClient) List(ctx context.Context, desired *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	result, err := c.activationLifecycleClient.List(ctx, desired, selector)
	if desired.GetKind() == "DPUNodeMaintenance" && c.listFailure != nil {
		return nil, c.listFailure
	}
	return result, err
}

func TestActivationPendingCohortDefersRemainingHostWithoutReplayingCompletedPeer(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	cases := []struct {
		name        string
		completed   int
		afterSafety bool
		mutation    string
	}{
		{name: "first peer completes during initial scope", completed: 0},
		{name: "second peer completes during initial scope", completed: 1},
		{name: "first peer completes after safety", completed: 0, afterSafety: true},
		{name: "second peer completes after safety", completed: 1, afterSafety: true},
		{name: "replacement is fatal", mutation: "UID"},
		{name: "denied list is fatal", mutation: "denied"},
		{name: "denied list after safety is fatal", mutation: "denied", afterSafety: true},
		{name: "missing target request is fatal", mutation: "request"},
		{name: "canceled incomplete read is fatal", mutation: "cancel"},
		{name: "canceled incomplete read after safety is fatal", mutation: "cancel", afterSafety: true},
		{name: "persistent incomplete cohort reaches deadline", mutation: "incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			life := &activationLifecycleClient{activationIntegrationFake: f.client}
			c := &pendingActivationClient{activationLifecycleClient: life}
			bindActivationLifecycleClient(&f, life)
			testTenant(f.controlPlane.tenant.Client).source = c
			desired, err := retainedDeploymentObject(execution.verificationInputs())
			if err != nil {
				t.Fatal(err)
			}
			created, _, err := execution.reconciler.Ensure(f.ctx, c, desired)
			if err != nil {
				t.Fatal(err)
			}
			for i := range c.maint {
				a := c.maint[i].GetAnnotations()
				a[activationHoldKey] = "false"
				c.maint[i].SetAnnotations(a)
			}
			selected := c.activationSelected
			remaining := 1 - tc.completed
			peer := selected[tc.completed]
			target := selected[remaining]
			observed, err := observeActivationScope(f.ctx, c, execution.verificationInputs(), desired, created.GetUID(), selected, nil)
			if err != nil || !observed.complete {
				t.Fatalf("initial scope: %v", err)
			}
			progress := map[string]activationNodeProgress{}
			for _, item := range selected {
				p, e := activationProgressForNode(observed, selected, item.nodeName, "released", false)
				if e != nil {
					t.Fatal(e)
				}
				progress[item.nodeName] = p
			}
			op, store := checkpointFixture(t, f)
			f.prepared.operation = op
			op.state.DeploymentUID = created.GetUID()
			objects, err := retainedWorkloadObjects(execution.verificationInputs())
			if err != nil {
				t.Fatal(err)
			}
			base := &recoveryRebooter{fixture: &f}
			requests := []string{}
			reboot := RebooterFunc(func(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
				requests = append(requests, request.HostInputRef)
				return base.RebootExternal(ctx, request)
			})
			invoke := func(ctx context.Context, index int, current activationObservation) ([]activationRebootReceipt, error) {
				return invokeSelectedHostReboots(ctx, c, f.prepared, f.controlPlane, f.workload, selected, map[string][]int{selected[index].host.ID: {index}}, objects, desired, created.GetUID(), current, progress, reboot, f.journal, time.Millisecond, time.Second)
			}
			// Establish the completed peer through the real claim, BeforeSubmit, changed
			// host/Arm proof, conditional ACK and durable operation progress path first.
			done, err := invoke(f.ctx, tc.completed, observed)
			if err != nil || len(done) != 1 {
				t.Fatalf("completed peer: receipts=%d err=%v", len(done), err)
			}
			for _, p := range done[0].completion {
				progress[p.nodeName] = p
			}
			if base.submissions != 1 || c.guardCalls != 1 {
				t.Fatal("peer did not actually reset once")
			}
			// The fake ACK normally finishes cleanup immediately. Expose the real
			// intermediate NodeEffectRemoval state and its still-present maintenance.
			for _, old := range c.removedMaintenance {
				if old.GetLabels()[activationDPUNodeLabel] == peer.nodeName {
					c.maint = append(c.maint, *old.DeepCopy())
				}
			}
			for i := range c.dpus {
				if c.dpus[i].GetName() == peer.dpuName {
					_ = unstructured.SetNestedField(c.dpus[i].Object, "Node Effect Removal", "status", "phase")
					_ = unstructured.SetNestedSlice(c.dpus[i].Object, []any{conditionMap("Ready", "False", "Progressing", 1), conditionMap("NodeEffectRemoved", "False", "Progressing", 1)}, "status", "conditions")
				}
			}
			observed, err = observeActivationScope(f.ctx, c, execution.verificationInputs(), desired, created.GetUID(), selected, progress)
			if err != nil || !observed.complete {
				t.Fatalf("pre-transition: %v", err)
			}
			var output bytes.Buffer
			ctx, cancel := context.WithCancel(ui.WithOutput(f.ctx, ui.NewWithWriter(&output)))
			defer cancel()
			safetyBefore := c.resetSafetyCalls
			patchesBefore := len(c.patches)
			crossed := false
			c.beforeList = func(client *activationIntegrationFake, kind string) {
				if kind != "DPUNodeMaintenance" || crossed || tc.afterSafety && client.resetSafetyCalls == safetyBefore {
					return
				}
				crossed = true
				switch tc.mutation {
				case "denied":
					c.listFailure = errors.New("private-scope-list-canary")
				case "request":
					node := client.base.resources["DPUNode/"+desired.GetNamespace()+"/"+target.nodeName]
					a := node.GetAnnotations()
					delete(a, activationExternalRebootKey)
					node.SetAnnotations(a)
				default:
					if tc.mutation == "incomplete" {
						client.maint = nil
					} else {
						completeActivationWithoutReset(client, peer.nodeName)
					}
					if tc.mutation == "cancel" {
						cancel()
					}
				}
			}
			if tc.mutation == "UID" {
				// Mutate before the DPU List is captured, after its parent GET establishes
				// that this is an actual scope observation rather than a resource fence.
				armed := false
				life.onGet = func(_ *activationIntegrationFake, o *unstructured.Unstructured) {
					if o.GetKind() == "DPUDeployment" {
						armed = true
					}
				}
				life.beforeList = func(client *activationIntegrationFake, kind string) {
					if armed && kind == "DPU" && !crossed {
						crossed = true
						for i := range client.dpus {
							if client.dpus[i].GetName() == peer.dpuName {
								client.dpus[i].SetUID("replacement-dpu")
							}
						}
					}
				}
			}
			receipts, err := invoke(ctx, remaining, observed)
			if !crossed || len(receipts) != 0 || base.submissions != 1 || c.guardCalls != 1 || len(c.patches) != patchesBefore {
				t.Fatalf("pending scope crossed an effect: crossed=%v receipts=%d submissions=%d claims=%d patches=%v err=%v", crossed, len(receipts), base.submissions, c.guardCalls, c.patches, err)
			}
			if tc.mutation != "" && tc.mutation != "incomplete" {
				if err == nil {
					t.Fatal("explicit error/request drift was treated as pending")
				}
				if tc.mutation == "denied" && (!errors.Is(err, c.listFailure) || strings.Contains(err.Error()+f.journal.file.(*managementMemoryFile).String()+output.String(), "private-scope-list-canary")) {
					t.Fatal("private read cause lost or exposed")
				}
				if tc.mutation == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel lost: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("incomplete observation should defer without effect: %v", err)
			}
			report := f.journal.file.(*managementMemoryFile).String()
			if !strings.Contains(report, `"state":"waiting"`) || !strings.Contains(report, "ACTIVATION_REBOOT_SCOPE_PENDING") || !strings.Contains(output.String(), "ACTIVATION_REBOOT_SCOPE_PENDING") {
				t.Fatal("pending reason missing from durable journal/progress")
			}
			expectedResets := 1
			if tc.afterSafety {
				expectedResets++
			}
			if len(op.state.Resets) != expectedResets {
				t.Fatalf("unexpected request checkpoint count: %d", len(op.state.Resets))
			}
			for _, reset := range op.state.Resets {
				if reset.Request.HostInputRef == target.host.InputRef && (reset.ClaimUID != "" || reset.Baseline != "" || reset.Observation.CurrentBootID != "") {
					t.Fatal("request-only pending checkpoint acquired effect proof")
				}
			}
			// Reopen the persisted checkpoint, then let the existing outer loop collect
			// afresh. It must complete only the remaining host, not replay the peer.
			f.prepared.operation = reopenOperation(t, store)
			if tc.mutation == "incomplete" {
				deadline, stop := context.WithTimeout(f.ctx, time.Second)
				defer stop()
				_, err = runActivationStage(deadline, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
				if err == nil || !errors.Is(err, context.DeadlineExceeded) || base.submissions != 1 || c.guardCalls != 1 {
					t.Fatalf("incomplete scope did not stop boundedly: %v", err)
				}
				return
			}
			coherent, err := observeActivationScope(f.ctx, c, execution.verificationInputs(), desired, created.GetUID(), selected, progress)
			if err != nil || !coherent.complete || !coherent.removed[peer.nodeName] {
				t.Fatalf("normal progress did not settle: %v", err)
			}
			result, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot)
			if err != nil || len(result.DPUs) != 2 || base.submissions != 2 || c.guardCalls != 2 || !slices.Equal(requests, []string{peer.host.InputRef, target.host.InputRef}) {
				t.Fatalf("remaining host did not finish once: requests=%v claims=%d submissions=%d err=%v", requests, c.guardCalls, base.submissions, err)
			}
		})
	}
}
