// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A structural CRD with a nested webhook port default and a representative
// schema map. No live API or released chart is needed for the microbenchmark.
func crdNormalizationFixture() *unstructured.Unstructured {
	properties := map[string]any{}
	for i := 0; i < 128; i++ {
		properties[fmt.Sprintf("field%03d", i)] = map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9_-]+$", "description": "A retained schema field whose complete value must remain compared."}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "things.example.test", "uid": "crd-uid", "resourceVersion": "1", "generation": int64(1)},
		"spec": map[string]any{"group": "example.test", "scope": "Namespaced", "names": map[string]any{"plural": "things", "kind": "Thing"},
			"versions":   []any{map[string]any{"name": "v1", "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": properties}}}},
			"conversion": map[string]any{"strategy": "Webhook", "webhook": map[string]any{"conversionReviewVersions": []any{"v1"}, "clientConfig": map[string]any{"caBundle": "AQID", "service": map[string]any{"namespace": "operator-system", "name": "webhook", "path": "/convert"}}}},
		},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}}
}

func crdWithExplicitDefaults(object *unstructured.Unstructured) *unstructured.Unstructured {
	result := object.DeepCopy()
	_ = unstructured.SetNestedField(result.Object, int64(443), "spec", "conversion", "webhook", "clientConfig", "service", "port")
	return result
}

func BenchmarkCRDIdentityMatches(b *testing.B) {
	desired := crdNormalizationFixture()
	expected, err := operatorCRDFromObject(desired)
	if err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		current *unstructured.Unstructured
	}{{"raw-same", desired.DeepCopy()}, {"api-defaulted", crdWithExplicitDefaults(desired)}} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !crdIdentityMatches(expected, test.current) {
					b.Fatal("matching CRD rejected")
				}
			}
		})
	}
}

func TestCRDNormalizationKeepsOwnedExpectedAndFreshCurrent(t *testing.T) {
	desired := crdNormalizationFixture()
	original := desired.DeepCopy()
	expected, err := operatorCRDFromObject(desired)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore := original.Object["spec"]
	same := original.DeepCopy()
	if !crdIdentityMatches(expected, same) || !crdIdentityMatches(expected, crdWithExplicitDefaults(original)) {
		t.Fatal("raw or API-defaulted spec rejected")
	}
	if !reflect.DeepEqual(desired.Object, original.Object) {
		t.Fatal("preparing/defaulting changed caller input")
	}
	// A later caller edit cannot replace the prepared desired spec. Current UID
	// and generation deliberately stay unchanged while its schema changes.
	versions, _, _ := unstructured.NestedSlice(desired.Object, "spec", "versions")
	// Modify the version map explicitly through its copied JSON slice.
	versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["field000"].(map[string]any)["pattern"] = "^changed$"
	_ = unstructured.SetNestedSlice(desired.Object, versions, "spec", "versions")
	if !crdIdentityMatches(expected, same) || crdIdentityMatches(expected, desired) {
		t.Fatal("caller mutation aliased desired state or same-identity live drift was ignored")
	}
	if !reflect.DeepEqual(expected.spec, rawBefore) {
		t.Fatal("comparison changed owned raw expectation")
	}
	changedExpected, err := operatorCRDFromObject(desired)
	if err != nil || !crdIdentityMatches(changedExpected, desired) || crdIdentityMatches(changedExpected, same) {
		t.Fatal("new expectation reused another execution's desired normalization")
	}

}

func TestCRDNormalizationRejectsMalformedDesiredAndLive(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{"typed schema", func(object *unstructured.Unstructured) {
			versions, _, _ := unstructured.NestedSlice(object.Object, "spec", "versions")
			versions[0].(map[string]any)["schema"] = true
			_ = unstructured.SetNestedSlice(object.Object, versions, "spec", "versions")
		}},
		{"typed webhook port", func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "not-an-integer", "spec", "conversion", "webhook", "clientConfig", "service", "port")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			malformed := crdNormalizationFixture()
			test.mutate(malformed)
			expected, err := operatorCRDFromObject(malformed)
			if err == nil || crdIdentityMatches(expected, malformed) {
				t.Fatal("identical malformed desired/live specs were accepted")
			}
			valid, err := operatorCRDFromObject(crdNormalizationFixture())
			if err != nil {
				t.Fatal(err)
			}
			if crdIdentityMatches(valid, malformed) {
				t.Fatal("malformed live typed data accepted")
			}
		})
	}
	expected, err := operatorCRDFromObject(crdNormalizationFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"metadata", "status"} {
		current := crdNormalizationFixture()
		if field == "metadata" {
			current.Object["metadata"].(map[string]any)["labels"] = map[string]any{"invalid": true}
		} else {
			current.Object["status"].(map[string]any)["conditions"] = true
		}
		if crdIdentityMatches(expected, current) {
			t.Fatalf("malformed current %s was ignored", field)
		}
	}
	if _, err := operatorCRDFromObject(nil); err == nil {
		t.Fatal("nil expected CRD accepted")
	}
	if crdIdentityMatches(operatorCRDExpectation{}, crdNormalizationFixture()) {
		t.Fatal("unprepared expected CRD accepted")
	}
}

func TestCRDNormalizationConcurrentFreshObservations(t *testing.T) {
	desired := crdNormalizationFixture()
	expected, err := operatorCRDFromObject(desired)
	if err != nil {
		t.Fatal(err)
	}
	before := desired.DeepCopy()
	var workers sync.WaitGroup
	failures := make(chan string, 8)
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			current := crdWithExplicitDefaults(before)
			for range 8 {
				if !crdIdentityMatches(expected, current) {
					failures <- "independent valid observation rejected"
					return
				}
			}
			_ = unstructured.SetNestedField(current.Object, "Y2hhbmdlZA==", "spec", "conversion", "webhook", "clientConfig", "caBundle")
			if crdIdentityMatches(expected, current) {
				failures <- "same-UID/generation conversion drift accepted"
			}
		})
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if !reflect.DeepEqual(desired.Object, before.Object) {
		t.Fatal("concurrent normalization changed caller input")
	}
}
