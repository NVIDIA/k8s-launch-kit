// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDesiredVerifierAndDeployShareDefaultingOwnershipAndDrift(t *testing.T) {
	verifier, template := newDesiredVerifierFixture(t)
	unstructured.RemoveNestedField(template.Object, "spec", "maxNodes")
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   string
	}{
		{name: "defaulted equality with status", mutate: func(current *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(current.Object, int64(1000), "spec", "maxNodes")
			current.Object["status"] = map[string]any{"phase": "Ready"}
		}},
		{name: "wrong bundle ownership", mutate: func(current *unstructured.Unstructured) {
			annotations := current.GetAnnotations()
			annotations[BundleDigestAnnotation] = "sha256:" + strings.Repeat("b", 64)
			current.SetAnnotations(annotations)
		}, want: "RESOURCE_OWNERSHIP_CONFLICT"},
		{name: "wrong intent ownership", mutate: func(current *unstructured.Unstructured) {
			annotations := current.GetAnnotations()
			annotations[IntentDigestAnnotation] = "sha256:" + strings.Repeat("b", 64)
			current.SetAnnotations(annotations)
		}, want: "RESOURCE_OWNERSHIP_CONFLICT"},
		{name: "unexpected owner", mutate: func(current *unstructured.Unstructured) {
			current.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Namespace", "name": "other", "uid": "other-uid"}}
		}, want: "RESOURCE_METADATA_CONFLICT"},
		{name: "spec drift", mutate: func(current *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(current.Object, "192.0.2.21", "spec", "clusterEndpoint", "keepalived", "vip")
		}, want: "RESOURCE_CONTENT_CONFLICT"},
		{name: "missing identity", mutate: func(current *unstructured.Unstructured) {
			current.SetUID("")
		}, want: "RESOURCE_UNAVAILABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := template.DeepCopy()
			current, err := verifier.markedDesired(desired)
			if err != nil {
				t.Fatal(err)
			}
			current.SetUID("current-uid")
			current.SetResourceVersion("7")
			test.mutate(current)
			desiredBefore, currentBefore := desired.DeepCopy(), current.DeepCopy()
			pureErr := verifier.Verify(desired, current)
			client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{reconcileKey(desired): current}}
			reconciler := &Reconciler{desiredVerifier: verifier}
			_, disposition, deployErr := reconciler.Ensure(context.Background(), client, desired)
			for name, err := range map[string]error{"pure": pureErr, "deploy": deployErr} {
				if test.want == "" {
					if err != nil {
						t.Fatalf("%s verification: %v", name, err)
					}
					continue
				}
				var typed *ReconciliationError
				if !errors.As(err, &typed) || typed.ReasonCode != test.want {
					t.Fatalf("%s verification = %v, want %s", name, err, test.want)
				}
			}
			if disposition != DispositionExisting || client.gets != 1 || client.creates != 0 {
				t.Fatalf("deploy read verification disposition=%s gets=%d creates=%d", disposition, client.gets, client.creates)
			}
			if !reflect.DeepEqual(desired.Object, desiredBefore.Object) || !reflect.DeepEqual(current.Object, currentBefore.Object) {
				t.Fatal("verification mutated retained or current input")
			}
		})
	}
}

func TestDesiredVerifierRejectsUnavailableInputs(t *testing.T) {
	var absent *desiredVerifier
	if absent.BundleDigest() != "" || absent.Verify(nil, nil) == nil {
		t.Fatal("nil verifier accepted")
	}
	verifier, desired := newDesiredVerifierFixture(t)
	if verifier.Verify(desired, nil) == nil || verifier.Verify(nil, desired) == nil {
		t.Fatal("missing desired/current accepted")
	}
	if _, err := newDesiredVerifier(nil, verifier.BundleDigest(), verifier.controllerMetadata); err == nil {
		t.Fatal("missing rules accepted")
	}
	if _, err := newDesiredVerifier(verifier.rules, "invalid", verifier.controllerMetadata); err == nil {
		t.Fatal("invalid digest accepted")
	}
	if (&Reconciler{}).BundleDigest() != "" {
		t.Fatal("zero reconciler has a bundle identity")
	}
}
