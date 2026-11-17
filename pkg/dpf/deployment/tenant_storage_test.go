// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTenantStoragePreflightRecollectsAndKeepsSafeFailure(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	reads := 0
	dynamicClient.PrependReactor("get", "datastores", func(action k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if action.GetNamespace() != "" || action.(k8stesting.GetAction).GetName() != "default" {
			t.Fatal("storage preflight did not inspect the selected cluster-scoped store")
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kamaji.clastix.io", Resource: "datastores"}, "default", fmt.Errorf("sensitive upstream error"))
	})
	api := &APIClient{access: discovery.ManagementClient{Client: client}, dynamic: dynamicClient}
	first := api.checkTenantStorage(context.Background(), "selected-dpf")
	second := api.checkTenantStorage(context.Background(), "selected-dpf")
	observed := (managementprobe.Collector{Kubernetes: client, Dynamic: dynamicClient}).CollectTenantStorage(context.Background(), "selected-dpf")
	if reads != 3 || first != second || first.ID != "external.kamaji.storage" || first.State != "blocked" || observed.State == "passed" || first.ReasonCode != observed.ReasonCode || first.Message != observed.Message {
		t.Fatalf("fresh/shared storage classification differs: reads=%d, first=%#v, second=%#v, observed=%#v", reads, first, second, observed)
	}
	if strings.Contains(first.Message, "sensitive upstream") {
		t.Fatal("storage result leaked untrusted API error contents")
	}
	for _, action := range append(client.Actions(), dynamicClient.Actions()...) {
		if action.GetVerb() != "get" || action.GetResource().Resource == "secrets" {
			t.Fatalf("storage preflight performed an unexpected action: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestTenantStoragePreflightRequiresBoundClientAndServedDataStore(t *testing.T) {
	if got := (*APIClient)(nil).checkTenantStorage(context.Background(), "selected-dpf"); got.State == "passed" {
		t.Fatal("nil management client passed storage preflight")
	}
	found := false
	for _, api := range pinnedHBNAPIs() {
		if api.kind == "DataStore" {
			found = api.group == "kamaji.clastix.io" && api.version == "v1alpha1" && api.resource == "datastores" && !api.namespaced
		}
	}
	if !found {
		t.Fatal("HBN preflight lacks the served cluster-scoped Kamaji DataStore API")
	}
}
