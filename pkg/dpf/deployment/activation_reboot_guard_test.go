// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const rebootGuardBundle = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const rebootGuardUID = "123e4567-e89b-12d3-a456-426614174000"

type rebootGuardClient struct {
	created  *unstructured.Unstructured
	err      error
	calls    int
	response func(*unstructured.Unstructured) *unstructured.Unstructured
}

func (c *rebootGuardClient) Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, errors.New("unexpected get")
}

func (c *rebootGuardClient) Create(_ context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.calls++
	if dryRun {
		return nil, errors.New("unexpected dry run")
	}
	c.created = desired.DeepCopy()
	if c.err != nil {
		return nil, c.err
	}
	if c.response != nil {
		created := c.response(desired.DeepCopy())
		c.created = created.DeepCopy()
		return created, nil
	}
	created := desired.DeepCopy()
	created.SetUID(types.UID(rebootGuardUID))
	created.SetResourceVersion("7")
	c.created = created.DeepCopy()
	return created, nil
}

type rebootGuardExecutor struct {
	calls   int
	journal string
	err     error
}

func (r *rebootGuardExecutor) RebootExternal(_ context.Context, _ ActivationRequest) (ActivationObservation, error) {
	r.calls++
	data, err := os.ReadFile(r.journal)
	if err != nil {
		return ActivationObservation{}, err
	}
	content := string(data)
	if !strings.Contains(content, `"reasonCode":"EXTERNAL_REBOOT_GUARD_CREATED"`) || !strings.Contains(content, `"reasonCode":"EXTERNAL_HOST_REBOOT_STARTED"`) {
		return ActivationObservation{}, errors.New("reboot callback ran before the durable claim and action records")
	}
	return ActivationObservation{PreviousBootID: "old", CurrentBootID: "new", Reconnected: true}, r.err
}

func rebootGuardDeployment() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUDeployment",
		"metadata": map[string]any{"name": "hbn", "namespace": "dpf", "uid": "deployment-uid"},
	}}
}

func rebootGuardRequest() ActivationRequest {
	return ActivationRequest{
		HostInputRef: "/discovery/hosts/0", HostEndpoint: "ssh://host.example:22", HostSystemUUID: "12345678-1234-1234-1234-123456789abc",
		HostAdapterKey: "adapter-a", HostAdapterSerial: "CARD-A", ResetSafetyExpiresAt: time.Now().Add(time.Minute),
		DPUNodes: []ActivationNode{
			{NodeName: "dpu-node-b", NodeUID: "node-b", RebootMethod: "SystemLevelReset", SelectedDPUs: []ActivationDPU{{SerialNumber: "CARD-B", DPUName: "card-b", DPUUID: "dpu-b", HostAdapterKey: "adapter-b", AdapterSerial: "CARD-B"}}},
			{NodeName: "dpu-node-a", NodeUID: "node-a", RebootMethod: "SystemLevelReset", SelectedDPUs: []ActivationDPU{{SerialNumber: "CARD-A", DPUName: "card-a", DPUUID: "dpu-a", HostAdapterKey: "adapter-a", AdapterSerial: "CARD-A"}}},
		},
	}
}

func rebootGuardPhases(request ActivationRequest, phase string) map[types.UID]string {
	phases := make(map[types.UID]string)
	for _, node := range request.DPUNodes {
		for _, dpu := range node.SelectedDPUs {
			phases[dpu.DPUUID] = phase
		}
	}
	return phases
}

func rebootGuardDPU(uid types.UID, previousPhase, transitionedAt string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU",
		"metadata": map[string]any{"name": "card-a", "namespace": "dpf", "uid": string(uid)},
		"status": map[string]any{
			"previousPhase": previousPhase,
			"conditions":    []any{map[string]any{"type": "Ready", "lastTransitionTime": transitionedAt}},
		},
	}}
}

func newRebootGuardJournal(t *testing.T) (*executionJournal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	journal, err := newExecutionJournal(path, rebootGuardBundle)
	if err != nil {
		t.Fatalf("newExecutionJournal(): %v", err)
	}
	t.Cleanup(func() { _ = journal.close() })
	return journal, path
}

