// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type enrollmentScopeClient struct {
	*managementFake
	crdErrors      map[string]error
	resourceErrors map[string]error
	listErrors     map[string]error
	listed         map[string][]unstructured.Unstructured
	listKinds      []string
	identityError  error
	identityCalls  int
	failIdentityAt int
}

func (c *enrollmentScopeClient) VerifyIdentity(context.Context) error {
	c.identityCalls++
	c.events = append(c.events, "verify-enrollment-scope")
	if c.identityError != nil && (c.failIdentityAt == 0 || c.identityCalls >= c.failIdentityAt) {
		return c.identityError
	}
	return nil
}

func (c *enrollmentScopeClient) getOperatorCRD(_ context.Context, name string) (*unstructured.Unstructured, error) {
	if err := c.crdErrors[name]; err != nil {
		return nil, err
	}
	if object := c.crds[name]; object != nil {
		return object.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "customresourcedefinitions"}, name)
}

func (c *enrollmentScopeClient) discoverResourceList(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	if err := c.resourceErrors[groupVersion]; err != nil {
		return nil, err
	}
	return c.managementFake.discoverResourceList(ctx, groupVersion)
}

func (c *enrollmentScopeClient) List(_ context.Context, template *unstructured.Unstructured, _ string) ([]unstructured.Unstructured, error) {
	kind := template.GetKind()
	c.listKinds = append(c.listKinds, kind)
	if err := c.listErrors[kind]; err != nil {
		return nil, err
	}
	items := c.listed[kind]
	result := make([]unstructured.Unstructured, len(items))
	for i := range items {
		result[i] = *items[i].DeepCopy()
	}
	return result, nil
}

func (*enrollmentScopeClient) checkTenantStorage(context.Context, string) PreflightCheck {
	return PreflightCheck{ID: "fixture.storage", State: "passed", Message: "fixture"}
}

func (*enrollmentScopeClient) connectTenant(context.Context, *generation.Plan, types.UID, *Reconciler, *executionJournal, time.Duration, time.Duration) (*tenantClient, error) {
	return &tenantClient{}, nil
}

func (*enrollmentScopeClient) observeTenantSources(context.Context, TenantReadinessExpected) (tenantSources, error) {
	return tenantSources{}, nil
}

func (*enrollmentScopeClient) checkTenantNetwork(context.Context, bootstrap.Tenant, tenantNetworkRunner, string, time.Duration) ([]inventory.Check, error) {
	return nil, nil
}

func (*enrollmentScopeClient) verifyEnrollmentBMCs(context.Context, []enrollmentBMC, time.Duration, *executionJournal) error {
	return nil
}

func (*enrollmentScopeClient) executeEnrollmentSecret(context.Context, ResourceClient, string, string, *executionJournal) (enrollmentSecretReceipt, error) {
	return enrollmentSecretReceipt{}, nil
}

func enrollmentScopeFixture(t *testing.T) (*managementExecution, *enrollmentScopeClient, []enrollmentBMC) {
	t.Helper()
	generated, err := generation.Generate(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client := newEnrollmentScopeClient(t, execution)
	selected := []enrollmentBMC{{device: generation.ResolvedDevice{Serial: "SERIAL-A", PSID: "MT_0000001774", BMCIP: "192.0.2.20", BMCPort: 443}}}
	return execution, client, selected
}

func newEnrollmentScopeClient(t *testing.T, execution *managementExecution) *enrollmentScopeClient {
	t.Helper()
	base := &managementFake{bundleDigest: execution.plan.Digest(), crds: map[string]*unstructured.Unstructured{}, resources: map[string][]metav1.APIResource{}, namespaces: map[string]*unstructured.Unstructured{}, preflight: true, binding: true}
	for _, expected := range execution.crds {
		spec, ok := runtime.DeepCopyJSONValue(expected.spec).(map[string]any)
		if !ok {
			t.Fatalf("expected CRD %s spec is not a JSON object", expected.name)
		}
		crd := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": expected.name, "uid": "crd-" + expected.name, "resourceVersion": "1"},
			"spec":     spec,
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Established", "status": "True"},
				map[string]any{"type": "NamesAccepted", "status": "True"},
			}},
		}}
		base.crds[expected.name] = crd
		for _, version := range expected.versions {
			groupVersion := expected.group + "/" + version
			base.resources[groupVersion] = append(base.resources[groupVersion], metav1.APIResource{Name: expected.plural, Kind: expected.kind, Namespaced: expected.namespaced})
		}
	}
	return &enrollmentScopeClient{managementFake: base, crdErrors: map[string]error{}, resourceErrors: map[string]error{}, listErrors: map[string]error{}, listed: map[string][]unstructured.Unstructured{}}
}

