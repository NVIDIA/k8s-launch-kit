// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectTenantStorageHealthyAndReadOnly(t *testing.T) {
	collector, kube, dynamic := tenantStorageCollector()
	result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
	if result.State != "passed" || result.ReasonCode != "TENANT_STORAGE_READY" {
		t.Fatalf("CollectTenantStorage() = %+v, want passed", result)
	}
	assertTenantStorageReadOnly(t, kube.Actions())
	assertTenantStorageReadOnly(t, dynamic.Actions())
}

func TestCollectTenantStorageRejectsUnreadyAndMismatchedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *tenantStorageFixture)
		state  string
		reason string
	}{
		{name: "stale datastore", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.dataStore.Object["status"].(map[string]interface{})["observedGeneration"] = int64(0)
		}, state: "unknown", reason: "TENANT_DATASTORE_STATUS_STALE"},
		{name: "malformed datastore condition", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.dataStore.Object["status"].(map[string]interface{})["conditions"] = []interface{}{"not-a-condition"}
		}, state: "failed", reason: "TENANT_DATASTORE_CONDITION_INVALID"},
		{name: "contradictory duplicate datastore conditions", mutate: func(t *testing.T, f *tenantStorageFixture) {
			conditions := f.dataStore.Object["status"].(map[string]interface{})["conditions"].([]interface{})
			f.dataStore.Object["status"].(map[string]interface{})["conditions"] = append(conditions, map[string]interface{}{"type": "kamaji.clastix.io/DataStoreValidation", "status": "False", "observedGeneration": int64(1)})
		}, state: "failed", reason: "TENANT_DATASTORE_VALIDATION_FAILED"},
		{name: "zero etcd replicas", mutate: func(t *testing.T, f *tenantStorageFixture) { f.statefulSet.Spec.Replicas = ptr[int32](0) }, state: "failed", reason: "TENANT_ETCD_REPLICAS_INVALID"},
		{name: "external datastore endpoint", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.dataStore.Object["spec"].(map[string]interface{})["endpoints"] = []interface{}{"etcd.attacker.test:2379"}
		}, state: "failed", reason: "TENANT_DATASTORE_ENDPOINTS_MISMATCH"},
		{name: "member endpoint with unsupported cluster domain", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.dataStore.Object["spec"].(map[string]interface{})["endpoints"] = []interface{}{"kamaji-etcd-0.kamaji-etcd.dpf-operator-system.svc.attacker.test:2379", "kamaji-etcd-1.kamaji-etcd.dpf-operator-system.svc.attacker.test:2379"}
		}, state: "failed", reason: "TENANT_DATASTORE_ENDPOINTS_MISMATCH"},
		{name: "wrong service target port", mutate: func(t *testing.T, f *tenantStorageFixture) { f.service.Spec.Ports[0].TargetPort = intstrFromInt(1234) }, state: "failed", reason: "TENANT_ETCD_SERVICE_PORT_MISMATCH"},
		{name: "wrong pod owner uid", mutate: func(t *testing.T, f *tenantStorageFixture) { f.pods[0].OwnerReferences[0].UID = types.UID("other") }, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "pod references another claim", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "foreign-claim"
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "data claim is not mounted", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].VolumeMounts = nil
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "data directory override", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.statefulSet.Spec.Template.Spec.Containers[0].Command = append(f.statefulSet.Spec.Template.Spec.Containers[0].Command, "--data-dir=/tmp/etcd")
		}, state: "failed", reason: "TENANT_ETCD_DATA_PATH_INVALID"},
		{name: "read-only PVC source", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Volumes[0].PersistentVolumeClaim.ReadOnly = true
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "nested mount diverts etcd data", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].VolumeMounts = append(f.pods[0].Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/var/run/etcd/member"})
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "parent mount competes with data mount", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].VolumeMounts = append(f.pods[0].Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/var/run"})
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "single dash data-dir override", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].Args = []string{"-data-dir=/tmp/etcd"}
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "config file override", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].Args = []string{"--config-file=/etc/other.yaml"}
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "config environment override", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ETCD_CONFIG_FILE", Value: "/etc/other.yaml"}}
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "unobserved config environment source", mutate: func(t *testing.T, f *tenantStorageFixture) {
			f.pods[0].Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "other"}}}}
		}, state: "unknown", reason: "TENANT_ETCD_POD_NOT_READY"},
		{name: "claimref uid mismatch", mutate: func(t *testing.T, f *tenantStorageFixture) { f.volumes[0].Spec.ClaimRef.UID = types.UID("other") }, state: "failed", reason: "TENANT_ETCD_VOLUME_BINDING_MISMATCH"},
		{name: "provisioner stale generation", mutate: func(t *testing.T, f *tenantStorageFixture) { f.provisioner.Status.ObservedGeneration = 0 }, state: "unknown", reason: "TENANT_PROVISIONER_NOT_READY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTenantStorageFixture()
			tt.mutate(t, fixture)
			collector, kube, dynamic := tenantStorageCollectorFromFixture(fixture)
			result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
			if result.State != tt.state || result.ReasonCode != tt.reason {
				t.Fatalf("CollectTenantStorage() = %+v, want state=%s reason=%s", result, tt.state, tt.reason)
			}
			assertTenantStorageReadOnly(t, kube.Actions())
			assertTenantStorageReadOnly(t, dynamic.Actions())
		})
	}
}

