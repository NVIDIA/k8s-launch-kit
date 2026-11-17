// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	sigsyaml "sigs.k8s.io/yaml"
)

type reconcileFixture struct {
	objects map[string]*unstructured.Unstructured
	gets    int
	creates int
	create  func(*unstructured.Unstructured) (*unstructured.Unstructured, error)
}

func (f *reconcileFixture) Get(_ context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.gets++
	object := f.objects[reconcileKey(desired)]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "provisioning.dpu.nvidia.com", Resource: "dpuclusters"}, desired.GetName())
	}
	return object.DeepCopy(), nil
}

func (f *reconcileFixture) Create(_ context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.creates++
	if dryRun {
		return nil, errors.New("unexpected dry run")
	}
	if f.create != nil {
		return f.create(desired.DeepCopy())
	}
	copy := desired.DeepCopy()
	copy.SetUID("created-uid")
	copy.SetResourceVersion("1")
	f.objects[reconcileKey(copy)] = copy.DeepCopy()
	return copy, nil
}

func reconcileKey(object *unstructured.Unstructured) string {
	return object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
}

func newReconcileFixture(t *testing.T) (*Reconciler, *unstructured.Unstructured) {
	t.Helper()
	archive, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "operator", "dpf-operator-v26.8.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := bootstrap.OperatorRenderInputs{
		ReleaseVersion: "26.8.0", ReleaseName: "dpf-operator", KubernetesVersion: "1.35.4", Values: map[string]any{},
		Bootstrap: bootstrap.Inputs{
			OperatorNamespace: "dpf-system", OperatorConfigName: "dpf-config", BMCFactoryResetPolicy: bootstrap.BMCFactoryResetPolicyNever,
			ManagementAPIIP: "192.0.2.10", ManagementAPIPort: 6443, PodCIDR: "10.244.0.0/14",
			Tenant:  bootstrap.Tenant{Name: "hbn-tenant", Namespace: "hbn-tenant", Labels: map[string]string{"l8k.example/tenant": "hbn"}, VIP: "192.0.2.20", Interface: "eth0", RouterID: 126, NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}, MaxNodes: 2, ServiceCIDR: "10.96.0.0/16"},
			Devices: []bootstrap.Device{{Serial: "SERIAL-A", BMCIP: "192.0.2.100", BMCPort: 443}},
		},
	}
	rendered, err := bootstrap.RenderOperator(archive, inputs)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(rendered.Manifest, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	files, err := bootstrap.Render(inputs.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	var cluster *unstructured.Unstructured
	for _, file := range files {
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(file.Data, &object.Object); err != nil {
			t.Fatal(err)
		}
		if object.GetKind() == "DPUCluster" {
			cluster = object
			break
		}
	}
	if cluster == nil {
		t.Fatal("bootstrap renderer did not emit DPUCluster")
	}
	return reconciler, cluster
}

func TestReconcilerCreateThenExactVerifyAllowsSchemaDefaultsAndStatus(t *testing.T) {
	r, desired := newReconcileFixture(t)
	unstructured.RemoveNestedField(desired.Object, "spec", "maxNodes")
	client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}}
	created, disposition, err := r.Ensure(context.Background(), client, desired)
	if err != nil || disposition != DispositionCreated || created == nil {
		t.Fatalf("Ensure() = %v, %q, %v", created, disposition, err)
	}
	if client.gets != 1 || client.creates != 1 {
		t.Fatalf("calls = get %d/create %d, want 1/1", client.gets, client.creates)
	}
	actual := client.objects[reconcileKey(desired)].DeepCopy()
	if err := unstructured.SetNestedField(actual.Object, int64(1000), "spec", "maxNodes"); err != nil {
		t.Fatal(err)
	}
	actual.Object["status"] = map[string]any{"phase": "Ready", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	client.objects[reconcileKey(desired)] = actual
	got, disposition, err := r.Ensure(context.Background(), client, desired)
	if err != nil || disposition != DispositionExisting || got == nil {
		t.Fatalf("Ensure(existing defaulted object) = %v, %q, %v", got, disposition, err)
	}
	if client.creates != 1 {
		t.Fatalf("existing exact resource triggered %d creates", client.creates-1)
	}
}

