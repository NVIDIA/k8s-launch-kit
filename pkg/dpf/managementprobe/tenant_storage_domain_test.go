// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
)

func setTenantStorageDomain(t *testing.T, fixture *tenantStorageFixture, domain string) {
	t.Helper()
	endpoints, _, err := unstructured.NestedStringSlice(fixture.dataStore.Object, "spec", "endpoints")
	if err != nil {
		t.Fatal(err)
	}
	for i := range endpoints {
		endpoints[i] = strings.Replace(endpoints[i], ".svc.cluster.local:", ".svc."+domain+":", 1)
	}
	if err := unstructured.SetNestedStringSlice(fixture.dataStore.Object, endpoints, "spec", "endpoints"); err != nil {
		t.Fatal(err)
	}
}

func tenantStorageFixtureBytes(t *testing.T, fixture *tenantStorageFixture) []byte {
	t.Helper()
	data, err := json.Marshal([]any{fixture.dataStore, fixture.statefulSet, fixture.service, fixture.pods, fixture.claims, fixture.volumes, fixture.class, fixture.provisioner})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCollectTenantStorageAcceptsConsistentObservedDNSDomain(t *testing.T) {
	for _, domain := range []string{"cluster.local", "corp.example", "region.corp.example", "internal"} {
		t.Run(domain, func(t *testing.T) {
			fixture := newTenantStorageFixture()
			setTenantStorageDomain(t, fixture, domain)
			before := tenantStorageFixtureBytes(t, fixture)
			collector, kube, dynamic := tenantStorageCollectorFromFixture(fixture)
			if result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system"); result.State != "passed" || result.ReasonCode != "TENANT_STORAGE_READY" {
				t.Fatalf("valid observed service DNS domain rejected: %+v", result)
			}
			if !bytes.Equal(before, tenantStorageFixtureBytes(t, fixture)) {
				t.Fatal("storage observation mutated its inputs")
			}
			assertTenantStorageReadOnly(t, kube.Actions())
			assertTenantStorageReadOnly(t, dynamic.Actions())
		})
	}
}

func TestCollectTenantStorageCustomDomainPreservesEndpointAndOwnershipGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*tenantStorageFixture, []string)
	}{
		{"mixed domains", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[1] = strings.Replace(endpoints[1], "corp.example", "other.example", 1)
		}},
		{"mixed services", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[1] = strings.Replace(endpoints[1], ".kamaji-etcd.", ".foreign.", 1)
		}},
		{"foreign service", func(f *tenantStorageFixture, endpoints []string) {
			f.service.Name = "foreign"
			for i := range endpoints {
				endpoints[i] = strings.Replace(endpoints[i], ".kamaji-etcd.", ".foreign.", 1)
			}
		}},
		{"foreign namespace", func(_ *tenantStorageFixture, endpoints []string) {
			for i := range endpoints {
				endpoints[i] = strings.Replace(endpoints[i], ".dpf-operator-system.", ".foreign.", 1)
			}
		}},
		{"foreign ordinal", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[1] = strings.Replace(endpoints[1], "kamaji-etcd-1.", "kamaji-etcd-9.", 1)
		}},
		{"duplicate member", func(_ *tenantStorageFixture, endpoints []string) { endpoints[1] = endpoints[0] }},
		{"empty domain", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", "", 1)
		}},
		{"empty DNS label", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", "corp..example", 1)
		}},
		{"overlong DNS label", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", strings.Repeat("a", 64)+".example", 1)
		}},
		{"invalid DNS label", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", "-corp.example", 1)
		}},
		{"noncanonical domain", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", "Corp.example", 1)
		}},
		{"absolute domain", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], "corp.example", "corp.example.", 1)
		}},
		{"wrong client port", func(_ *tenantStorageFixture, endpoints []string) {
			endpoints[0] = strings.Replace(endpoints[0], ":2379", ":2380", 1)
		}},
		{"HTTPS URL", func(_ *tenantStorageFixture, endpoints []string) { endpoints[0] = "https://" + endpoints[0] }},
		{"HTTP URL", func(_ *tenantStorageFixture, endpoints []string) { endpoints[0] = "http://" + endpoints[0] }},
		{"changed owner UID", func(f *tenantStorageFixture, _ []string) {
			f.pods[0].OwnerReferences[0].UID = types.UID("replaced-statefulset")
		}},
		{"changed service selector", func(f *tenantStorageFixture, _ []string) {
			f.service.Spec.Selector["app.kubernetes.io/name"] = "foreign"
		}},
		{"changed volume claim UID", func(f *tenantStorageFixture, _ []string) { f.volumes[0].Spec.ClaimRef.UID = types.UID("foreign-claim") }},
		{"unready member", func(f *tenantStorageFixture, _ []string) { f.pods[0].Status.ContainerStatuses[0].Ready = false }},
		{"foreign provisioner", func(f *tenantStorageFixture, _ []string) { f.class.Provisioner = "corp.example/local-path-provisioner" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTenantStorageFixture()
			setTenantStorageDomain(t, fixture, "corp.example")
			endpoints, _, _ := unstructured.NestedStringSlice(fixture.dataStore.Object, "spec", "endpoints")
			test.mutate(fixture, endpoints)
			if err := unstructured.SetNestedStringSlice(fixture.dataStore.Object, endpoints, "spec", "endpoints"); err != nil {
				t.Fatal(err)
			}
			before := tenantStorageFixtureBytes(t, fixture)
			collector, kube, dynamic := tenantStorageCollectorFromFixture(fixture)
			if result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system"); result.State == "passed" {
				t.Fatalf("inconsistent custom-domain storage accepted: %+v", result)
			}
			if !bytes.Equal(before, tenantStorageFixtureBytes(t, fixture)) {
				t.Fatal("failed storage observation mutated its inputs")
			}
			assertTenantStorageReadOnly(t, kube.Actions())
			assertTenantStorageReadOnly(t, dynamic.Actions())
		})
	}
}

func TestCollectTenantStorageRejectsDNSDomainDriftBetweenReads(t *testing.T) {
	fixture := newTenantStorageFixture()
	setTenantStorageDomain(t, fixture, "corp.example")
	collector, kube, dynamic := tenantStorageCollectorFromFixture(fixture)
	reads := 0
	dynamic.PrependReactor("get", "datastores", func(action ktesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads != 2 {
			return false, nil, nil
		}
		current := fixture.dataStore.DeepCopy()
		endpoints, _, _ := unstructured.NestedStringSlice(current.Object, "spec", "endpoints")
		for i := range endpoints {
			endpoints[i] = strings.Replace(endpoints[i], ".svc.corp.example:", ".svc.changed.example:", 1)
		}
		if err := unstructured.SetNestedStringSlice(current.Object, endpoints, "spec", "endpoints"); err != nil {
			return true, nil, err
		}
		return true, current, nil
	})
	before := tenantStorageFixtureBytes(t, fixture)
	result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
	if reads != 2 || result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_CHANGED_DURING_CHECK" {
		t.Fatalf("changed observed domain was accepted: reads=%d result=%+v", reads, result)
	}
	if !bytes.Equal(before, tenantStorageFixtureBytes(t, fixture)) {
		t.Fatal("domain drift check mutated input")
	}
	assertTenantStorageReadOnly(t, kube.Actions())
	assertTenantStorageReadOnly(t, dynamic.Actions())
}