func TestGuardEnrollmentScopeAllowsExactAbsentCRDsButRejectsUnavailableAPIs(t *testing.T) {
	execution, _, selected := enrollmentScopeFixture(t)
	t.Run("exact CRD absence", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.crds = map[string]*unstructured.Unstructured{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err != nil {
			t.Fatalf("exact missing CRDs should prove no existing enrollment objects: %v", err)
		}
		if len(client.listKinds) != 0 {
			t.Fatalf("guard listed APIs after exact CRD absence: %v", client.listKinds)
		}
		if err := inspectEnrollmentScope(ctx, client, execution, selected, true); err == nil {
			t.Fatal("post-bootstrap inspection accepted a disappeared required CRD")
		}
	})

	t.Run("CRD read forbidden", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		name := expectedEnrollmentCRD(t, execution, "DPUDiscovery").name
		client.crdErrors[name] = apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, name, errors.New("denied"))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil || len(client.listKinds) != 0 {
			t.Fatalf("unreadable exact CRD did not block before lists: err=%v lists=%v", err, client.listKinds)
		}
	})

	t.Run("CRD exists but group version unavailable", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		version := "provisioning.dpu.nvidia.com/v1alpha1"
		client.resourceErrors[version] = apierrors.NewNotFound(schema.GroupResource{Resource: "apis"}, version)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil {
			t.Fatal("missing advertised group-version was treated like an absent CRD")
		}
	})
}

func TestGuardEnrollmentScopeBlocksMalformedPartialAndDriftedObservations(t *testing.T) {
	execution, _, selected := enrollmentScopeFixture(t)
	t.Run("schema identity drift", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		crd := expectedEnrollmentCRD(t, execution, "DPUDiscovery")
		current := client.crds[crd.name]
		if err := unstructured.SetNestedField(current.Object, "unexpected", "spec", "names", "plural"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil || len(client.listKinds) != 0 {
			t.Fatalf("CRD schema drift did not block before list: err=%v lists=%v", err, client.listKinds)
		}
	})

	t.Run("partial all namespace list", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.listErrors["DPUDiscovery"] = errors.New("page continuation failed")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil {
			t.Fatal("partial all-namespace list was accepted")
		}
	})

	t.Run("identity changed before scanning", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.identityError = errors.New("management cluster changed")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil || len(client.listKinds) != 0 {
			t.Fatalf("identity drift did not block before scan: err=%v lists=%v", err, client.listKinds)
		}
	})
	t.Run("identity changed after scanning", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.identityError = errors.New("management cluster changed")
		client.failIdentityAt = 2
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil || len(client.listKinds) != 6 {
			t.Fatalf("final identity drift did not block after a complete scan: err=%v lists=%v", err, client.listKinds)
		}
	})

	t.Run("selected device without matching node blocks partial rescan", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.listed["DPUDevice"] = []unstructured.Unstructured{*selectedEnrollmentDevice(selected[0], execution.operatorNamespace)}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := guardEnrollmentScope(ctx, client, execution, selected); err == nil {
			t.Fatal("partial selected DPUDevice enrollment without a DPUNode was accepted")
		}
	})

	t.Run("selected device with matching node is accepted", func(t *testing.T) {
		client := newEnrollmentScopeClient(t, execution)
		client.listed["DPUDevice"] = []unstructured.Unstructured{*selectedEnrollmentDevice(selected[0], execution.operatorNamespace)}
		client.listed["DPUNode"] = []unstructured.Unstructured{*enrollmentNodeWithDevice("dpu-node-serial-a", execution.operatorNamespace, "serial-a")}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := inspectEnrollmentScope(ctx, client, execution, selected, true); err != nil {
			t.Fatalf("exact selected device/node pair was rejected: %v", err)
		}
	})
}

