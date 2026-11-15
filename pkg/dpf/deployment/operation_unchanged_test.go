// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	corev1 "k8s.io/api/core/v1"
	"testing"
	"time"
)

type operationProofInterruptionStore struct {
	*operationMemoryStore
	failProof bool
}

func (s *operationProofInterruptionStore) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	state, err := decodeOperation(object.Data["checkpoint.json"])
	if err != nil {
		return nil, err
	}
	for _, reset := range state.Resets {
		if s.failProof && len(reset.ArmProofs) > 0 {
			s.failProof = false
			return nil, errors.New("interrupted before Arm proof checkpoint")
		}
	}
	return s.operationMemoryStore.updateOperation(ctx, object)
}
func TestOperationResumeAfterSavedHostObservationWithUnchangedCheckpoint(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	operation, memory := checkpointFixture(t, fixture)
	store := &operationProofInterruptionStore{operationMemoryStore: memory, failProof: true}
	operation.store = store
	fixture.prepared.operation = operation
	client := recoveryActivationClient{fixture.client}
	fixture.controlPlane.tenant.Client.source = client
	reboot := &recoveryRebooter{fixture: &fixture}
	run := func() error {
		_, err := runActivationStage(fixture.ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, reboot)
		return err
	}
	if err := run(); err == nil || store.failProof {
		t.Fatalf("first attempt did not reach injected Arm-proof save failure: %v", err)
	}
	if reboot.submissions != 1 {
		t.Fatalf("expected exactly one physical reset, got %d", reboot.submissions)
	}
	fixture.prepared.operation = reopenOperation(t, memory)
	fixture.prepared.operation.store = store
	err := run()
	if err != nil {
		t.Fatalf("resume must accept same checkpoint proven current by CAS; got %v; no-op updates=%d, reset submissions=%d", err, memory.noops, reboot.submissions)
	}
	if reboot.submissions != 1 || reboot.observations != 1 || memory.noops == 0 {
		t.Fatalf("resume must observe once without another reset and accept an unchanged CAS: resets=%d observations=%d noops=%d", reboot.submissions, reboot.observations, memory.noops)
	}
}

type operationResponseStore struct {
	operationStore
	update func(context.Context, *corev1.ConfigMap) (*corev1.ConfigMap, error)
}

func (s operationResponseStore) updateOperation(ctx context.Context, object *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	return s.update(ctx, object)
}

func TestOperationUnchangedCheckpointStillRequiresCurrentCAS(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	seed, _ := checkpointFixture(t, fixture)
	cases := []struct {
		name    string
		mutate  func(*deploymentOperation, *operationMemoryStore)
		reply   func(*corev1.ConfigMap)
		wantErr bool
	}{
		{name: "accepted unchanged checkpoint"},
		{name: "concurrent update", mutate: func(_ *deploymentOperation, store *operationMemoryStore) { store.object.ResourceVersion = "2" }, wantErr: true},
		{name: "lost unchanged response", mutate: func(_ *deploymentOperation, store *operationMemoryStore) { store.committedError = true }, wantErr: true},
		{name: "changed data with unchanged version", mutate: func(operation *deploymentOperation, _ *operationMemoryStore) {
			operation.state.DeploymentUID = activationIntegrationDeploymentUID
		}, reply: func(*corev1.ConfigMap) {}, wantErr: true},
		{name: "replaced response", reply: func(reply *corev1.ConfigMap) { reply.UID = "123e4567-e89b-12d3-a456-426614174099" }, wantErr: true},
		{name: "missing response version", reply: func(reply *corev1.ConfigMap) { reply.ResourceVersion = "" }, wantErr: true},
		{name: "different response payload", reply: func(reply *corev1.ConfigMap) { reply.Data["checkpoint.json"] += " " }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			object := seed.object.DeepCopy()
			state, err := decodeOperation(object.Data["checkpoint.json"])
			if err != nil {
				t.Fatal(err)
			}
			store := &operationMemoryStore{object: object.DeepCopy()}
			operation := &deploymentOperation{store: store, object: object, state: state}
			if tc.mutate != nil {
				tc.mutate(operation, store)
			}
			calls := 0
			operation.store = operationResponseStore{operationStore: store, update: func(ctx context.Context, desired *corev1.ConfigMap) (*corev1.ConfigMap, error) {
				calls++
				if tc.reply != nil {
					reply := desired.DeepCopy()
					tc.reply(reply)
					return reply, nil
				}
				return store.updateOperation(ctx, desired)
			}}
			err = operation.save(fixture.ctx)
			if (err != nil) != tc.wantErr || operation.failed != tc.wantErr || calls != 1 {
				t.Fatalf("checkpoint result: err=%v failed=%v calls=%d", err, operation.failed, calls)
			}
			if tc.wantErr {
				if err := operation.save(fixture.ctx); err == nil || calls != 1 {
					t.Fatal("an uncertain writer retried its CAS")
				}
			} else if operation.object.ResourceVersion != seed.object.ResourceVersion || store.noops != 1 {
				t.Fatal("the successful no-op did not preserve the current checkpoint version")
			}
		})
	}
}
