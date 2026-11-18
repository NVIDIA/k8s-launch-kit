// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func checkpointAtState(t *testing.T, template *deploymentOperation, state operationState) (*deploymentOperation, *operationMemoryStore) {
	t.Helper()
	object := template.object.DeepCopy()
	data, err := encodeOperation(state)
	if err != nil {
		t.Fatal(err)
	}
	object.Data["checkpoint.json"] = data
	store := &operationMemoryStore{object: object.DeepCopy()}
	return newDeploymentOperation(store, object, state), store
}
func completedOperationFixture(t *testing.T) (activationIntegrationFixture, *deploymentOperation) {
	t.Helper()
	f := newActivationIntegrationFixture(t)
	op, _ := checkpointFixture(t, f)
	f.prepared.operation = op
	client := recoveryActivationClient{f.client}
	testTenant(f.controlPlane.tenant.Client).source = client
	reboot := &recoveryRebooter{fixture: &f}
	if _, err := runActivationStage(f.ctx, client, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second, reboot); err != nil {
		t.Fatal(err)
	}
	return f, op
}
func resetProofs(reset operationReset) map[types.UID]activationRebootRound {
	result := map[types.UID]activationRebootRound{}
	for uid, round := range reset.ArmProofs {
		result[uid] = liveRound(round)
	}
	return result
}
func TestOperationNamedTransitionsValidateBeforeAssignment(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var key string
	var reset operationReset
	for k, r := range completed.state.Resets {
		key, reset = k, r
	}
	p := completed.restoreProgress()[reset.Request.DPUNodes[0].NodeName]
	proofs := resetProofs(reset)
	tests := []struct {
		name       string
		prepare    func(*operationState)
		transition func(*deploymentOperation) error
	}{
		{"hold confirmation without intent", func(s *operationState) {
			s.HoldIntents = map[string]operationProgress{}
			s.Progress = map[string]operationProgress{}
		}, func(o *deploymentOperation) error {
			p := copyActivationNodeProgress(p)
			p.releasedVersion = "released"
			p.resetAcknowledged = false
			return o.confirmHoldReleased(f.ctx, p)
		}},
		{"children validate all groups before adding", nil, func(o *deploymentOperation) error {
			deployment, _ := retainedDeploymentObject(f.prepared.execution.verificationInputs())
			observed, err := observeActivationScope(f.ctx, recoveryActivationClient{f.client}, f.prepared.execution.verificationInputs(), deployment, o.deploymentUID(), f.client.activationSelected, o.restoreProgress())
			if err != nil {
				t.Fatal(err)
			}
			extra := observed.sets[f.client.activationSelected[0].dpuSet.GetName()].DeepCopy()
			extra.SetName("additional-child")
			observed.sets[extra.GetName()] = extra
			for _, dpu := range observed.dpus {
				dpu.SetUID("replaced-child")
				break
			}
			return o.bindChildren(f.ctx, observed)
		}},

		{"unknown claim", nil, func(o *deploymentOperation) error { return o.bindResetClaim(f.ctx, "missing", reset.ClaimUID) }},
		{"claim rebinding", nil, func(o *deploymentOperation) error { return o.bindResetClaim(f.ctx, key, reset.ClaimUID) }},
		{"baseline wrong host", func(s *operationState) {
			r := s.Resets[key]
			r.Baseline = ""
			r.Acknowledged = false
			s.Resets[key] = r
		}, func(o *deploymentOperation) error {
			proof := reset.Observation
			proof.HostSystemUUID = "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff"
			return o.recordResetBaseline(f.ctx, key, reset.ClaimUID, proof)
		}},
		{"baseline before claim", func(s *operationState) { r := s.Resets[key]; r.ClaimUID = ""; r.Baseline = ""; s.Resets[key] = r }, func(o *deploymentOperation) error {
			return o.recordResetBaseline(f.ctx, key, reset.ClaimUID, reset.Observation)
		}},
		{"physical observation before baseline", func(s *operationState) { r := s.Resets[key]; r.Baseline = ""; s.Resets[key] = r }, func(o *deploymentOperation) error {
			_, err := o.recordResetObservation(f.ctx, key, reset.Observation)
			return err
		}},
		{"changed physical boot", nil, func(o *deploymentOperation) error {
			proof := reset.Observation
			proof.CurrentBootID = "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff"
			_, err := o.recordResetObservation(f.ctx, key, proof)
			return err
		}},
		{"unknown physical observation", nil, func(o *deploymentOperation) error {
			_, err := o.recordResetObservation(f.ctx, "missing", reset.Observation)
			return err
		}},
		{"ACK before physical proof", func(s *operationState) {
			r := s.Resets[key]
			r.Observation = ActivationObservation{}
			s.Resets[key] = r
		}, func(o *deploymentOperation) error { return o.recordResetACKIntent(f.ctx, key, proofs) }},
		{"missing Arm proof", nil, func(o *deploymentOperation) error {
			return o.recordResetACKIntent(f.ctx, key, map[types.UID]activationRebootRound{})
		}},
		{"foreign Arm proof", nil, func(o *deploymentOperation) error {
			v := maps.Clone(proofs)
			for uid, proof := range v {
				delete(v, uid)
				v["foreign"] = proof
				break
			}
			return o.recordResetACKIntent(f.ctx, key, v)
		}},
		{"same Arm boot", nil, func(o *deploymentOperation) error {
			v := maps.Clone(proofs)
			for uid, proof := range v {
				proof.kernelBoot = reset.Rounds[uid].KernelBoot
				v[uid] = proof
			}
			return o.recordResetACKIntent(f.ctx, key, v)
		}},
		{"ACK completion without intent", func(s *operationState) { r := s.Resets[key]; r.ACKIntent = false; s.Resets[key] = r }, func(o *deploymentOperation) error {
			return o.confirmResetAcknowledged(f.ctx, key, []activationNodeProgress{p})
		}},
		{"ACK completion foreign node", nil, func(o *deploymentOperation) error {
			v := copyActivationNodeProgress(p)
			v.nodeName = "foreign"
			return o.confirmResetAcknowledged(f.ctx, key, []activationNodeProgress{v})
		}},
		{"unknown ACK completion", nil, func(o *deploymentOperation) error {
			return o.confirmResetAcknowledged(f.ctx, "missing", []activationNodeProgress{p})
		}},
		{"host result without intent", nil, func(o *deploymentOperation) error {
			return o.confirmHostNetwork(f.ctx, "SERIAL-A", sshprobe.HostNetworkRequest{}, HostWorkloadNetworkResult{})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed := newDeploymentOperation(nil, completed.object, completed.state).state
			if tc.prepare != nil {
				tc.prepare(&seed)
			}
			op, store := checkpointAtState(t, completed, seed)
			before, _ := encodeOperation(op.state)
			if err := tc.transition(op); err == nil {
				t.Fatal("illegal transition accepted")
			}
			after, _ := encodeOperation(op.state)
			if before != after || store.updates != 0 {
				t.Fatal("invalid transition partially assigned or persisted")
			}
		})
	}
}
func TestOperationNamedTransitionsRepeatCASAndCopyInputs(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var key string
	var reset operationReset
	for k, r := range completed.state.Resets {
		key, reset = k, r
	}
	op, store := checkpointAtState(t, completed, completed.state)
	observation := copyOperationObservation(reset.Observation)
	ready := true
	observation.KubernetesNodeReady = &ready
	if _, err := op.recordResetObservation(f.ctx, key, observation); err != nil {
		t.Fatal(err)
	}
	ready = false
	retained, _ := op.reset(key)
	if !*retained.Observation.KubernetesNodeReady {
		t.Fatal("observation input aliases owner")
	}
	before := store.updates
	if _, err := op.recordResetObservation(f.ctx, key, retained.Observation); err != nil {
		t.Fatal(err)
	}
	proofs := resetProofs(reset)
	if err := op.recordResetACKIntent(f.ctx, key, proofs); err != nil {
		t.Fatal(err)
	}
	if err := op.recordResetACKIntent(f.ctx, key, proofs); err != nil {
		t.Fatal(err)
	}
	if store.updates != before+3 || store.noops < 2 {
		t.Fatal("mandatory repeated fences skipped CAS")
	}
	for uid, v := range proofs {
		v.kernelBoot = "mutated"
		proofs[uid] = v
	}
	retained, _ = op.reset(key)
	for _, v := range retained.ArmProofs {
		if v.KernelBoot == "mutated" {
			t.Fatal("proof input aliases owner")
		}
	}
	// Exercise the legal first-observed-false path without manufacturing a hold intent.
	seed := newDeploymentOperation(nil, completed.object, completed.state).state
	seed.Progress = map[string]operationProgress{}
	seed.HoldIntents = map[string]operationProgress{}
	r := seed.Resets[key]
	r.Acknowledged = false
	seed.Resets[key] = r
	first, _ := checkpointAtState(t, completed, seed)
	p := completed.restoreProgress()[reset.Request.DPUNodes[0].NodeName]
	if err := first.confirmResetAcknowledged(f.ctx, key, []activationNodeProgress{p}); err != nil {
		t.Fatalf("qualified reset must allow first-observed false: %v", err)
	}
	for name := range p.dpus {
		p.dpus[name] = "mutated"
	}
	if reflect.DeepEqual(first.restoreProgress()[p.nodeName].dpus, p.dpus) {
		t.Fatal("completion input aliases owner")
	}
}
func TestOperationSetupAndViewsDetachNestedData(t *testing.T) {
	_, completed := completedOperationFixture(t)
	state := newDeploymentOperation(nil, completed.object, completed.state).state
	state.Hosts["SERIAL-A"] = operationHost{Result: &HostWorkloadNetworkResult{MTU: 9000}}
	for key, r := range state.Resets {
		ready := true
		r.Observation.KubernetesNodeReady = &ready
		r.Request.BeforeSubmit = func(ActivationObservation) error { return nil }
		r.Request.recordClaim = func(types.UID) error { return nil }
		state.Resets[key] = r
	}
	op, _ := checkpointAtState(t, completed, state)
	before, _ := encodeOperation(op.state)
	state.Management.NamespaceUIDs["mutated"] = "mutated"
	state.Enrollment.Devices[0].Serial = "mutated"
	state.Software.Components[0].Component = "mutated"
	state.Workload[0].Name = "mutated"
	state.TenantSources["mutated"] = "mutated"
	for _, p := range state.Progress {
		for k := range p.Sets {
			p.Sets[k] = "mutated"
		}
		for k := range p.DPUs {
			p.DPUs[k] = "mutated"
		}
		for k := range p.Versions {
			p.Versions[k] = "mutated"
		}
	}
	for _, r := range state.Resets {
		r.Request.DPUNodes[0].SelectedDPUs[0].DPUName = "mutated"
		*r.Observation.KubernetesNodeReady = false
		for uid := range r.Rounds {
			delete(r.Rounds, uid)
		}
	}
	for _, p := range state.HoldIntents {
		for key := range p.Sets {
			p.Sets[key] = "mutated"
		}
		for key := range p.DPUs {
			p.DPUs[key] = "mutated"
		}
		for key := range p.Versions {
			p.Versions[key] = "mutated"
		}
	}
	state.Hosts["SERIAL-A"].Result.MTU = 1
	receipts := op.preparationReceipts()
	receipts.Management.NamespaceUIDs["view"] = "view"
	receipts.Enrollment.Devices[0].Serial = "view"
	receipts.Software.Components[0].Component = "view"
	receipts.Workload[0].Name = "view"
	receipts.TenantSources["view"] = "view"
	_, sources := op.tenantBinding()
	sources["view"] = "view"
	devices := op.enrolledDevices()
	devices[0].Serial = "view"
	requests := op.hostRequests()
	requests["SERIAL-A"] = sshprobe.HostNetworkRequest{}
	children := op.childBindings()
	for k := range children {
		delete(children, k)
	}
	progress := op.restoreProgress()
	for _, p := range progress {
		for k := range p.sets {
			delete(p.sets, k)
		}
		for k := range p.dpus {
			delete(p.dpus, k)
		}
		for k := range p.versions {
			delete(p.versions, k)
		}
	}
	for key := range op.state.Resets {
		r, _ := op.reset(key)
		if r.Request.BeforeSubmit != nil || r.Request.recordClaim != nil || !r.Request.ResetSafetyExpiresAt.IsZero() {
			t.Fatal("transient authority escaped")
		}
		r.Request.DPUNodes[0].SelectedDPUs[0].DPUName = "view"
		*r.Observation.KubernetesNodeReady = false
		for uid := range r.Rounds {
			delete(r.Rounds, uid)
		}
		for uid := range r.ArmProofs {
			delete(r.ArmProofs, uid)
		}
	}
	for _, rounds := range op.resetRoundHistory() {
		for uid := range rounds {
			delete(rounds, uid)
		}
	}
	host, _ := op.hostTransaction("SERIAL-A")
	host.Result.MTU = 1
	after, _ := encodeOperation(op.state)
	if before != after {
		t.Fatal("constructor or detached view retained an alias")
	}
	if copyOperationEnrollment(enrollmentStageReceipt{}).Devices != nil || copyOperationEnrollment(enrollmentStageReceipt{Devices: []enrolledDeviceReceipt{}}).Devices == nil {
		t.Fatal("nil/empty lost")
	}
}
func TestOperationStickyFailureAcrossNamedRoutes(t *testing.T) {
	f, completed := completedOperationFixture(t)
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "committed response lost"}[commit], func(t *testing.T) {
			op, store := checkpointAtState(t, completed, completed.state)
			store.fail, store.committedError = !commit, commit
			for key, reset := range op.state.Resets {
				if _, err := op.recordResetObservation(f.ctx, key, reset.Observation); err == nil {
					t.Fatal("failure accepted")
				}
				break
			}
			calls := store.updates
			if err := op.bindDeployment(f.ctx, op.deploymentUID()); err == nil {
				t.Fatal("equal parent bypassed failure latch")
			}
			if err := op.bindChildren(f.ctx, activationObservation{}); err == nil {
				t.Fatal("child no-op bypassed failure latch")
			}
			if _, err := op.recordHostIntent(f.ctx, "SERIAL-A", sshprobe.HostNetworkRequest{}); err == nil {
				t.Fatal("host route bypassed failure latch")
			}
			if err := op.confirmHostNetwork(f.ctx, "SERIAL-A", sshprobe.HostNetworkRequest{}, HostWorkloadNetworkResult{}); err == nil {
				t.Fatal("confirmation bypassed failure latch")
			}
			for key, reset := range op.state.Resets {
				if err := op.recordResetACKIntent(f.ctx, key, resetProofs(reset)); err == nil {
					t.Fatal("ACK route bypassed failure latch")
				}
				break
			}
			if store.updates != calls {
				t.Fatal("poisoned attempt retried checkpoint")
			}
		})
	}
}