func TestReconcilerRejectsUnownedModifiedOrDeletingObjects(t *testing.T) {
	r, template := newReconcileFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "missing ownership marker", mutate: func(object *unstructured.Unstructured) { object.SetAnnotations(nil) }},
		{name: "wrong bundle", mutate: func(object *unstructured.Unstructured) {
			annotations := object.GetAnnotations()
			annotations[BundleDigestAnnotation] = "sha256:" + strings.Repeat("b", 64)
			object.SetAnnotations(annotations)
		}},
		{name: "extra metadata label", mutate: func(object *unstructured.Unstructured) {
			labels := object.GetLabels()
			labels["untrusted.example/extra"] = "1"
			object.SetLabels(labels)
		}},
		{name: "unexpected owner reference", mutate: func(object *unstructured.Unstructured) {
			object.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Namespace", "name": "other", "uid": "other"}}
		}},
		{name: "changed VIP", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "192.0.2.21", "spec", "clusterEndpoint", "keepalived", "vip")
		}},
		{name: "unexpected spec field", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "unsafe", "spec", "unknownOption")
		}},
		{name: "unexpected kubeconfig", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "unrelated-secret", "spec", "kubeconfig")
		}},
		{name: "deleting", mutate: func(object *unstructured.Unstructured) {
			timestamp := "2026-11-14T00:00:00Z"
			object.Object["metadata"].(map[string]any)["deletionTimestamp"] = timestamp
		}},
		{name: "missing UID", mutate: func(object *unstructured.Unstructured) { object.SetUID("") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := template.DeepCopy()
			marked, err := r.markedDesired(desired)
			if err != nil {
				t.Fatal(err)
			}
			marked.SetUID("existing-uid")
			marked.SetResourceVersion("3")
			test.mutate(marked)
			client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{reconcileKey(desired): marked}}
			if _, disposition, err := r.Ensure(context.Background(), client, desired); err == nil || disposition != DispositionExisting {
				t.Fatalf("Ensure() = %q, %v, want conflict", disposition, err)
			} else {
				var reconciliationErr *ReconciliationError
				if !errors.As(err, &reconciliationErr) || reconciliationErr.ReasonCode == "" {
					t.Fatalf("Ensure() returned untyped reconciliation error: %T %v", err, err)
				}
			}
			if client.creates != 0 {
				t.Fatal("conflicting existing object triggered create")
			}
		})
	}
}

