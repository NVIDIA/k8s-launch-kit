// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestManagementReconcilerUsesSelectedFiniteMetadata(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := retainedBlueFieldSoftware(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := controllerMetadataForPlan(execution.rules, execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := execution.rules.ActivationSetIntents(activationDeploymentForPlan(t, execution.plan), execution.plan.Intent().Devices)
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{}
	for _, set := range sets {
		tokens = append(tokens, "provisioning.dpu.nvidia.com/bfs-dpuset-"+set.GetName())
	}
	if len(tokens) != len(execution.plan.Intent().Devices) || len(tokens) == 0 {
		t.Fatal("plan did not bind every selected set")
	}
	current, err := execution.reconciler.markedDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID("software-uid")
	current.SetResourceVersion("1")
	finalizers := []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "dpu.nvidia.com/dpudeployment"}
	finalizers = append(finalizers, tokens...)
	current.SetFinalizers(finalizers)
	if err := execution.reconciler.Verify(desired, current); err != nil {
		t.Fatalf("native factory lost plan policy: %v", err)
	}
	strict, err := newReconciler(execution.rules, execution.plan.Digest(), controllercontract.MetadataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.Verify(desired, current); err == nil {
		t.Fatal("empty metadata projection allowed unbound dynamic finalizers")
	}
	isolated, err := newReconciler(execution.rules, execution.plan.Digest(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := isolated.Verify(desired, current); err != nil {
		t.Fatal(err)
	}
	foreign := current.DeepCopy()
	foreign.SetFinalizers([]string{"foreign.example/added-after-construction"})
	if err := isolated.Verify(desired, foreign); err == nil {
		t.Fatal("foreign finalizer broadened policy")
	}
	original := current.DeepCopy()
	client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{reconcileKey(current): current.DeepCopy()}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, disposition, err := isolated.Ensure(ctx, client, desired); err != nil || disposition != DispositionExisting || client.creates != 0 {
		t.Fatal("exact finalized software was not verified without effects")
	}
	if !reflect.DeepEqual(current.Object, original.Object) {
		t.Fatal("verification mutated observed software")
	}
	for _, edit := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[BundleDigestAnnotation] = "foreign"
			o.SetAnnotations(a)
		},
		func(o *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(o.Object, "foreign", "spec", "version"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		changed := original.DeepCopy()
		edit(changed)
		if err := isolated.Verify(desired, changed); err == nil {
			t.Fatal("plan metadata policy weakened markers or spec verification")
		}
	}
}

func TestActivationBoundaryAcceptsVendorFinalizersForCreatedSelectedSets(t *testing.T) {
	fixture := newActivationIntegrationFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	execution, client := fixture.prepared.execution, fixture.client
	deployment, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	marked, err := execution.reconciler.markedDesired(deployment)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.Create(fixture.ctx, marked, false)
	if err != nil || created == nil {
		t.Fatalf("create exact parent fixture: %v", err)
	}
	software, err := retainedBlueFieldSoftware(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	key := tenantStageKey(software)
	observed := client.base.resources[key]
	if observed == nil {
		t.Fatal("fixture software receipt is missing")
	}
	observed = observed.DeepCopy()
	// Model the vendor controller independently from the compiled allowlist:
	// each actually created same-namespace Set contributes its literal name.
	finalizers := []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "dpu.nvidia.com/dpudeployment"}
	if len(client.sets) != 2 {
		t.Fatal("fixture did not create both selected Sets")
	}
	for _, set := range client.sets {
		if set.GetName() == "" || set.GetNamespace() != observed.GetNamespace() || set.GetUID() == "" {
			t.Fatal("created Set identity is incomplete")
		}
		finalizers = append(finalizers, "provisioning.dpu.nvidia.com/bfs-dpuset-"+set.GetName())
	}
	observed.SetFinalizers(finalizers)
	client.base.resources[key] = observed
	objects, err := retainedWorkloadObjects(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	streams, creates := client.streamRequests.Load(), client.base.createCount
	ctx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
	defer cancel()
	if err := verifyActivationSoftware(ctx, client, execution.verificationInputs(), fixture.workload.Software); err != nil {
		t.Fatalf("vendor finalizers invalidated exact software receipt: %v", err)
	}
	if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(fixture.prepared, fixture.controlPlane, fixture.workload), fixture.controlPlane.tenant.Client, client.activationSelected, created.GetUID(), objects, fixture.journal, time.Second); err != nil {
		t.Fatalf("post-create vendor finalizers blocked the real activation boundary: %v", err)
	}
	if len(client.patches) != 0 || client.guardCalls != 0 || client.resetSafetyCalls != 0 || client.base.createCount != creates || client.streamRequests.Load() != streams {
		t.Fatal("readonly boundary verification released hold, claimed reboot, created an object or restreamed software")
	}
}

func activationDeploymentForPlan(t *testing.T, plan *generation.Plan) *unstructured.Unstructured {
	t.Helper()
	for _, stage := range plan.Stages() {
		for _, doc := range stage.Documents() {
			obj := doc.ObjectCopy()
			if obj.GetKind() == "DPUDeployment" {
				return obj
			}
		}
	}
	t.Fatal("missing retained deployment")
	return nil
}
