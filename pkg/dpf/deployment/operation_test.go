// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const operationOldBoot = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
const operationNewBoot = "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff"

type operationMemoryStore struct {
	object         *corev1.ConfigMap
	fail           bool
	committedError bool
	updates        int
	noops          int
}

func (s *operationMemoryStore) getOperation(context.Context, string, string) (*corev1.ConfigMap, error) {
	return s.object.DeepCopy(), nil
}
func (s *operationMemoryStore) createOperation(_ context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if s.object != nil {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, object.Name)
	}
	s.object = object.DeepCopy()
	s.object.UID = activationIntegrationGuardUID
	s.object.ResourceVersion = "1"
	return s.object.DeepCopy(), nil
}
func (s *operationMemoryStore) updateOperation(_ context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	s.updates++
	if s.fail {
		return nil, errors.New("checkpoint unavailable")
	}
	if s.object == nil || object.UID != s.object.UID || object.ResourceVersion != s.object.ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, object.Name, errors.New("stale version"))
	}
	if reflect.DeepEqual(object, s.object) {
		// Model API storage eliding a byte-identical update after its CAS check.
		s.noops++
	} else {
		version, _ := strconv.Atoi(s.object.ResourceVersion)
		s.object = object.DeepCopy()
		s.object.ResourceVersion = strconv.Itoa(version + 1)
	}
	if s.committedError {
		return nil, errors.New("checkpoint response lost")
	}
	return s.object.DeepCopy(), nil
}

func checkpointFixture(t *testing.T, fixture activationIntegrationFixture) (*deploymentOperation, *operationMemoryStore) {
	t.Helper()
	deployment, err := retainedDeploymentObject(fixture.prepared.execution)
	if err != nil {
		t.Fatal(err)
	}
	state := operationState{Binding: operationBinding{Version: 1, ID: "test-operation", BundleDigest: fixture.prepared.execution.plan.Digest(), Management: fixture.prepared.execution.plan.Intent().Management, NamespaceUID: "operator-namespace-uid", DeploymentName: deployment.GetName(), CohortDigest: inventory.Digest("dpf-operation-cohort-v1", fixture.prepared.execution.plan.Intent().Devices)},
		Management: fixture.controlPlane.management, TenantUID: fixture.controlPlane.tenant.ClusterUID, TenantSystemUID: fixture.controlPlane.tenant.Client.systemUID, TenantSources: map[string]types.UID{"Secret/tenant/source": "source-uid"}, Enrollment: fixture.controlPlane.enrollment, Software: fixture.workload.Software, Workload: fixture.workload.Resources,
		Children: map[string]activationObjectReceipt{}, HoldIntents: map[string]operationProgress{}, Progress: map[string]operationProgress{}, Resets: map[string]operationReset{}, Hosts: map[string]operationHost{}}
	data, err := encodeOperation(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOperation(data); err != nil {
		t.Fatal(err)
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: deployment.GetNamespace(), Name: operationName(deployment), UID: activationIntegrationGuardUID, ResourceVersion: "1", Labels: map[string]string{operationLabel: "v1"}}, Data: map[string]string{"checkpoint.json": data}}
	store := &operationMemoryStore{object: object.DeepCopy()}
	return &deploymentOperation{store: store, object: object, state: state}, store
}
func reopenOperation(t *testing.T, store *operationMemoryStore) *deploymentOperation {
	t.Helper()
	state, err := decodeOperation(store.object.Data["checkpoint.json"])
	if err != nil {
		t.Fatal(err)
	}
	return &deploymentOperation{store: store, object: store.object.DeepCopy(), state: state}
}

type recoveryActivationClient struct{ *activationIntegrationFake }

func (c recoveryActivationClient) Get(ctx context.Context, want *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if want.GetKind() == "ConfigMap" && strings.HasPrefix(want.GetName(), activationRebootGuardName) {
		if object := c.guards[want.GetName()]; object != nil {
			return object.DeepCopy(), nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, want.GetName())
	}
	return c.activationIntegrationFake.Get(ctx, want)
}