func TestReconcilerAllowsOnlyDocumentedDPUClusterControllerFields(t *testing.T) {
	r, desired := newReconcileFixture(t)
	current, err := r.markedDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID("cluster-uid")
	current.SetResourceVersion("8")
	if err := unstructured.SetNestedField(current.Object, desired.GetName()+"-admin-kubeconfig", "spec", "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	current.SetFinalizers([]string{"provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up", "provisioning.dpu.nvidia.com/cluster-manager-clean-up"})
	if err := r.Verify(desired, current); err != nil {
		t.Fatalf("Verify() rejected documented DPUCluster controller fields: %v", err)
	}
}

func TestReconcilerAllowsOnlyPinnedDPUDeploymentDependencyMetadata(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceTemplate", "metadata": map[string]any{"name": "hbn", "namespace": "dpf-system", "labels": map[string]any{"profile.example/name": "hbn"}}}}
	current := desired.DeepCopy()
	labels := current.GetLabels()
	labels["svc.dpu.nvidia.com/consumed-by-dpudeployment-0123456789"] = ""
	current.SetLabels(labels)
	if !labelsMatch(desired, current) {
		t.Fatal("labelsMatch() rejected the pinned DPF dependent-object label")
	}
	labels["untrusted.example/extra"] = "x"
	current.SetLabels(labels)
	if labelsMatch(desired, current) {
		t.Fatal("labelsMatch() accepted an unrelated extra metadata label")
	}
}

func TestReconcilerCreateRaceReReadsAndDoesNotAdoptMismatch(t *testing.T) {
	t.Run("matching object", func(t *testing.T) {
		r, desired := newReconcileFixture(t)
		client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}}
		client.create = func(marked *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			marked.SetUID("raced-uid")
			marked.SetResourceVersion("2")
			client.objects[reconcileKey(marked)] = marked.DeepCopy()
			return nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "provisioning.dpu.nvidia.com", Resource: "dpuclusters"}, marked.GetName())
		}
		got, disposition, err := r.Ensure(context.Background(), client, desired)
		if err != nil || disposition != DispositionExistingAfterRace || got == nil || client.gets != 2 || client.creates != 1 {
			t.Fatalf("Ensure() = %v, %q, %v; calls get=%d create=%d", got, disposition, err, client.gets, client.creates)
		}
	})
	t.Run("conflicting object", func(t *testing.T) {
		r, desired := newReconcileFixture(t)
		client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}}
		client.create = func(marked *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			marked.SetUID("other-uid")
			marked.SetResourceVersion("2")
			_ = unstructured.SetNestedField(marked.Object, "192.0.2.99", "spec", "clusterEndpoint", "keepalived", "vip")
			client.objects[reconcileKey(marked)] = marked.DeepCopy()
			return nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "provisioning.dpu.nvidia.com", Resource: "dpuclusters"}, marked.GetName())
		}
		if _, disposition, err := r.Ensure(context.Background(), client, desired); err == nil || disposition != DispositionExistingAfterRace || client.gets != 2 {
			t.Fatalf("Ensure() = %q, %v, gets=%d; want conflict after reread", disposition, err, client.gets)
		}
	})
}

func TestReconcilerReportsUncertainCreateErrorsAndPostCreateMismatch(t *testing.T) {
	r, desired := newReconcileFixture(t)
	client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}, create: func(*unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return nil, context.DeadlineExceeded
	}}
	if _, disposition, err := r.Ensure(context.Background(), client, desired); err == nil || disposition != DispositionUncertain || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure() = %q, %v; want uncertain write result", disposition, err)
	} else {
		var reconciliationErr *ReconciliationError
		if !errors.As(err, &reconciliationErr) || reconciliationErr.ReasonCode != "RESOURCE_CREATE_UNCERTAIN" {
			t.Fatalf("Ensure() error = %#v, want RESOURCE_CREATE_UNCERTAIN", err)
		}
	}
	client = &reconcileFixture{objects: map[string]*unstructured.Unstructured{}, create: func(marked *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		marked.SetUID("created-uid")
		marked.SetResourceVersion("4")
		_ = unstructured.SetNestedField(marked.Object, "192.0.2.22", "spec", "clusterEndpoint", "keepalived", "vip")
		return marked, nil
	}}
	if _, disposition, err := r.Ensure(context.Background(), client, desired); err == nil || disposition != DispositionCreated || !strings.Contains(err.Error(), "re-read before retry") {
		t.Fatalf("Ensure() = %q, %v; want partial create diagnostic", disposition, err)
	}
}