func TestOperationLegalIntentClaimBaselineHoldAndResetHistory(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var key string
	var reset operationReset
	for k, r := range completed.state.Resets {
		key, reset = k, r
	}
	state := newDeploymentOperation(nil, completed.object, completed.state).state
	state.Resets = map[string]operationReset{}
	state.HoldIntents = map[string]operationProgress{}
	state.Progress = map[string]operationProgress{}
	op, store := checkpointAtState(t, completed, state)
	proof := completed.restoreProgress()[reset.Request.DPUNodes[0].NodeName]
	proof.releasedVersion, proof.resetAcknowledged = "pending", false
	if err := op.recordHoldIntent(f.ctx, proof); err != nil {
		t.Fatal(err)
	}
	intent := copyActivationNodeProgress(proof)
	for k := range proof.sets {
		proof.sets[k] = "mutated"
	}
	for k := range proof.dpus {
		proof.dpus[k] = "mutated"
	}
	for k := range proof.versions {
		proof.versions[k] = "mutated"
	}
	intent.releasedVersion = "refreshed-pending"
	if err := op.recordHoldIntent(f.ctx, intent); err != nil {
		t.Fatalf("fresh same-hold RV must remain legal: %v", err)
	}
	intent.releasedVersion = "released"
	if err := op.confirmHoldReleased(f.ctx, intent); err != nil {
		t.Fatal(err)
	}
	for k := range intent.dpus {
		intent.dpus[k] = "mutated"
	}
	request := copyOperationRequest(reset.Request)
	request.BeforeSubmit = func(ActivationObservation) error { return errors.New("must not escape") }
	request.recordClaim = func(types.UID) error { return errors.New("must not escape") }
	request.ResetSafetyExpiresAt = time.Now()
	phases := map[types.UID]string{}
	rounds := map[types.UID]activationRebootRound{}
	for uid, round := range reset.Rounds {
		phases[uid] = round.Source
		rounds[uid] = liveRound(round)
	}
	saved, known, err := op.recordResetIntent(f.ctx, request, phases, rounds)
	if err != nil || known {
		t.Fatalf("new intent: known=%v err=%v", known, err)
	}
	request.DPUNodes[0].SelectedDPUs[0].DPUName = "mutated"
	for uid := range rounds {
		delete(rounds, uid)
	}
	retained, _ := op.reset(key)
	if retained.Request.DPUNodes[0].SelectedDPUs[0].DPUName == "mutated" || len(retained.Rounds) == 0 || saved.Request.BeforeSubmit != nil || saved.Request.recordClaim != nil || !saved.Request.ResetSafetyExpiresAt.IsZero() {
		t.Fatal("intent retained mutable input or transient authority")
	}
	if err := op.bindResetClaim(f.ctx, key, reset.ClaimUID); err != nil {
		t.Fatal(err)
	}
	if err := op.recordResetBaseline(f.ctx, key, reset.ClaimUID, reset.Observation); err != nil {
		t.Fatal(err)
	}
	if _, err := op.recordResetObservation(f.ctx, key, reset.Observation); err != nil {
		t.Fatal(err)
	}
	if err := op.recordResetACKIntent(f.ctx, key, resetProofs(reset)); err != nil {
		t.Fatal(err)
	}
	completion := completed.restoreProgress()[reset.Request.DPUNodes[0].NodeName]
	if err := op.confirmResetAcknowledged(f.ctx, key, []activationNodeProgress{completion}); err != nil {
		t.Fatalf("reset must replace release-style progress: %v", err)
	}
	restored := op.restoreProgress()[completion.nodeName]
	if restored.releasedVersion != "" || !restored.resetAcknowledged || store.updates < 8 {
		t.Fatal("legal history not retained")
	}
}