func TestCollectTenantStorageAcceptsExpandedBoundClaim(t *testing.T) {
	fixture := newTenantStorageFixture()
	for index := range fixture.claims {
		fixture.claims[index].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
		fixture.claims[index].Status.Capacity[corev1.ResourceStorage] = resource.MustParse("10Gi")
		fixture.volumes[index].Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("10Gi")
	}
	collector, _, _ := tenantStorageCollectorFromFixture(fixture)
	result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
	if result.State != "passed" {
		t.Fatalf("CollectTenantStorage() = %+v, want expanded existing claims to pass", result)
	}
}

func TestCollectTenantStorageMissingClaimAndContextCancellationAreNotPassed(t *testing.T) {
	t.Run("missing existing etcd claim", func(t *testing.T) {
		fixture := newTenantStorageFixture()
		fixture.claims = fixture.claims[1:]
		collector, _, _ := tenantStorageCollectorFromFixture(fixture)
		result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
		if result.State != "failed" || result.ReasonCode != "TENANT_STORAGE_RESOURCE_MISSING" || !strings.Contains(result.Message, "data-kamaji-etcd-0") {
			t.Fatalf("CollectTenantStorage() = %+v, want actionable missing-claim failure", result)
		}
	})
	t.Run("canceled during final read", func(t *testing.T) {
		collector, _, dynamic := tenantStorageCollector()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		gets := 0
		dynamic.PrependReactor("get", "datastores", func(action ktesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == 2 {
				cancel()
			}
			return false, nil, nil
		})
		result := collector.CollectTenantStorage(ctx, "dpf-operator-system")
		if result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_CANCELED" {
			t.Fatalf("CollectTenantStorage() = %+v, want canceled unknown", result)
		}
	})
}

func TestCollectTenantStorageUnavailableAndCanceledReadsStayUnknown(t *testing.T) {
	t.Run("forbidden datastore", func(t *testing.T) {
		collector, _, dynamic := tenantStorageCollector()
		dynamic.PrependReactor("get", "datastores", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: tenantDataStoreGVR.Group, Resource: tenantDataStoreGVR.Resource}, tenantDataStoreName, errors.New("denied"))
		})
		result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
		if result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_ACCESS_DENIED" {
			t.Fatalf("CollectTenantStorage() = %+v, want access-denied unknown", result)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		collector, _, _ := tenantStorageCollector()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := collector.CollectTenantStorage(ctx, "dpf-operator-system")
		if result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_CANCELED" {
			t.Fatalf("CollectTenantStorage() = %+v, want canceled unknown", result)
		}
	})
	t.Run("missing client", func(t *testing.T) {
		result := (Collector{}).CollectTenantStorage(context.Background(), "dpf-operator-system")
		if result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_CLIENT_UNAVAILABLE" {
			t.Fatalf("CollectTenantStorage() = %+v, want unavailable unknown", result)
		}
	})
}