type recoveryRebooter struct {
	fixture                    *activationIntegrationFixture
	submissions, observations  int
	loseResponse, omitBaseline bool
	pauseBeforeDispatch        bool
	afterObservation           func()
	beforeBaseline             func()
	boot                       string
}

func (r *recoveryRebooter) RebootExternal(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
	if r.beforeBaseline != nil {
		r.beforeBaseline()
	}
	if !r.omitBaseline && request.BeforeSubmit != nil {
		if err := request.BeforeSubmit(ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: operationOldBoot}); err != nil {
			return ActivationObservation{}, err
		}
	}
	if r.pauseBeforeDispatch {
		return ActivationObservation{}, errors.New("paused after durable baseline before dispatch")
	}
	r.submissions++
	r.fixture.client.completeReboot(request.DPUNodes[0].NodeName)
	r.boot = operationNewBoot
	if r.loseResponse {
		return ActivationObservation{}, errors.New("submitted, response lost")
	}
	return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: operationOldBoot, CurrentBootID: r.boot, Reconnected: true}, nil
}
func (r *recoveryRebooter) ObserveExternal(_ context.Context, request ActivationRequest, baseline string) (ActivationObservation, error) {
	r.observations++
	if r.afterObservation != nil {
		r.afterObservation()
	}
	if baseline != operationOldBoot || r.boot != operationNewBoot {
		return ActivationObservation{}, fmt.Errorf("no changed host boot")
	}
	return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial, PreviousBootID: baseline, CurrentBootID: r.boot, Reconnected: true}, nil
}

func TestOperationResumeActualActivationAfterLostResetResponse(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture, loseResponse: true}
	run := func() (activationStageReceipt, error) {
		return runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
	}
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "EXTERNAL_HOST_REBOOT_UNCERTAIN") {
		t.Fatalf("lost dispatch result=%v", err)
	}
	if reboot.submissions != 1 || len(fixture.client.patches) != 1 || len(fixture.client.guards) != 1 {
		t.Fatalf("first attempt submissions=%d patches=%v guards=%d", reboot.submissions, fixture.client.patches, len(fixture.client.guards))
	}
	fixture.prepared.operation = reopenOperation(t, store)
	reboot.loseResponse = false
	result, err := run()
	if err != nil {
		t.Fatalf("actual stage recovery: %v", err)
	}
	if result.DeploymentUID != activationIntegrationDeploymentUID || len(result.DPUs) != 1 || reboot.submissions != 1 || reboot.observations != 1 || fixture.client.guardCalls != 1 || len(fixture.client.patches) != 2 {
		t.Fatalf("receipt=%+v submissions=%d observations=%d claims=%d patches=%v", result, reboot.submissions, reboot.observations, fixture.client.guardCalls, fixture.client.patches)
	}
	completed := reopenOperation(t, store)
	for _, reset := range completed.state.Resets {
		if !reset.Acknowledged || !reset.ACKIntent || len(reset.ArmProofs) != 1 || reset.Baseline != operationOldBoot {
			t.Fatalf("incomplete durable reset proof: %+v", reset)
		}
	}
	// A later validation retry restores history and performs no hold/reset effects.
	fixture.prepared.operation = completed
	beforePatches, beforeClaims, beforeCheckpointWrites := len(fixture.client.patches), fixture.client.guardCalls, store.updates
	third, err := run()
	if err != nil {
		t.Fatalf("completed operation revalidation: %v", err)
	}
	if summary := activationSummary(third); summary == nil || len(summary.RebootedHosts) != 1 {
		t.Fatal("resume lost confirmed physical reboot history")
	}
	if len(fixture.client.patches) != beforePatches || fixture.client.guardCalls != beforeClaims || reboot.submissions != 1 || store.updates != beforeCheckpointWrites {
		t.Fatal("completed operation replayed activation or rewrote checkpoint")
	}
}