func TestValidateEnrollmentObjectsAllowsOnlySelectedEnrollmentGraph(t *testing.T) {
	execution, _, selected := enrollmentScopeFixture(t)
	item := selected[0]
	operatorNamespace := execution.operatorNamespace
	discoveries := retainedEnrollmentDiscoveries(execution)
	if len(discoveries) != 1 {
		t.Fatalf("fixture retained %d discoveries, want 1", len(discoveries))
	}
	var desiredDiscovery *unstructured.Unstructured
	for _, object := range discoveries {
		desiredDiscovery = object
	}
	listedDiscovery, err := execution.reconciler.markedDesired(desiredDiscovery)
	if err != nil {
		t.Fatalf("mark retained discovery fixture: %v", err)
	}
	listedDiscovery.SetUID(types.UID("discovery-uid"))
	listedDiscovery.SetResourceVersion("1")

	device := selectedEnrollmentDevice(item, operatorNamespace)
	node := enrollmentNodeWithDevice("dpu-node-serial-a", operatorNamespace, "serial-a")

	tests := []struct {
		name     string
		resource enrollmentResource
		object   *unstructured.Unstructured
		wantErr  bool
	}{
		{name: "selected discovery", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDiscovery"}, object: listedDiscovery},
		{name: "unrelated discovery", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDiscovery"}, object: enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUDiscovery", "unrelated", operatorNamespace), wantErr: true},
		{name: "selected device", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDevice"}, object: device},
		{name: "unrelated device", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDevice"}, object: enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUDevice", "serial-b", operatorNamespace), wantErr: true},
		{name: "selected node association", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUNode"}, object: node},
		{name: "unrelated node association", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUNode"}, object: enrollmentNodeWithDevice("dpu-node-serial-a", operatorNamespace, "serial-b"), wantErr: true},
		{name: "unrelated DPU root", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPU"}, object: enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPU", "dpu-a", operatorNamespace), wantErr: true},
		{name: "unrelated DPUSet root", resource: enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUSet"}, object: enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", "set-a", operatorNamespace), wantErr: true},
		{name: "unrelated DPUDeployment root", resource: enrollmentResource{group: "svc.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDeployment"}, object: enrollmentScopeObject("svc.dpu.nvidia.com/v1alpha1", "DPUDeployment", "deployment-a", operatorNamespace), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []unstructured.Unstructured{*test.object.DeepCopy()}
			err := validateEnrollmentObjects(test.resource, objects, execution, selected)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateEnrollmentObjects() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}

	malformed := device.DeepCopy()
	malformed.SetUID("")
	if err := validateEnrollmentObjects(enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUDevice"}, []unstructured.Unstructured{*malformed}, execution, selected); err == nil {
		t.Fatal("object with missing UID was accepted")
	}
	deleting := node.DeepCopy()
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)
	if err := validateEnrollmentObjects(enrollmentResource{group: "provisioning.dpu.nvidia.com", version: "v1alpha1", kind: "DPUNode"}, []unstructured.Unstructured{*deleting}, execution, selected); err == nil {
		t.Fatal("deleting object was accepted")
	}
}

func expectedEnrollmentCRD(t *testing.T, execution *managementExecution, kind string) operatorCRDExpectation {
	t.Helper()
	for _, expected := range execution.crds {
		if expected.kind == kind {
			return expected
		}
	}
	t.Fatalf("retained operator projection lacks %s CRD", kind)
	return operatorCRDExpectation{}
}

func enrollmentScopeObject(apiVersion, kind, name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": "uid-" + name, "resourceVersion": "1"},
	}}
}

func selectedEnrollmentDevice(item enrollmentBMC, namespace string) *unstructured.Unstructured {
	object := enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUDevice", "serial-a", namespace)
	_ = unstructured.SetNestedField(object.Object, item.device.Serial, "spec", "serialNumber")
	_ = unstructured.SetNestedField(object.Object, item.device.BMCIP, "spec", "bmcIp")
	_ = unstructured.SetNestedField(object.Object, int64(item.device.BMCPort), "spec", "bmcPort")
	_ = unstructured.SetNestedField(object.Object, "Never", "spec", "bmcFactoryResetPolicy")
	return object
}

func enrollmentNodeWithDevice(name, namespace, deviceName string) *unstructured.Unstructured {
	object := enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUNode", name, namespace)
	_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": deviceName}}, "spec", "dpus")
	_ = unstructured.SetNestedMap(object.Object, map[string]any{"external": map[string]any{}}, "spec", "nodeRebootMethod")
	return object
}

func TestGuardEnrollmentScopeRecognizesRetainedAPIExpectation(t *testing.T) {
	execution, client, selected := enrollmentScopeFixture(t)
	if _, ok := retainedCRD(execution.crds, "provisioning.dpu.nvidia.com", "DPUDiscovery"); !ok {
		t.Fatal("expected exactly one retained DPUDiscovery CRD")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := guardEnrollmentScope(ctx, client, execution, selected); err != nil {
		t.Fatalf("empty but complete enrollment scope was rejected: %v", err)
	}
	if len(client.listKinds) != 6 {
		t.Fatalf("scope guard inspected %d resource kinds, want all 6: %v", len(client.listKinds), client.listKinds)
	}
	if client.listKinds[0] != "DPUDiscovery" || client.listKinds[len(client.listKinds)-1] != "DPUDeployment" {
		t.Fatalf("unexpected scope inspection order: %v", client.listKinds)
	}
}