func TestActivationRebootGuardIdentityIsDeterministicAndCohortBound(t *testing.T) {
	deployment := rebootGuardDeployment()
	request := rebootGuardRequest()
	phases := rebootGuardPhases(request, "Update Firmware")
	before := request
	before.DPUNodes = append([]ActivationNode(nil), request.DPUNodes...)
	first, digest, err := activationRebootGuardObject(deployment, rebootGuardBundle, request, phases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(): %v", err)
	}
	reversed := rebootGuardRequest()
	reversed.DPUNodes[0], reversed.DPUNodes[1] = reversed.DPUNodes[1], reversed.DPUNodes[0]
	second, secondDigest, err := activationRebootGuardObject(deployment, rebootGuardBundle, reversed, phases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(reversed): %v", err)
	}
	if first.GetName() != second.GetName() || digest != secondDigest {
		t.Fatalf("cohort ordering changed identity: %s/%s != %s/%s", first.GetName(), digest, second.GetName(), secondDigest)
	}
	changedAccess := rebootGuardRequest()
	changedAccess.HostInputRef = "/discovery/hosts/reordered"
	changedAccess.HostAdapterKey = "adapter-b"
	changedAccess.HostAdapterSerial = "CARD-B"
	accessGuard, accessDigest, err := activationRebootGuardObject(deployment, rebootGuardBundle, changedAccess, phases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(changed access anchor): %v", err)
	}
	if first.GetName() != accessGuard.GetName() || digest != accessDigest {
		t.Fatalf("nonphysical access reference/anchor changed the one-time guard key: %s/%s != %s/%s", first.GetName(), digest, accessGuard.GetName(), accessDigest)
	}
	changed := rebootGuardRequest()
	changed.DPUNodes[0].SelectedDPUs[0].DPUUID = "replacement-dpu-uid"
	third, _, err := activationRebootGuardObject(deployment, rebootGuardBundle, changed, rebootGuardPhases(changed, "Update Firmware"))
	if err != nil {
		t.Fatalf("activationRebootGuardObject(changed): %v", err)
	}
	if first.GetName() == third.GetName() {
		t.Fatal("a changed selected DPU UID reused the previous latch name")
	}
	firmwarePhases := rebootGuardPhases(request, "Update Firmware")
	configPhases := rebootGuardPhases(request, "DPU Config")
	firmwareGuard, _, err := activationRebootGuardObject(deployment, rebootGuardBundle, request, firmwarePhases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(firmware cycle): %v", err)
	}
	configGuard, _, err := activationRebootGuardObject(deployment, rebootGuardBundle, request, configPhases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(config cycle): %v", err)
	}
	if firmwareGuard.GetName() == configGuard.GetName() {
		t.Fatal("a later supported DPF phase reused the prior phase's one-time reboot claim")
	}
	oldDPU := rebootGuardDPU("dpu-a", "Update Firmware", "2026-11-14T09:00:00Z")
	newDPU := rebootGuardDPU("dpu-a", "Update Firmware", "2026-11-14T09:15:00Z")
	oldPhase, oldPhaseErr := activationRebootSourcePhase(oldDPU)
	newPhase, newPhaseErr := activationRebootSourcePhase(newDPU)
	if oldPhaseErr != nil || newPhaseErr != nil || oldPhase != newPhase {
		t.Fatalf("same controller phase changed with condition timestamp: old=%q/%v new=%q/%v", oldPhase, oldPhaseErr, newPhase, newPhaseErr)
	}
	samePhaseRetry := rebootGuardPhases(request, oldPhase)
	transitionRetry, _, err := activationRebootGuardObject(deployment, rebootGuardBundle, request, samePhaseRetry)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(same phase retry): %v", err)
	}
	if firmwareGuard.GetName() != transitionRetry.GetName() {
		t.Fatal("same-phase retry changed identity when only the condition transition timestamp changed")
	}
	if request.HostInputRef != before.HostInputRef || request.DPUNodes[0].NodeName != before.DPUNodes[0].NodeName {
		t.Fatal("building a reboot claim mutated the request")
	}
	if len(first.GetName()) > 63 || !strings.HasPrefix(first.GetName(), activationRebootGuardName) {
		t.Fatalf("invalid ConfigMap name %q", first.GetName())
	}
	immutable, found, err := unstructured.NestedBool(first.Object, "immutable")
	if err != nil || !found || !immutable {
		t.Fatalf("claim is not immutable: found=%v value=%v err=%v", found, immutable, err)
	}
	owners, found, err := unstructured.NestedSlice(first.Object, "metadata", "ownerReferences")
	if err != nil || !found || len(owners) != 1 {
		t.Fatalf("claim lacks one owner reference: %#v (%v)", owners, err)
	}
	owner := owners[0].(map[string]any)
	if owner["uid"] != "deployment-uid" || owner["name"] != "hbn" || owner["kind"] != "DPUDeployment" {
		t.Fatalf("claim owner does not bind to the selected deployment: %#v", owner)
	}
}