func TestOperationClaimWithoutBaselineCannotReplayOrAcknowledge(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture, loseResponse: true, omitBaseline: true}
	run := func() error {
		_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil {
		t.Fatal("lost response succeeded")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	if err := run(); err == nil || !strings.Contains(err.Error(), "RESET_ACTION_REQUIRED") {
		t.Fatalf("missing baseline recovery=%v", err)
	}
	if reboot.submissions != 1 || reboot.observations != 0 || len(fixture.client.patches) != 1 || fixture.client.guardCalls != 1 {
		t.Fatal("uncertain claim replayed or acknowledged")
	}
}

func TestOperationCheckpointCASAndTamperFailClosed(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	first, store := checkpointFixture(t, fixture)
	stale := reopenOperation(t, store)
	first.state.DeploymentUID = activationIntegrationDeploymentUID
	if err := first.save(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := stale.save(fixture.ctx); err == nil || !stale.failed {
		t.Fatal("stale checkpoint writer retained authority")
	}
	updates := store.updates
	if err := stale.save(fixture.ctx); err == nil || updates != store.updates {
		t.Fatal("poisoned attempt retried its CAS")
	}
	store.committedError = true
	lost := reopenOperation(t, store)
	if err := lost.save(fixture.ctx); err == nil || !lost.failed {
		t.Fatal("committed checkpoint with lost response retained writer authority")
	}
	store.committedError = false
	if reopened := reopenOperation(t, store); reopened.state.Binding != first.state.Binding {
		t.Fatal("reopened checkpoint lost immutable operation binding")
	}
	valid := store.object.Data["checkpoint.json"]
	for _, data := range []string{valid + "{}", strings.Replace(valid, `"Version":1`, `"Version":2`, 1), strings.Replace(valid, `"Binding":{`, `"password":"must-not-be-accepted","Binding":{`, 1), strings.Repeat("x", maxOperationBytes+1)} {
		if _, err := decodeOperation(data); err == nil {
			t.Fatal("malformed/unrecognized checkpoint accepted")
		}
	}
	// Unknown reset/host input fields cannot be smuggled into the record schema.
	var object map[string]any
	if err := json.Unmarshal([]byte(valid), &object); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(valid, `"Password":`) || strings.Contains(valid, "super-admin.conf") || strings.Contains(valid, "CredentialDigest") || strings.Contains(valid, "BeforeSubmit") {
		t.Fatal("private access leaked into checkpoint")
	}
	ref := first.ref()
	parsed, err := ParseOperationRef(ref.String())
	if err != nil || !reflect.DeepEqual(ref, parsed) {
		t.Fatalf("reference=%v err=%v", parsed, err)
	}
	for _, value := range []string{"", "a/b", "a/b/c", ref.String() + "/extra"} {
		if _, err := ParseOperationRef(value); err == nil {
			t.Fatalf("invalid reference %q accepted", value)
		}
	}
}

func TestOperationHostLostAcknowledgementInspectsOriginalRequest(t *testing.T) {
	fixture, activation, snapshot := hostNetworkStageFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	reader := func(context.Context) (hostWorkloadLeaseSnapshot, error) {
		return cloneHostNetworkStageSnapshot(snapshot), nil
	}
	effects, inspections := 0, 0
	var submitted sshprobe.HostNetworkRequest
	configure := func(_ context.Context, _ config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
		effects++
		submitted = request
		return sshprobe.HostNetworkReceipt{}, errors.New("response lost")
	}
	inspect := func(_ context.Context, _ config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
		inspections++
		if !sameOperationRequest(request, submitted) {
			t.Fatal("recovery generated a different semantic host transaction")
		}
		return sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true, VerifiedRepeat: true}, nil
	}
	if _, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation, testJournal(fixture.prepared.execution), time.Second, reader, configure, inspect); err == nil {
		t.Fatal("lost host result succeeded")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	results, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation, testJournal(fixture.prepared.execution), time.Second, reader, configure, inspect)
	if err != nil || len(results) != 1 || !results[0].LeaseReverified || effects != 1 || inspections != 1 {
		t.Fatalf("results=%+v err=%v effects=%d inspections=%d", results, err, effects, inspections)
	}
	fixture.prepared.operation = reopenOperation(t, store)
	for key, value := range fixture.prepared.operation.state.Hosts {
		value.Request.MTU++
		fixture.prepared.operation.state.Hosts[key] = value
	}
	if _, err := runHostWorkloadNetworkStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload, activation, testJournal(fixture.prepared.execution), time.Second, reader, configure, inspect); err == nil || !strings.Contains(err.Error(), "HOST_NETWORK_OPERATION_CHANGED") {
		t.Fatalf("changed host binding=%v", err)
	}
	if effects != 1 || inspections != 1 {
		t.Fatal("changed host request reached transport")
	}
}

