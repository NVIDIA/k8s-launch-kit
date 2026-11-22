// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRunAcceptanceReceiptCollectionOwnsSourceBoundary(t *testing.T) {
	resourceReads, tenantChecks := 0, 0
	fixture := newAPIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/prefix/api/v1/namespaces/dpf-system/configmaps/selected" {
			t.Errorf("unexpected receipt read %s %s", request.Method, request.URL.Path)
		}
		resourceReads++
		fmt.Fprintf(w, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"selected","namespace":"dpf-system","resourceVersion":"%d"}}`, resourceReads)
	})
	client, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	tenant := &runtimeTenantReadOnlyFake{identity: func(context.Context) error { tenantChecks++; return nil }}
	before := fixture.calls.Load()
	err = client.withAcceptanceReceiptReads(context.Background(), tenant, func(reader activationControlReader, _ acceptanceTenantReceiptReader) error {
		for _, method := range []string{"Create", "patchEnrollmentSelectors", "verifyEnrollmentBMCs", "getOperation"} {
			if _, found := reflect.TypeOf(reader).MethodByName(method); found {
				t.Errorf("receipt collection exposed %s", method)
			}
		}
		for i := 1; i <= 2; i++ {
			object, err := reader.Get(context.Background(), desiredConfigMap())
			if err != nil || object.GetResourceVersion() != fmt.Sprint(i) {
				t.Fatalf("receipt read was not current: object=%v err=%v", object, err)
			}
		}
		return nil
	})
	// Two complete source boundaries, plus fresh mapping and object per Get.
	// Per-object identity recursion would add eight extra identity requests.
	if err != nil || tenantChecks != 2 || resourceReads != 2 || fixture.calls.Load()-before != 8 {
		t.Fatalf("receipt boundary err=%v tenant=%d reads=%d HTTP=%d", err, tenantChecks, resourceReads, fixture.calls.Load()-before)
	}
}

func TestRunAcceptanceReceiptCollectionClosesSourcesAfterReadFailure(t *testing.T) {
	fixture := newAPIFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected resource read") })
	client, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	tenantChecks := 0
	closingTenantErr, readErr := errors.New("closing tenant failed"), errors.New("read failed")
	tenant := &runtimeTenantReadOnlyFake{identity: func(context.Context) error {
		tenantChecks++
		if tenantChecks == 2 {
			return closingTenantErr
		}
		return nil
	}}
	err = client.withAcceptanceReceiptReads(context.Background(), tenant, func(activationControlReader, acceptanceTenantReceiptReader) error {
		fixture.uid.Store("replaced-during-collection")
		return readErr
	})
	if !errors.Is(err, errManagementIdentityChanged) || !errors.Is(err, closingTenantErr) || !errors.Is(err, readErr) || tenantChecks != 2 {
		t.Fatalf("closing source evidence lost: tenant=%d err=%v", tenantChecks, err)
	}
}

func TestRunAcceptanceReceiptCollectionRefreshesDiscovery(t *testing.T) {
	mapping := "dpuclusters"
	paths := []string{}
	fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1":
			fmt.Fprintf(w, `{"groupVersion":"provisioning.dpu.nvidia.com/v1alpha1","resources":[{"name":%q,"kind":"DPUCluster","namespaced":true}]}`, mapping)
		case "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/system/dpuclusters/tenant", "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/system/replacedclusters/tenant":
			paths = append(paths, r.URL.Path)
			fmt.Fprint(w, `{"apiVersion":"provisioning.dpu.nvidia.com/v1alpha1","kind":"DPUCluster","metadata":{"name":"tenant","namespace":"system"}}`)
		default:
			t.Errorf("unexpected receipt path %s", r.URL.Path)
		}
	})
	client, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	tenant := &runtimeTenantReadOnlyFake{identity: func(context.Context) error { return nil }}
	object := validationObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", "system", "tenant")
	for _, next := range []string{"dpuclusters", "replacedclusters"} {
		mapping = next
		err := client.withAcceptanceReceiptReads(context.Background(), tenant, func(reader activationControlReader, _ acceptanceTenantReceiptReader) error {
			_, err := reader.Get(context.Background(), object)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(paths, []string{"/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/system/dpuclusters/tenant", "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/system/replacedclusters/tenant"}) {
		t.Fatalf("receipt mapping was cached across its source boundary: %v", paths)
	}
}

func (f *runtimeTenantReadOnlyFake) currentAcceptanceReceiptReads() acceptanceTenantReceiptReader {
	return f
}

func TestRunAcceptanceTenantReceiptReadsUseCurrentObjectsInsideSourceBoundary(t *testing.T) {
	h := newNativeRuntimeHarness(t)
	ports := h.transports()
	ctx := context.Background()
	plan := h.fixture.prepared.execution.plan
	client, err := newAPIClient(ctx, h.access, plan.Intent().Management, plan.Intent().KubernetesVersion, nil, ports.tenantDial, ports.requestLimiter)
	if err != nil {
		t.Fatal(err)
	}
	_, observer, err := compileValidationObserver(plan.Bundle(), plan, h.access, nil, validation.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	observer.runtime = validationRuntimeTransports{tenantDial: ports.tenantDial, requestLimiter: ports.requestLimiter}
	tenant, err := observer.openValidationTenant(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := retainedWorkloadIPAM(plan)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := client.Get(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	err = client.withAcceptanceReceiptReads(ctx, tenant, func(_ activationControlReader, reads acceptanceTenantReceiptReader) error {
		before := h.api.calls.Load()
		first, err := reads.getWorkloadChild(ctx, parent)
		if err != nil {
			return err
		}
		path := nativeResourcePath(first, "cidrpools")
		h.mu.Lock()
		replacement := h.tenantGets[path].(*unstructured.Unstructured).DeepCopy()
		replacement.SetUID("replaced-current-pool")
		h.tenantGets[path] = replacement
		h.mu.Unlock()
		second, err := reads.getWorkloadChild(ctx, parent)
		if err != nil {
			return err
		}
		if second.GetUID() == first.GetUID() || second.GetUID() != "replaced-current-pool" {
			t.Fatalf("bounded child reads reused an object: first=%s second=%s", first.GetUID(), second.GetUID())
		}
		if h.api.calls.Load() != before {
			t.Fatal("individual child read reopened management source verification")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunAcceptanceReceiptCollectionRejectsUnavailableOpeningSource(t *testing.T) {
	for _, scenario := range []string{"management", "tenant", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAPIFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected resource read") })
			client, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tenantErr := errors.New("tenant source unavailable")
			tenant := &runtimeTenantReadOnlyFake{identity: func(context.Context) error {
				if scenario == "tenant" {
					return tenantErr
				}
				return nil
			}}
			switch scenario {
			case "management":
				fixture.uid.Store("replaced-before-collection")
			case "canceled":
				cancel()
			}
			err = client.withAcceptanceReceiptReads(ctx, tenant, func(activationControlReader, acceptanceTenantReceiptReader) error {
				t.Fatal("receipt collection began without valid opening sources")
				return nil
			})
			if err == nil || (scenario == "management" && !errors.Is(err, errManagementIdentityChanged)) || (scenario == "tenant" && !errors.Is(err, tenantErr)) || (scenario == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("opening source classification lost: %v", err)
			}
		})
	}
}

func (f *runtimeTenantReadOnlyFake) listCompleteNodes(context.Context) ([]unstructured.Unstructured, error) {
	return nil, fmt.Errorf("unexpected tenant Node read")
}

func TestRunAcceptanceCurrentLeaseCollectionKeepsAllSourceAndDataFences(t *testing.T) {
	state := &hostLeaseManagementState{}
	fixture := newAPIFixture(t, state.serveManagement)
	plan, reconciler := prepareHostLeaseObservationPlan(t, fixture)
	for _, scenario := range []string{"healthy", "parent", "pool-replaced", "pool-allocation", "node-list", "management-source", "tenant-source"} {
		t.Run(scenario, func(t *testing.T) {
			state.reset()
			fixture.uid.Store("cluster-one")
			h := newHostLeaseObservationHarnessFromPlan(t, state, fixture, plan, reconciler)
			switch scenario {
			case "parent":
				state.ipamMutation = "spec"
			case "pool-replaced":
				state.poolMutation = "replace"
			case "pool-allocation":
				state.poolMutation = "allocations"
			case "node-list":
				h.tenantState.listMode = "forbidden"
			case "management-source":
				h.tenantState.listMode = "replace-management"
			case "tenant-source":
				h.tenantState.listMode = "replace-tenant"
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			inputs, err := prepareHostLeaseObservation(ctx, h.management, h.tenant, h.input.plan, h.input.reconciler, newWorkloadLeaseInputs(h.workload), 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot hostWorkloadLeaseSnapshot
			err = h.management.withAcceptanceReceiptReads(ctx, h.tenant, func(management activationControlReader, tenant acceptanceTenantReceiptReader) error {
				var err error
				snapshot, err = collectCurrentHostWorkloadLeaseSnapshot(ctx, management, tenant, inputs)
				return err
			})
			if scenario == "healthy" {
				if err != nil || len(snapshot.TenantNodes) != len(h.tenantState.nodes) || state.ipamReads != 2 || state.poolReads != 2 || h.tenantState.listCalls != 2 {
					t.Fatalf("incomplete healthy lease snapshot: err=%v parent=%d pool=%d nodes=%d", err, state.ipamReads, state.poolReads, h.tenantState.listCalls)
				}
			} else if err == nil {
				t.Fatal("bounded receipt collection accepted source/data drift")
			}
			if scenario == "management-source" && !errors.Is(err, errManagementIdentityChanged) {
				t.Fatalf("closing management source drift lost: %v", err)
			}
			if scenario == "tenant-source" && !errors.Is(err, errTenantIdentityChanged) {
				t.Fatalf("closing tenant source drift lost: %v", err)
			}
		})
	}
}

func TestRunAcceptanceLeasePreparationRequiresOriginalClientInstances(t *testing.T) {
	h := newHostLeaseObservationHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	before := h.apiFixture.calls.Load()
	copiedManagement := *h.management
	copiedTenant := *h.tenant
	for _, source := range []struct {
		management *APIClient
		tenant     *tenantClient
	}{{management: &copiedManagement, tenant: h.tenant}, {management: h.management, tenant: &copiedTenant}} {
		_, err := prepareHostLeaseObservation(ctx, source.management, source.tenant, h.input.plan, h.input.reconciler, newWorkloadLeaseInputs(h.workload), 15*time.Second)
		if err == nil || h.apiFixture.calls.Load() != before || h.tenantState.listCalls != 0 {
			t.Fatalf("substituted original source reached collection: err=%v", err)
		}
	}
}