func TestReconcilerRestrictsOperatorConfigFinalizerToItsOwnKind(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		finalizers []string
		accepted   bool
	}{
		{name: "config without finalizer", kind: "DPFOperatorConfig", finalizers: nil, accepted: true},
		{name: "config own finalizer", kind: "DPFOperatorConfig", finalizers: []string{"dpu.nvidia.com/dpfoperatorconfig"}, accepted: true},
		{name: "duplicate", kind: "DPFOperatorConfig", finalizers: []string{"dpu.nvidia.com/dpfoperatorconfig", "dpu.nvidia.com/dpfoperatorconfig"}, accepted: false},
		{name: "unknown", kind: "DPFOperatorConfig", finalizers: []string{"untrusted-sentinel"}, accepted: false},
		{name: "other controller on config", kind: "DPFOperatorConfig", finalizers: []string{"dpu.nvidia.com/dpudeployment"}, accepted: false},
		{name: "config controller on cluster", kind: "DPUCluster", finalizers: []string{"dpu.nvidia.com/dpfoperatorconfig"}, accepted: false},
		{name: "config controller on dependency", kind: "DPUServiceTemplate", finalizers: []string{"dpu.nvidia.com/dpfoperatorconfig"}, accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := &unstructured.Unstructured{}
			object.SetKind(tc.kind)
			object.SetFinalizers(tc.finalizers)
			if err := validateControllerMetadata(object, nil); (err == nil) != tc.accepted {
				t.Fatalf("finalizer acceptance=%v, want %v", err == nil, tc.accepted)
			}
		})
	}
}