func TestOperationPausedClaimResumerOnlyObserves(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture, pauseBeforeDispatch: true, boot: operationOldBoot}
	run := func() error {
		_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil {
		t.Fatal("paused original action reported complete")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	if err := run(); err == nil {
		t.Fatal("unchanged boot claim reported recovered")
	}
	if reboot.submissions != 0 || reboot.observations != 1 || fixture.client.guardCalls != 1 || len(fixture.client.patches) != 1 {
		t.Fatalf("submissions=%d observations=%d claims=%d patches=%v", reboot.submissions, reboot.observations, fixture.client.guardCalls, fixture.client.patches)
	}
}

func TestOperationResumeRequiresNewCurrentArmNotInitialBoot(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	client := &stopAfterArmPollClient{recoveryActivationClient: recoveryActivationClient{fixture.client}}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture, loseResponse: true}
	if _, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot); err == nil {
		t.Fatal("lost response succeeded")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	for _, reset := range fixture.prepared.operation.state.Resets {
		for _, old := range reset.Rounds {
			for i := range fixture.client.dpus {
				current := &fixture.client.dpus[i]
				if current.GetUID() != old.UID {
					continue
				}
				_ = unstructured.SetNestedField(current.Object, old.KernelBoot, "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
				_ = unstructured.SetNestedField(current.Object, old.Startup.Format(time.RFC3339Nano), "status", "agentStatus", "lastStartupTime")
				_ = unstructured.SetNestedField(current.Object, operationNewBoot, "status", "agentStatus", "initialBootID")
			}
		}
	}
	reboot.afterObservation = func() { client.armed = true }
	_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
	if err == nil || !strings.Contains(err.Error(), "EXTERNAL_REBOOT_AGENT_UNVERIFIED") || client.polls != 2 {
		t.Fatalf("unchanged current Arm recovery=%v", err)
	}
	if reboot.submissions != 1 || fixture.client.guardCalls != 1 || len(fixture.client.patches) != 1 {
		t.Fatal("missing current Arm proof allowed replay or ACK")
	}
}

type failProgressOperationStore struct {
	*operationMemoryStore
	failed bool
}

func (s *failProgressOperationStore) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	state, err := decodeOperation(object.Data["checkpoint.json"])
	if err != nil {
		return nil, err
	}
	if !s.failed && len(state.Progress) > 0 {
		s.failed = true
		return nil, errors.New("crash after hold patch before confirmed checkpoint")
	}
	return s.operationMemoryStore.updateOperation(ctx, object)
}
func TestOperationLostHoldConfirmationNeverInfersHistory(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, memory := checkpointFixture(t, fixture)
	operation.store = &failProgressOperationStore{operationMemoryStore: memory}
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture}
	run := func() error {
		_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil {
		t.Fatal("lost hold checkpoint permitted further effects")
	}
	fixture.prepared.operation = reopenOperation(t, memory)
	if err := run(); err == nil || !strings.Contains(err.Error(), "HOLD_ACTION_REQUIRED") {
		t.Fatalf("lost hold confirmation=%v", err)
	}
	if reboot.submissions != 0 || len(fixture.client.patches) != 1 || fixture.client.guardCalls != 0 {
		t.Fatal("unknown hold history permitted reset")
	}
}