func TestCollectTenantStorageDetectsReplacementDuringFinalRead(t *testing.T) {
	collector, _, dynamic := tenantStorageCollector()
	gets := 0
	dynamic.PrependReactor("get", "datastores", func(action ktesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 2 {
			obj, err := dynamic.Tracker().Get(tenantDataStoreGVR, "", tenantDataStoreName)
			if err != nil {
				return true, nil, err
			}
			changed := obj.(*unstructured.Unstructured).DeepCopy()
			changed.SetUID(types.UID("replacement"))
			return true, changed, nil
		}
		return false, nil, nil
	})
	result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system")
	if result.State != "unknown" || result.ReasonCode != "TENANT_STORAGE_CHANGED_DURING_CHECK" {
		t.Fatalf("CollectTenantStorage() = %+v, want changed-during-check unknown", result)
	}
}

type tenantStorageFixture struct {
	dataStore   *unstructured.Unstructured
	statefulSet *appsv1.StatefulSet
	service     *corev1.Service
	claims      []*corev1.PersistentVolumeClaim
	volumes     []*corev1.PersistentVolume
	pods        []*corev1.Pod
	class       *storagev1.StorageClass
	provisioner *appsv1.Deployment
}

func newTenantStorageFixture() *tenantStorageFixture {
	const namespace = "dpf-operator-system"
	const uid = "etcd-sts-uid"
	replicas := int32(2)
	trueValue := true
	dataStore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kamaji.clastix.io/v1alpha1", "kind": "DataStore",
		"metadata": map[string]interface{}{"name": tenantDataStoreName, "uid": "datastore-uid", "generation": int64(1)},
		"spec": map[string]interface{}{"driver": "etcd", "endpoints": []interface{}{
			"kamaji-etcd-0.kamaji-etcd.dpf-operator-system.svc.cluster.local:2379",
			"kamaji-etcd-1.kamaji-etcd.dpf-operator-system.svc.cluster.local:2379",
		}},
		"status": map[string]interface{}{"ready": true, "observedGeneration": int64(1), "conditions": []interface{}{map[string]interface{}{"type": "kamaji.clastix.io/DataStoreValidation", "status": "True", "observedGeneration": int64(1)}}},
	}}
	dataStore.SetGroupVersionKind(tenantDataStoreGVR.GroupVersion().WithKind("DataStore"))
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": tenantEtcdLabelName, "app.kubernetes.io/instance": tenantEtcdLabelInst}}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: tenantEtcdName, Namespace: namespace, UID: types.UID(uid), Generation: 1}, Spec: appsv1.StatefulSetSpec{
		Replicas: &replicas, ServiceName: tenantEtcdName, Selector: selector,
		Template:             corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "etcd", Command: []string{"etcd", "--data-dir=/var/run/etcd"}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/run/etcd"}}}}}},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr(tenantStorageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}}},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 2, ReadyReplicas: 2, CurrentReplicas: 2, UpdatedReplicas: 2, CurrentRevision: "revision-a", UpdateRevision: "revision-a"}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tenantEtcdName, Namespace: namespace, UID: types.UID("service-uid")}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{"app.kubernetes.io/name": tenantEtcdLabelName, "app.kubernetes.io/instance": tenantEtcdLabelInst}, Ports: []corev1.ServicePort{{Port: tenantEtcdPort, TargetPort: intstrFromInt(tenantEtcdPort), Protocol: corev1.ProtocolTCP}}}}
	claims := make([]*corev1.PersistentVolumeClaim, 0, replicas)
	volumes := make([]*corev1.PersistentVolume, 0, replicas)
	pods := make([]*corev1.Pod, 0, replicas)
	for i := int32(0); i < replicas; i++ {
		claimName := fmt.Sprintf("data-%s-%d", tenantEtcdName, i)
		claimUID := types.UID(fmt.Sprintf("claim-uid-%d", i))
		volumeName := fmt.Sprintf("pv-%d", i)
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace, UID: claimUID}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr(tenantStorageClass), VolumeName: volumeName, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}
		volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volumeName, UID: types.UID(fmt.Sprintf("pv-uid-%d", i))}, Spec: corev1.PersistentVolumeSpec{StorageClassName: tenantStorageClass, ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: claimName, Namespace: namespace, UID: claimUID}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound}}
		controller := trueValue
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", tenantEtcdName, i), Namespace: namespace, UID: types.UID(fmt.Sprintf("pod-uid-%d", i)), Labels: map[string]string{"app.kubernetes.io/name": tenantEtcdLabelName, "app.kubernetes.io/instance": tenantEtcdLabelInst, "controller-revision-hash": "revision-a"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: tenantEtcdName, UID: types.UID(uid), Controller: &controller}}}, Spec: corev1.PodSpec{Hostname: fmt.Sprintf("%s-%d", tenantEtcdName, i), Subdomain: tenantEtcdName, NodeName: fmt.Sprintf("node-%d", i), Containers: []corev1.Container{{Name: "etcd", Command: []string{"etcd", "--data-dir=/var/run/etcd"}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/run/etcd"}}}}, Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "etcd", Ready: true}}}}
		claims = append(claims, claim)
		volumes = append(volumes, volume)
		pods = append(pods, pod)
	}
	binding := storagev1.VolumeBindingWaitForFirstConsumer
	class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: tenantStorageClass, UID: types.UID("storage-class-uid")}, Provisioner: tenantProvisioner, VolumeBindingMode: &binding}
	provisionerReplicas := int32(1)
	provisioner := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: tenantProvisionerID, Namespace: tenantProvisionerNS, UID: types.UID("provisioner-uid"), Generation: 3}, Spec: appsv1.DeploymentSpec{Replicas: &provisionerReplicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": tenantProvisionerID, "app.kubernetes.io/instance": tenantProvisionerID}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1}}
	return &tenantStorageFixture{dataStore: dataStore, statefulSet: statefulSet, service: service, claims: claims, volumes: volumes, pods: pods, class: class, provisioner: provisioner}
}