func TestClaimAndRebootExternalPersistsImmutableClaimBeforeCallback(t *testing.T) {
	journal, path := newRebootGuardJournal(t)
	client := &rebootGuardClient{}
	executor := &rebootGuardExecutor{journal: path}
	request := rebootGuardRequest()
	got, err := claimAndRebootExternal(context.Background(), client, rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"), journal, executor)
	if err != nil {
		t.Fatalf("claimAndRebootExternal(): %v", err)
	}
	if client.calls != 1 || executor.calls != 1 || got.PreviousBootID != "old" {
		t.Fatalf("create calls=%d reboot calls=%d observation=%#v", client.calls, executor.calls, got)
	}
	want, digest, buildErr := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"))
	if buildErr != nil || !activationRebootGuardMatches(client.created, want, digest) {
		t.Fatalf("created latch does not match its selected identity: build=%v created=%#v want=%#v digest=%s", buildErr, client.created.Object, want.Object, digest)
	}
}

func TestClaimAndRebootExternalNeverCallsBackForExistingOrUncertainClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "already exists", err: apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "dpf-rb-existing")},
		{name: "uncertain create", err: errors.New("transport timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal, path := newRebootGuardJournal(t)
			client := &rebootGuardClient{err: tc.err}
			executor := &rebootGuardExecutor{journal: path}
			request := rebootGuardRequest()
			if _, err := claimAndRebootExternal(context.Background(), client, rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"), journal, executor); err == nil {
				t.Fatal("claim conflict or uncertain create was accepted")
			}
			if client.calls != 1 || executor.calls != 0 {
				t.Fatalf("create calls=%d reboot calls=%d; uncertain claims must not invoke the host", client.calls, executor.calls)
			}
		})
	}
}

func TestClaimAndRebootExternalRejectsMalformedCreateResponse(t *testing.T) {
	journal, path := newRebootGuardJournal(t)
	client := &rebootGuardClient{response: func(desired *unstructured.Unstructured) *unstructured.Unstructured {
		created := desired.DeepCopy()
		created.SetUID(types.UID(rebootGuardUID))
		created.SetResourceVersion("2")
		_ = unstructured.SetNestedField(created.Object, false, "immutable")
		return created
	}}
	executor := &rebootGuardExecutor{journal: path}
	request := rebootGuardRequest()
	if _, err := claimAndRebootExternal(context.Background(), client, rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"), journal, executor); err == nil {
		t.Fatal("mutable claim response was accepted")
	}
	if executor.calls != 0 {
		t.Fatalf("host reboot callback ran %d times for an unverified claim", executor.calls)
	}
}

type failingRebootGuardJournalFile struct {
	bytes.Buffer
	syncCount int
	failSync  int
}

func (f *failingRebootGuardJournalFile) Sync() error {
	f.syncCount++
	if f.syncCount == f.failSync {
		return errors.New("simulated fsync failure")
	}
	return nil
}

func (f *failingRebootGuardJournalFile) Close() error { return nil }

func TestClaimAndRebootExternalDoesNotCallBackWhenClaimReceiptCannotBeSynced(t *testing.T) {
	file := &failingRebootGuardJournalFile{failSync: 2}
	journal := &executionJournal{file: file, digest: rebootGuardBundle}
	client := &rebootGuardClient{}
	executor := &rebootGuardExecutor{}
	request := rebootGuardRequest()
	if _, err := claimAndRebootExternal(context.Background(), client, rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware"), journal, executor); err == nil {
		t.Fatal("failed durable claim receipt was accepted")
	}
	if client.calls != 1 || executor.calls != 0 {
		t.Fatalf("create calls=%d reboot calls=%d; physical action must wait for synced claim receipt", client.calls, executor.calls)
	}
}

func TestActivationRebootGuardRejectsIncompleteOrUnsupportedCohort(t *testing.T) {
	for _, mutate := range []func(*ActivationRequest){
		func(r *ActivationRequest) { r.DPUNodes[0].RebootMethod = "PowerCycle" },
		func(r *ActivationRequest) { r.DPUNodes[0].NodeUID = "" },
		func(r *ActivationRequest) {
			r.DPUNodes[1].SelectedDPUs[0].DPUUID = r.DPUNodes[0].SelectedDPUs[0].DPUUID
		},
	} {
		request := rebootGuardRequest()
		mutate(&request)
		if _, _, err := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, request, rebootGuardPhases(request, "Update Firmware")); err == nil {
			t.Fatal("invalid host reboot cohort was accepted")
		}
	}
	for _, phase := range []string{"", "unknown", "Config FW Parameters"} {
		if _, _, err := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, rebootGuardRequest(), rebootGuardPhases(rebootGuardRequest(), phase)); err == nil {
			t.Fatalf("unsupported DPF source phase %q was accepted", phase)
		}
	}
	request := rebootGuardRequest()
	phases := rebootGuardPhases(request, "Update Firmware")
	delete(phases, request.DPUNodes[0].SelectedDPUs[0].DPUUID)
	if _, _, err := activationRebootGuardObject(rebootGuardDeployment(), rebootGuardBundle, request, phases); err == nil {
		t.Fatal("missing source phase for a selected DPU was accepted")
	}
}