func TestReconcilerRecognizesOnlyPinnedStaticControllerFinalizers(t *testing.T) {
	// These combinations come from the qualified 26.8 controller source and
	// retained manual lifecycle metadata. DPUSet-specific BFS finalizers need
	// separate plan-bound ownership proof and are deliberately not allowed here.
	for _, tc := range []struct {
		name, kind string
		finalizers []string
		accepted   bool
	}{
		{name: "cluster controller combination", kind: "DPUCluster", finalizers: []string{"provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up", "provisioning.dpu.nvidia.com/cluster-manager-clean-up"}, accepted: true},
		{name: "BFS static controller combination", kind: "BlueFieldSoftware", finalizers: []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "dpu.nvidia.com/dpudeployment"}, accepted: true},
		{name: "template controller combination", kind: "DPUServiceTemplate", finalizers: []string{"dpu.nvidia.com/dpuservicetemplate", "dpu.nvidia.com/dpudeployment"}, accepted: true},
		{name: "service interface controller", kind: "DPUServiceInterface", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceinterface"}, accepted: true},
		{name: "service IPAM controller", kind: "DPUServiceIPAM", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceipam"}, accepted: true},
		{name: "cluster duplicate", kind: "DPUCluster", finalizers: []string{"provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up", "provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up"}, accepted: false},
		{name: "BFS duplicate", kind: "BlueFieldSoftware", finalizers: []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "provisioning.dpu.nvidia.com/bluefieldsoftware-protection"}, accepted: false},
		{name: "template duplicate", kind: "DPUServiceTemplate", finalizers: []string{"dpu.nvidia.com/dpuservicetemplate", "dpu.nvidia.com/dpuservicetemplate"}, accepted: false},
		{name: "interface duplicate", kind: "DPUServiceInterface", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceinterface", "svc.dpu.nvidia.com/dpuserviceinterface"}, accepted: false},
		{name: "IPAM duplicate", kind: "DPUServiceIPAM", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceipam", "svc.dpu.nvidia.com/dpuserviceipam"}, accepted: false},
		{name: "cluster token on BFS", kind: "BlueFieldSoftware", finalizers: []string{"provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up"}, accepted: false},
		{name: "BFS token on cluster", kind: "DPUCluster", finalizers: []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection"}, accepted: false},
		{name: "template token on interface", kind: "DPUServiceInterface", finalizers: []string{"dpu.nvidia.com/dpuservicetemplate"}, accepted: false},
		{name: "interface token on IPAM", kind: "DPUServiceIPAM", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceinterface"}, accepted: false},
		{name: "IPAM token on template", kind: "DPUServiceTemplate", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceipam"}, accepted: false},
		{name: "unknown token with valid interface", kind: "DPUServiceInterface", finalizers: []string{"svc.dpu.nvidia.com/dpuserviceinterface", "untrusted-sentinel"}, accepted: false},
		{name: "dynamic BFS unproven", kind: "BlueFieldSoftware", finalizers: []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "dpu.nvidia.com/dpudeployment", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-bgp-dpu-74abadc2ddbdca64", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-bgp-dpu-4b30517bb67d5c9c"}, accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := &unstructured.Unstructured{}
			object.SetKind(tc.kind)
			object.SetFinalizers(tc.finalizers)
			before := object.DeepCopy()
			if err := validateControllerMetadata(object, nil); (err == nil) != tc.accepted {
				t.Fatalf("finalizer acceptance=%v, want %v", err == nil, tc.accepted)
			}
			if !reflect.DeepEqual(before.Object, object.Object) {
				t.Fatal("controller metadata validation mutated its input")
			}
		})
	}
}

// flavorAdmissionResponse models only the four value-struct JSON fields in the
// pinned DPUFlavor admission type. It is a serialization oracle, not production
// normalization; test inputs contain no other fields that it could discard.
func flavorAdmissionResponse(t *testing.T, marked *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	var spec struct {
		Grub struct {
			KernelParameters []string `json:"kernelParameters,omitempty"`
		} `json:"grub,omitempty"`
		Sysctl struct {
			Parameters []string `json:"parameters,omitempty"`
		} `json:"sysctl,omitempty"`
		OVS struct {
			RawConfigScript string `json:"rawConfigScript,omitempty"`
		} `json:"ovs,omitempty"`
		ContainerdConfig struct {
			RegistryEndpoint string `json:"registryEndpoint,omitempty"`
		} `json:"containerdConfig,omitempty"`
	}
	data, err := json.Marshal(marked.Object["spec"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var admitted map[string]any
	if err := json.Unmarshal(data, &admitted); err != nil {
		t.Fatal(err)
	}
	response := marked.DeepCopy()
	response.Object["spec"] = admitted
	response.SetUID("flavor-created-uid")
	response.SetResourceVersion("1")
	response.SetGeneration(1)
	return response
}

func flavorReconcileDesired(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUFlavor",
		"metadata": map[string]any{"name": "hbn-flavor", "namespace": "dpf-system"}, "spec": spec,
	}}
}

func TestReconcilerDPUFlavorAdmissionCreateAndReadback(t *testing.T) {
	r, _ := newReconcileFixture(t)
	// These nonempty sections model the retained HBN shape. The other two
	// sections are omitted, while typed admission serializes them as {}.
	desired := flavorReconcileDesired(map[string]any{
		"grub": map[string]any{"kernelParameters": []any{"console=ttyAMA0"}},
		"ovs":  map[string]any{"rawConfigScript": "echo hbn-test"},
	})
	before := desired.DeepCopy()
	client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}}
	client.create = func(marked *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		response := flavorAdmissionResponse(t, marked)
		client.objects[reconcileKey(marked)] = response.DeepCopy()
		return response, nil
	}
	created, disposition, err := r.Ensure(context.Background(), client, desired)
	if err != nil || disposition != DispositionCreated || created == nil {
		t.Fatalf("typed admission create rejected: disposition=%s err=%v", disposition, err)
	}
	for _, field := range []string{"sysctl", "containerdConfig"} {
		value, found, err := unstructured.NestedMap(created.Object, "spec", field)
		if err != nil || !found || len(value) != 0 {
			t.Fatalf("serialization oracle omitted empty struct %s", field)
		}
	}
	serverBefore := client.objects[reconcileKey(desired)].DeepCopy()
	if err := r.Verify(desired, created); err != nil {
		t.Fatalf("admitted readback rejected: %v", err)
	}
	existing, disposition, err := r.Ensure(context.Background(), client, desired)
	if err != nil || disposition != DispositionExisting || existing == nil || client.creates != 1 {
		t.Fatalf("existing admitted object rejected or recreated: disposition=%s creates=%d err=%v", disposition, client.creates, err)
	}
	if !reflect.DeepEqual(before.Object, desired.Object) || !reflect.DeepEqual(serverBefore.Object, client.objects[reconcileKey(desired)].Object) {
		t.Fatal("verification changed caller or observed objects")
	}
}

func fullFlavorSpec() map[string]any {
	return map[string]any{
		"grub":             map[string]any{"kernelParameters": []any{"console=ttyAMA0"}},
		"sysctl":           map[string]any{"parameters": []any{"net.ipv4.ip_forward=1"}},
		"ovs":              map[string]any{"rawConfigScript": "echo hbn-test"},
		"containerdConfig": map[string]any{"registryEndpoint": "https://registry.example.test"},
	}
}

func TestReconcilerDPUFlavorAdmissionFourSectionMatrix(t *testing.T) {
	r, _ := newReconcileFixture(t) // Immutable schema compilation shared by rows.
	for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig", "all-omitted"} {
		t.Run(field, func(t *testing.T) {
			spec := fullFlavorSpec()
			if field == "all-omitted" {
				spec = map[string]any{}
			} else {
				delete(spec, field)
			}
			desired := flavorReconcileDesired(spec)
			marked, err := r.markedDesired(desired)
			if err != nil {
				t.Fatal(err)
			}
			current := flavorAdmissionResponse(t, marked)
			beforeDesired, beforeCurrent := desired.DeepCopy(), current.DeepCopy()
			client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{reconcileKey(desired): current.DeepCopy()}}
			if _, disposition, err := r.Ensure(context.Background(), client, desired); err != nil || disposition != DispositionExisting || client.creates != 0 {
				t.Fatalf("existing admission object rejected: disposition=%s err=%v", disposition, err)
			}
			if err := r.Verify(desired, current); err != nil {
				t.Fatalf("schema-valid admitted field %s rejected: %v", field, err)
			}
			if !reflect.DeepEqual(beforeDesired.Object, desired.Object) || !reflect.DeepEqual(beforeCurrent.Object, current.Object) || !reflect.DeepEqual(beforeCurrent.Object, client.objects[reconcileKey(desired)].Object) {
				t.Fatal("admission comparison changed input maps")
			}
		})
	}
}