func TestOperationConfigMapIngressAndReturnedObjectAreDetached(t *testing.T) {
	f := newActivationIntegrationFixture(t)
	template, _ := checkpointFixture(t, f)
	object := template.object.DeepCopy()
	memory := &operationMemoryStore{object: object.DeepCopy()}
	var returned *corev1.ConfigMap
	store := operationResponseStore{operationStore: memory, update: func(ctx context.Context, desired *corev1.ConfigMap) (*corev1.ConfigMap, error) {
		result, err := memory.updateOperation(ctx, desired)
		returned = result
		return result, err
	}}
	op := newDeploymentOperation(store, object, template.state)
	object.Data["checkpoint.json"] = "changed by constructor caller"
	object.Labels[operationLabel] = "changed"
	if err := op.bindDeployment(f.ctx, activationIntegrationDeploymentUID); err != nil {
		t.Fatal(err)
	}
	retained := op.object.DeepCopy()
	returned.Data["checkpoint.json"] = "changed by store"
	returned.Labels[operationLabel] = "changed"
	returned.UID = "changed"
	if !reflect.DeepEqual(retained, op.object) {
		t.Fatal("accepted ConfigMap aliases returned store object")
	}
}

func TestOperationHostConfirmationRequiresExactDefinitiveResult(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var reset operationReset
	for _, r := range completed.state.Resets {
		reset = r
		break
	}
	serial := reset.Request.HostAdapterSerial
	request := sshprobe.HostNetworkRequest{Identity: sshprobe.HostNetworkIdentity{SystemUUID: reset.Request.HostSystemUUID, AdapterSerial: serial, InterfaceName: "ens5f0np0", PCIAddress: "0000:21:00.0", PermanentMAC: "20:4d:52:3e:9a:b8", PhysicalPort: "p0"}, LeaseBinding: f.prepared.execution.plan.Digest(), MTU: 9000, HostAddressPrefix: "10.244.0.2/24", Gateway: "10.244.0.1", WorkloadCIDR: "10.244.0.0/16"}
	result := HostWorkloadNetworkResult{DPUSerial: serial, HostInputRef: reset.Request.HostInputRef, Interface: request.Identity.InterfaceName, HostAddressPrefix: request.HostAddressPrefix, Gateway: request.Gateway, MTU: request.MTU, LeaseReverified: true, Configuration: sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true, ChangedMTU: true}}
	for _, tc := range []struct {
		name string
		edit func(*sshprobe.HostNetworkRequest, *HostWorkloadNetworkResult)
	}{
		{"different request", func(r *sshprobe.HostNetworkRequest, _ *HostWorkloadNetworkResult) { r.MTU++ }},
		{"wrong serial", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.DPUSerial = "foreign" }},
		{"missing input reference", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.HostInputRef = "" }},
		{"wrong interface", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.Interface = "eth9" }},
		{"wrong address", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) {
			r.HostAddressPrefix = "10.244.1.2/24"
		}},
		{"wrong gateway", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.Gateway = "10.244.0.3" }},
		{"wrong MTU", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.MTU++ }},
		{"missing post-fence", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.LeaseReverified = false }},
		{"blocking", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.Configuration.Blocking = true }},
		{"incomplete state", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) { r.Configuration.State = "unknown" }},
		{"runtime unverified", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) {
			r.Configuration.RuntimeVerified = false
		}},
		{"file unverified", func(_ *sshprobe.HostNetworkRequest, r *HostWorkloadNetworkResult) {
			r.Configuration.FileVerified = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, store := checkpointAtState(t, completed, completed.state)
			if known, err := op.recordHostIntent(f.ctx, serial, request); known || err != nil {
				t.Fatalf("intent: known=%v err=%v", known, err)
			}
			updates := store.updates
			before, _ := encodeOperation(op.state)
			changedRequest, changedResult := request, result
			tc.edit(&changedRequest, &changedResult)
			if err := op.confirmHostNetwork(f.ctx, serial, changedRequest, changedResult); err == nil {
				t.Fatal("incomplete/mismatched result accepted")
			}
			after, _ := encodeOperation(op.state)
			if before != after || updates != store.updates {
				t.Fatal("invalid host result partially persisted")
			}
		})
	}
	op, store := checkpointAtState(t, completed, completed.state)
	if _, err := op.recordHostIntent(f.ctx, serial, request); err != nil {
		t.Fatal(err)
	}
	updates := store.updates
	if known, err := op.recordHostIntent(f.ctx, serial, request); !known || err != nil || store.updates != updates {
		t.Fatal("known host intent did not select inspection without rewrite")
	}
	if err := op.confirmHostNetwork(f.ctx, serial, request, result); err != nil {
		t.Fatal(err)
	}
	saved, _ := op.hostTransaction(serial)
	result.Configuration.ChangedMTU = false
	result.Configuration.VerifiedRepeat = true
	if err := op.confirmHostNetwork(f.ctx, serial, request, result); err != nil {
		t.Fatal(err)
	}
	repeated, _ := op.hostTransaction(serial)
	if !reflect.DeepEqual(saved, repeated) {
		t.Fatal("repeat inspection changed original provenance")
	}
}