func TestOperationRuntimeRetryMeasuresEveryRowWithoutEffects(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	operation, store := checkpointFixture(t, f.activationIntegrationFixture)
	f.prepared.operation = operation
	operation.state.DeploymentUID = f.activation.DeploymentUID
	if err := operation.saveProgress(f.ctx, f.activation.progress); err != nil {
		t.Fatal(err)
	}
	beforePatches, beforeClaims, beforeCheckpoints := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls, store.updates
	f.trafficHook = func(_ int, result *sshprobe.HostTrafficResult) {
		result.State = sshprobe.TrafficProbeFailed
		result.Ordinary.State = sshprobe.TrafficProbeFailed
		result.Ordinary.Counters.Received = 2
		result.Ordinary.Counters.LostPct = 33.3333
	}
	first, err := f.run(testJournal(f.prepared.execution))
	if err == nil || first.TrafficValidated {
		t.Fatal("failed runtime attempt passed")
	}
	f.prepared.operation = reopenOperation(t, store)
	priorCommands, priorTraffic := f.commandReads, f.trafficReads
	f.trafficHook = nil
	second, err := f.run(testJournal(f.prepared.execution))
	if err != nil || !second.TrafficValidated || len(second.BGP) != 2 || len(second.Routes) != 2 || len(second.Traffic) != 2 || f.commandReads-priorCommands != 4 || f.trafficReads-priorTraffic != 2 {
		t.Fatalf("fresh runtime matrix=%+v err=%v", second, err)
	}
	if len(f.activationIntegrationFixture.client.patches) != beforePatches || f.activationIntegrationFixture.client.guardCalls != beforeClaims || store.updates != beforeCheckpoints {
		t.Fatal("runtime retry invoked activation or checkpoint mutation")
	}
}

type lostDDCreateClient struct {
	recoveryActivationClient
	lose bool
}

func (c *lostDDCreateClient) Create(ctx context.Context, object *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	created, err := c.recoveryActivationClient.Create(ctx, object, dryRun)
	if err == nil && object.GetKind() == "DPUDeployment" && c.lose {
		c.lose = false
		return nil, errors.New("DD committed, response lost")
	}
	return created, err
}
func TestOperationResolvesOwnPreparedDDCreateWithoutAdoptingReplacement(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = reopenOperation(t, store)
	sourceReads := 0
	fixture.prepared.beforeDeploymentCreate = func(context.Context) error { sourceReads++; return nil }
	client := &lostDDCreateClient{recoveryActivationClient: recoveryActivationClient{fixture.client}, lose: true}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture}
	run := func() error {
		_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil {
		t.Fatal("lost DD create result succeeded")
	}
	if fixture.client.deployment == nil || operation.state.DeploymentUID != "" || reboot.submissions != 0 || len(fixture.client.patches) != 0 {
		t.Fatal("uncertain DD creation reached dependent effects")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	if err := run(); err != nil {
		t.Fatalf("own prepared DD create recovery=%v", err)
	}
	if sourceReads != 1 || reboot.submissions != 1 || fixture.prepared.operation.state.DeploymentUID != activationIntegrationDeploymentUID {
		t.Fatal("prepared DD UID was not durably resolved")
	}
	fixture.prepared.operation = reopenOperation(t, store)
	fixture.client.deployment.SetUID("123e4567-e89b-12d3-a456-426614174099")
	beforePatches, beforeClaims := len(fixture.client.patches), fixture.client.guardCalls
	if err := run(); err == nil || !strings.Contains(err.Error(), "OPERATION_PARENT_CHANGED") {
		t.Fatalf("replacement DD recovery=%v", err)
	}
	if len(fixture.client.patches) != beforePatches || fixture.client.guardCalls != beforeClaims || reboot.submissions != 1 {
		t.Fatal("replacement DD reached effects")
	}
}

// Stop transport only on the second agent-wait poll. The first complete poll
// must reject unchanged current Arm even when initialBootID looks different.
type stopAfterArmPollClient struct {
	recoveryActivationClient
	armed bool
	polls int
}

func (c *stopAfterArmPollClient) Get(ctx context.Context, want *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if c.armed && want.GetKind() == "DPUDeployment" {
		c.polls++
		if c.polls == 2 {
			return nil, errors.New("transport unavailable after first unchanged-current-Arm poll")
		}
	}
	return c.recoveryActivationClient.Get(ctx, want)
}

func TestOperationCannotRebindRecreatedClaimBeforeSubmission(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, _ := checkpointFixture(t, fixture)
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture}
	reboot.beforeBaseline = func() {
		for name, claim := range fixture.client.guards {
			replacement := claim.DeepCopy()
			replacement.SetUID("123e4567-e89b-12d3-a456-426614174099")
			fixture.client.guards[name] = replacement
		}
	}
	_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
	if err == nil || reboot.submissions != 0 || len(fixture.client.patches) != 1 || fixture.client.guardCalls != 1 {
		t.Fatalf("recreated claim err=%v submissions=%d patches=%v", err, reboot.submissions, fixture.client.patches)
	}
	for _, reset := range operation.state.Resets {
		if reset.ClaimUID != activationIntegrationGuardUID || reset.Baseline != "" {
			t.Fatal("replacement claim was rebound or reached host baseline")
		}
	}
}