func TestReconcilerDPUFlavorAdmissionPreservesSemanticAndOwnershipConflicts(t *testing.T) {
	r, _ := newReconcileFixture(t)
	for _, tc := range []struct {
		name    string
		prepare func(*unstructured.Unstructured)
		change  func(*unstructured.Unstructured)
	}{
		{name: "grub content", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(o.Object, []string{"console=foreign"}, "spec", "grub", "kernelParameters")
		}},
		{name: "sysctl content", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringSlice(o.Object, []string{"net.ipv4.ip_forward=0"}, "spec", "sysctl", "parameters")
		}},
		{name: "ovs content", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "echo foreign", "spec", "ovs", "rawConfigScript")
		}},
		{name: "containerd content", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "https://foreign.example.test", "spec", "containerdConfig", "registryEndpoint")
		}},
		{name: "nonempty to empty", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "spec", "sysctl")
		}},
		{name: "nonempty to missing", change: func(o *unstructured.Unstructured) { unstructured.RemoveNestedField(o.Object, "spec", "grub") }},
		{name: "null follows retained schema", change: func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, nil, "spec", "sysctl") }},
		{name: "wrong section type", change: func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, false, "spec", "sysctl") }},
		{name: "wrong section list", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{}, "spec", "containerdConfig")
		}},
		{name: "unknown empty field", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "spec", "unrecognizedEmpty")
		}},
		{name: "unqualified known empty object", change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "spec", "serviceReadiness")
		}},
		{name: "explicit false", prepare: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"portNumber": int64(0), "dhcp": false}}, "spec", "hostNetworkInterfaceConfigs")
		}, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"portNumber": int64(0)}}, "spec", "hostNetworkInterfaceConfigs")
		}},
		{name: "explicit zero", prepare: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"portNumber": int64(0)}}, "spec", "hostNetworkInterfaceConfigs")
		}, change: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"portNumber": int64(1)}}, "spec", "hostNetworkInterfaceConfigs")
		}},
		{name: "explicit empty list", prepare: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []any{}, "spec", "bfcfgParameters")
		}, change: func(o *unstructured.Unstructured) {
			unstructured.RemoveNestedField(o.Object, "spec", "bfcfgParameters")
		}},
		{name: "namespace", change: func(o *unstructured.Unstructured) { o.SetNamespace("foreign") }},
		{name: "api version", change: func(o *unstructured.Unstructured) { o.SetAPIVersion("provisioning.dpu.nvidia.com/v1alpha2") }},
		{name: "kind", change: func(o *unstructured.Unstructured) { o.SetKind("DPUFlavorTemplate") }},
		{name: "ownership marker", change: func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[IntentDigestAnnotation] = "sha256:" + strings.Repeat("f", 64)
			o.SetAnnotations(a)
		}},
		{name: "owner", change: func(o *unstructured.Unstructured) {
			o.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Namespace", "name": "foreign", "uid": "foreign"}}
		}},
		{name: "unknown finalizer", change: func(o *unstructured.Unstructured) { o.SetFinalizers([]string{"untrusted.example/finalizer"}) }},
		{name: "duplicate finalizer", change: func(o *unstructured.Unstructured) {
			o.SetFinalizers([]string{"dpu.nvidia.com/dpudeployment", "dpu.nvidia.com/dpudeployment"})
		}},
		{name: "cross kind finalizer", change: func(o *unstructured.Unstructured) { o.SetFinalizers([]string{"dpu.nvidia.com/dpfoperatorconfig"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := flavorReconcileDesired(fullFlavorSpec())
			if tc.prepare != nil {
				tc.prepare(desired)
			}
			current, err := r.markedDesired(desired)
			if err != nil {
				t.Fatalf("invalid negative fixture: %v", err)
			}
			current.SetUID("existing-flavor")
			current.SetResourceVersion("4")
			tc.change(current)
			beforeDesired, beforeCurrent := desired.DeepCopy(), current.DeepCopy()
			client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{reconcileKey(desired): current.DeepCopy()}}
			_, disposition, err := r.Ensure(context.Background(), client, desired)
			if err == nil || disposition != DispositionExisting || client.creates != 0 {
				t.Fatalf("conflict accepted/adopted: disposition=%s creates=%d err=%v", disposition, client.creates, err)
			}
			if !reflect.DeepEqual(beforeDesired.Object, desired.Object) || !reflect.DeepEqual(beforeCurrent.Object, current.Object) || !reflect.DeepEqual(beforeCurrent.Object, client.objects[reconcileKey(desired)].Object) {
				t.Fatal("failed comparison changed input")
			}
		})
	}
}

