// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKamajiPreflightUsesFreshReadAndHonorsExplicitNamespace(t *testing.T) {
	api := fake.NewSimpleClientset()
	client := &APIClient{access: managementaccess.Client{Client: api}}
	for _, namespace := range []string{"first-kamaji", "other-kamaji"} {
		got := client.checkKamajiCompatibility(context.Background(), namespace, "26.8.0")
		if got.State == "passed" || got.ID != "external.kamaji.compatibility" || got.ReasonCode != "KAMAJI_COMPATIBILITY_AMBIGUOUS" {
			t.Fatalf("missing fresh producer did not block: %+v", got)
		}
	}
	actions := api.Actions()
	if len(actions) != 2 {
		t.Fatalf("expected fresh reads for each call, got %d", len(actions))
	}
	for index, namespace := range []string{"first-kamaji", "other-kamaji"} {
		if actions[index].GetNamespace() != namespace || actions[index].GetVerb() != "list" {
			t.Fatalf("unexpected effects/scope: %#v", actions[index])
		}
	}
	var absent *APIClient
	if got := absent.checkKamajiCompatibility(context.Background(), "kamaji", "26.8.0"); got.State == "passed" {
		t.Fatal(got)
	}
}
