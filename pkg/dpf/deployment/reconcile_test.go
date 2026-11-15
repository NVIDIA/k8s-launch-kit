// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
			r, desired := newReconcileFixture(t)
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
	current.SetFinalizers([]string{"provisioning.dpu.nvidia.com/cluster-manager-clean-up"})
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