func TestOperationFirstDDSourceFailureBlocksCreateWithoutStageReplay(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, store := checkpointFixture(t, fixture)
	fixture.prepared.operation = reopenOperation(t, store)
	reads := 0
	fixture.prepared.beforeDeploymentCreate = func(context.Context) error { reads++; return errors.New("fresh source byte check failed") }
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture}
	_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
	if err == nil || reads != 1 || fixture.client.deployment != nil || len(fixture.client.patches) != 0 || fixture.client.guardCalls != 0 || reboot.submissions != 0 || operation.state.DeploymentUID != "" {
		t.Fatalf("fresh source failure err=%v reads=%d parent=%v effects=%v", err, reads, fixture.client.deployment, fixture.client.patches)
	}
}

func TestOperationFirstDDFreshnessUsesActualReadOnlyByteAndChartHelpers(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	client := fixture.client
	chartReads := 0
	chart := func(context.Context, *generation.Plan) (PreflightCheck, error) {
		chartReads++
		return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "current chart matched"}, nil
	}
	streams, creates := client.streamRequests.Load(), client.createSoftware
	if err := verifyFirstDeploymentSources(fixture.ctx, client, fixture.prepared.execution, fixture.workload.Software, chart, fixture.journal, 1<<20); err != nil {
		t.Fatal(err)
	}
	if chartReads != 1 || client.streamRequests.Load()-streams != int32(len(fixture.workload.Software.Components)) || client.createSoftware != creates {
		t.Fatal("freshness did not read exact cached components without creation")
	}
	client.streamMismatch.Store(true)
	if err := verifyFirstDeploymentSources(fixture.ctx, client, fixture.prepared.execution, fixture.workload.Software, chart, fixture.journal, 1<<20); err == nil || !strings.Contains(err.Error(), "RESUME_SOFTWARE_BYTES_UNVERIFIED") {
		t.Fatalf("changed actual cached bytes=%v", err)
	}
	streams = client.streamRequests.Load()
	blockedChart := func(context.Context, *generation.Plan) (PreflightCheck, error) {
		return PreflightCheck{}, errors.New("source unavailable")
	}
	if err := verifyFirstDeploymentSources(fixture.ctx, client, fixture.prepared.execution, fixture.workload.Software, blockedChart, fixture.journal, 1<<20); err == nil {
		t.Fatal("unavailable chart passed")
	}
	if client.streamRequests.Load() != streams || client.createSoftware != creates {
		t.Fatal("failed chart invoked software streaming or creation")
	}
}
