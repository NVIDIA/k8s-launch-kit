// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestManagementDesiredPreparationNeedsNoAccessOrExecution(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	before := retained.Files()
	// Any accidental ambient connection attempt must fail. Pure preparation
	// needs no current credentials, deployment journal or writable operation root.
	t.Setenv("KUBECONFIG", "/unavailable/validation-must-not-open-kubeconfig")
	desired, err := prepareManagementDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	if desired.plan.Digest() != retained.Index().BundleDigest || desired.verifier.BundleDigest() != desired.plan.Digest() {
		t.Fatal("desired plan/verifier lost retained identity")
	}
	if desired.rules.Validate() != nil || desired.configObject == nil || desired.manager == nil || len(desired.crds) == 0 || len(desired.namespaces) == 0 {
		t.Fatal("read preparation omitted expected retained inputs")
	}
	current, err := desired.verifier.markedDesired(desired.configObject)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID("existing-config")
	current.SetResourceVersion("9")
	if err := desired.verifier.Verify(desired.configObject, current); err != nil {
		t.Fatal(err)
	}
	// The observation-only verifier cannot become a create-or-verify adapter.
	if _, creates := any(desired.verifier).(interface {
		Ensure(context.Context, ResourceClient, *unstructured.Unstructured) (*unstructured.Unstructured, Disposition, error)
	}); creates {
		t.Fatal("desired verifier acquired create capability")
	}

	// Mutation of an object copied for a client cannot alter retained intent.
	copied := desired.configObject.DeepCopy()
	copied.SetName("changed-by-client")
	if desired.configObject.GetName() == copied.GetName() || !reflect.DeepEqual(before, retained.Files()) {
		t.Fatal("read preparation or a consumer mutated retained input")
	}
}

func TestManagementDesiredAndDeploymentPrepareSameReadProjection(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareManagementDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := loadManagementExecution(retained, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if desired.plan.Digest() != execution.plan.Digest() || desired.operatorVersion != execution.operatorVersion || desired.operatorNamespace != execution.operatorNamespace || desired.operator.releaseName != execution.operatorReleaseName {
		t.Fatal("deployment and read-only preparation disagree on retained identity")
	}
	for name, objects := range map[string][2]*unstructured.Unstructured{
		"manager": {desired.manager, execution.manager}, "config": {desired.configObject, execution.configObject},
	} {
		if !reflect.DeepEqual(objects[0].Object, objects[1].Object) {
			t.Fatalf("different %s projection", name)
		}
	}
	if !reflect.DeepEqual(desired.crds, execution.crds) || !reflect.DeepEqual(desired.namespaces, execution.namespaces) || !bytes.Equal(desired.operator.expectedManifest, execution.operator.expectedManifest) {
		t.Fatal("deployment and read preparation disagree on chart projection")
	}
	if execution.operator.postRenderer == nil || execution.operator.timeout != time.Minute || execution.preflight.bundleDigest != desired.plan.Digest() {
		t.Fatal("deployment composition lost installation or preflight inputs")
	}
	current, err := desired.verifier.markedDesired(desired.configObject)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID("config-uid")
	current.SetResourceVersion("1")
	if err := execution.reconciler.Verify(execution.configObject, current); err != nil {
		t.Fatal(err)
	}
}

func TestDesiredPreparationRejectsInvalidAndPartialInputsWithoutResult(t *testing.T) {
	for name, retained := range map[string]*bundle.Bundle{"missing": nil, "incomplete": {}} {
		t.Run(name, func(t *testing.T) {
			if desired, err := prepareManagementDesired(retained); err == nil || desired != nil {
				t.Fatalf("preparation = %v, %v; want no partial result", desired, err)
			}
		})
	}
	request := operatorTestRequest(t)
	if desired, err := prepareOperatorDesired(request.Archive[:len(request.Archive)/2], request.Inputs); err == nil || desired != nil {
		t.Fatalf("partial chart preparation = %v, %v; want no partial result", desired, err)
	}
}

func TestOperatorDesiredAndDeploymentShareExactReleaseComparison(t *testing.T) {
	request := operatorTestRequest(t)
	pure, err := prepareOperatorDesired(request.Archive, request.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepareOperator(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*release.Release)
		want   operatorReleaseMismatch
	}{
		{name: "exact", mutate: func(*release.Release) {}},
		{name: "wrong status", mutate: func(installed *release.Release) { installed.Info.Status = release.StatusFailed }, want: "HELM_NOT_DEPLOYED"},
		{name: "wrong version", mutate: func(installed *release.Release) { installed.Chart.Metadata.Version = "0.0.0" }, want: "HELM_CHART_DIFFER"},
		{name: "wrong values", mutate: func(installed *release.Release) { installed.Config["unexpected"] = true }, want: "HELM_VALUES_DIFFER"},
		{name: "wrong projection", mutate: func(installed *release.Release) {
			installed.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n"
		}, want: "HELM_MANIFEST_DIFFER"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installed := releaseForPlan(t, plan)
			test.mutate(installed)
			if got := operatorDesiredReleaseMismatchReason(installed, pure); got != test.want {
				t.Fatalf("pure = %s, want %s", got, test.want)
			}
			if got := operatorReleaseMismatchReason(installed, plan); got != test.want {
				t.Fatalf("deploy = %s, want %s", got, test.want)
			}
		})
	}
	// The pure preparation owns chart bytes rather than retaining caller buffers.
	archiveBefore := bytes.Clone(pure.archive)
	request.Archive[0] ^= 0xff
	if !bytes.Equal(pure.archive, archiveBefore) {
		t.Fatal("desired chart aliases caller input")
	}
}

func TestSharedPlanComposesSeparateExecutionControls(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan := requireRunPlan(t, retained)
	first, err := prepareManagementExecution(plan, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareManagementExecution(plan, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.operator == second.operator || first.operator.postRenderer == nil || second.operator.postRenderer == nil || first.reconciler == second.reconciler ||
		first.operator.timeout != time.Minute || second.operator.timeout != 2*time.Minute {
		t.Fatal("shared Plan reused mutable execution, omitted its postrenderer, or retained a stale timeout")
	}
}