func TestOperationCopyPreservesNilAndEmptyRequestCollections(t *testing.T) {
	for _, nodes := range [][]ActivationNode{nil, {}} {
		copied := copyOperationRequest(ActivationRequest{DPUNodes: nodes})
		if (nodes == nil) != (copied.DPUNodes == nil) {
			t.Fatal("request nil/empty changed")
		}
	}
	for _, dpus := range [][]ActivationDPU{nil, {}} {
		copied := copyOperationRequest(ActivationRequest{DPUNodes: []ActivationNode{{SelectedDPUs: dpus}}})
		if (dpus == nil) != (copied.DPUNodes[0].SelectedDPUs == nil) {
			t.Fatal("selected DPU nil/empty changed")
		}
	}
	if copyOperationObservation(ActivationObservation{}).KubernetesNodeReady != nil {
		t.Fatal("optional boolean invented")
	}
}

func TestOperationResetPlanningPreservesInterruptedAndDistinctPhaseRequests(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var key string
	var reset operationReset
	for k, r := range completed.state.Resets {
		key, reset = k, r
		break
	}
	phases := map[types.UID]string{}
	rounds := map[types.UID]activationRebootRound{}
	for uid, round := range reset.Rounds {
		phases[uid] = round.Source
		rounds[uid] = liveRound(round)
	}
	op, store := checkpointAtState(t, completed, completed.state)
	before, _ := encodeOperation(op.state)
	if _, _, err := op.recordResetIntent(f.ctx, reset.Request, phases, rounds); err == nil {
		t.Fatal("completed same-key reset planning accepted")
	}
	after, _ := encodeOperation(op.state)
	if before != after || store.updates != 0 {
		t.Fatal("completed request refusal changed history")
	}
	// A genuinely different supported phase keeps its own immutable claim key;
	// completion of the preceding phase does not close this next logical request.
	nextRounds := map[types.UID]activationRebootRound{}
	nextPhases := map[types.UID]string{}
	for uid, old := range reset.ArmProofs {
		round := liveRound(old)
		round.source = "DPU Config"
		if phases[uid] == round.source {
			round.source = "Initialize Interface"
		}
		round.requested = round.requested.Add(2 * time.Hour)
		nextRounds[uid] = round
		nextPhases[uid] = round.source
	}
	if _, known, err := op.recordResetIntent(f.ctx, reset.Request, nextPhases, nextRounds); known || err != nil {
		t.Fatalf("distinct next phase: known=%v err=%v", known, err)
	}
	if len(op.state.Resets) != 2 || !op.state.Resets[key].Acknowledged || store.updates != 1 {
		t.Fatal("distinct phase replaced acknowledged history")
	}
	// An interrupted same-key request may have newer agent observations; keep
	// its saved original baseline and select observe-only without a new write.
	seed := newDeploymentOperation(nil, completed.object, completed.state).state
	interrupted := seed.Resets[key]
	interrupted.Acknowledged = false
	seed.Resets[key] = interrupted
	pending, pendingStore := checkpointAtState(t, completed, seed)
	currentRounds := resetProofs(reset)
	saved, known, err := pending.recordResetIntent(f.ctx, reset.Request, phases, currentRounds)
	if err != nil || !known || pendingStore.updates != 0 || !reflect.DeepEqual(saved.Rounds, reset.Rounds) {
		t.Fatalf("interrupted same-key request lost original baseline: known=%v err=%v", known, err)
	}
}

func TestOperationObservationMatchIsSeparateFromPersistence(t *testing.T) {
	f, completed := completedOperationFixture(t)
	var key string
	var reset operationReset
	for k, r := range completed.state.Resets {
		key, reset = k, r
		break
	}
	op, store := checkpointAtState(t, completed, completed.state)
	invalid := reset.Observation
	invalid.PreviousBootID = "bbbbbbbb-bbbb-4ccc-8ddd-ffffffffffff"
	if matched, err := op.recordResetObservation(f.ctx, key, invalid); matched || err == nil || store.updates != 0 {
		t.Fatal("invalid saved history reported a match")
	}
	store.fail = true
	if matched, err := op.recordResetObservation(f.ctx, key, reset.Observation); !matched || err == nil || !op.failed {
		t.Fatal("valid physical history was lost with failed persistence")
	}
	updates := store.updates
	if matched, err := op.recordResetObservation(f.ctx, key, reset.Observation); matched || err == nil || store.updates != updates {
		t.Fatal("sticky failure was represented as newly matched history")
	}
	var stageOnly *deploymentOperation
	if matched, err := stageOnly.recordResetObservation(f.ctx, key, reset.Observation); !matched || err != nil {
		t.Fatal("nil owner changed stage-only behavior")
	}
}