func TestDPUFlavorAdmissionDefaultsAreMissingOnlyAndExactScope(t *testing.T) {
	for _, value := range []any{nil, false, int64(0), []any{}, map[string]any{"present": false}} {
		spec := map[string]any{}
		for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig"} {
			spec[field] = value
		}
		object := flavorReconcileDesired(spec)
		before := object.DeepCopy()
		defaultDPUFlavorAdmissionObjects(object)
		if !reflect.DeepEqual(before.Object, object.Object) {
			t.Fatal("canonicalizer overwrote an explicitly present value")
		}
	}
	for _, change := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) { o.SetKind("DPUFlavorTemplate") },
		func(o *unstructured.Unstructured) { o.SetAPIVersion("provisioning.dpu.nvidia.com/v1alpha2") },
		func(o *unstructured.Unstructured) { delete(o.Object, "spec") },
		func(o *unstructured.Unstructured) { o.Object["spec"] = map[string]any(nil) },
	} {
		object := flavorReconcileDesired(map[string]any{})
		change(object)
		before := object.DeepCopy()
		defaultDPUFlavorAdmissionObjects(object)
		if !reflect.DeepEqual(before.Object, object.Object) {
			t.Fatal("canonicalizer expanded beyond present exact flavor spec")
		}
	}
	defaultDPUFlavorAdmissionObjects(nil)
}