func tenantStorageCollector() (Collector, *kubernetesfake.Clientset, *dynamicfake.FakeDynamicClient) {
	return tenantStorageCollectorFromFixture(newTenantStorageFixture())
}

func tenantStorageCollectorFromFixture(f *tenantStorageFixture) (Collector, *kubernetesfake.Clientset, *dynamicfake.FakeDynamicClient) {
	objects := []runtime.Object{f.statefulSet, f.service, f.class, f.provisioner}
	for _, object := range f.claims {
		objects = append(objects, object)
	}
	for _, object := range f.volumes {
		objects = append(objects, object)
	}
	for _, object := range f.pods {
		objects = append(objects, object)
	}
	kube := kubernetesfake.NewSimpleClientset(objects...)
	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), f.dataStore)
	return Collector{Kubernetes: kube, Dynamic: dynamic}, kube, dynamic
}

func assertTenantStorageReadOnly(t *testing.T, actions []ktesting.Action) {
	t.Helper()
	for _, action := range actions {
		verb := action.GetVerb()
		if verb == "create" || verb == "update" || verb == "patch" || verb == "delete" || verb == "delete-collection" {
			t.Errorf("storage observation performed prohibited %s on %s", verb, action.GetResource().Resource)
		}
		if action.GetResource().Resource == "secrets" {
			t.Errorf("storage observation accessed Secrets with %s", verb)
		}
	}
}

func ptr[T any](value T) *T { return &value }

func intstrFromInt(value int) intstr.IntOrString { return intstr.FromInt32(int32(value)) }

func TestCollectTenantStorageIgnoresHealthyConditionMetadataChurn(t *testing.T) {
	collector, kube, _ := tenantStorageCollector()
	gets := map[string]int{}
	kube.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := action.(ktesting.GetAction).GetName()
		gets[name]++
		obj, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace(), name)
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if gets[name] > 1 {
			pod.Status.Conditions[0].Message = "Updated health observation"
			pod.Status.Conditions[0].LastProbeTime = metav1.Now()
		}
		return true, pod, nil
	})
	if result := collector.CollectTenantStorage(context.Background(), "dpf-operator-system"); result.State != "passed" {
		t.Fatalf("healthy metadata churn invalidated storage: %#v", result)
	}
}