func TestPostCreateConflictMessageKeepsOnlyFiniteVerificationCategory(t *testing.T) {
	var typedNil *ReconciliationError
	for _, tc := range []struct {
		name     string
		err      error
		category string
	}{
		{"known content", &ReconciliationError{ReasonCode: "RESOURCE_CONTENT_CONFLICT", Message: "private-message-canary", cause: errors.New("private-cause-canary")}, "RESOURCE_CONTENT_CONFLICT"},
		{"wrapped metadata", fmt.Errorf("private-wrapper-canary: %w", &ReconciliationError{ReasonCode: "RESOURCE_METADATA_CONFLICT", Message: "private-message-canary"}), "RESOURCE_METADATA_CONFLICT"},
		{"unknown code", &ReconciliationError{ReasonCode: "private-code-canary", Message: "private-message-canary"}, ""},
		{"code suffix", &ReconciliationError{ReasonCode: "RESOURCE_CONTENT_CONFLICT_private-code-canary"}, ""},
		{"non verification category", &ReconciliationError{ReasonCode: "RESOURCE_CREATE_UNCERTAIN"}, ""},
		{"arbitrary error", errors.New("private-error-canary"), ""},
		{"typed nil", typedNil, ""}, {"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := postCreateConflictMessage(tc.err)
			if strings.Contains(message, "private-") || !strings.Contains(message, "re-read before retry") {
				t.Fatal("unsafe or incomplete post-create message")
			}
			if tc.category != "" && !strings.Contains(message, "("+tc.category+")") {
				t.Fatal("known verifier category omitted")
			}
			if tc.category == "" && strings.Contains(message, "(") {
				t.Fatal("unknown verifier category exposed")
			}
		})
	}
}

func TestReconcilerPostCreateConflictReportsSafeCategoryAndKeepsAmbiguity(t *testing.T) {
	r, _ := newReconcileFixture(t)
	for _, tc := range []struct {
		name, category string
		change         func(*unstructured.Unstructured)
	}{
		{"content", "RESOURCE_CONTENT_CONFLICT", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "private-content-canary", "spec", "ovs", "rawConfigScript")
		}},
		{"metadata", "RESOURCE_METADATA_CONFLICT", func(o *unstructured.Unstructured) {
			a := o.GetAnnotations()
			a["untrusted.example/extra"] = "private-annotation-canary"
			o.SetAnnotations(a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &reconcileFixture{objects: map[string]*unstructured.Unstructured{}}
			client.create = func(marked *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				marked.SetUID("created-flavor")
				marked.SetResourceVersion("1")
				tc.change(marked)
				return marked, nil
			}
			desired := flavorReconcileDesired(fullFlavorSpec())
			created, disposition, err := r.Ensure(context.Background(), client, desired)
			if err == nil || created == nil || disposition != DispositionCreated || client.creates != 1 {
				t.Fatalf("post-create ambiguity changed: disposition=%s err=%v", disposition, err)
			}
			var outer *ReconciliationError
			if !errors.As(err, &outer) || outer.ReasonCode != "RESOURCE_POST_CREATE_CONFLICT" || !strings.Contains(outer.Message, "("+tc.category+")") || strings.Contains(err.Error(), "private-") {
				t.Fatal("post-create category missing or private data exposed")
			}
			cause := errors.Unwrap(err)
			var verification *ReconciliationError
			if cause == nil || !errors.As(cause, &verification) || verification.ReasonCode != tc.category || !errors.Is(err, cause) {
				t.Fatal("post-create verification cause lost")
			}
		})
	}
}
