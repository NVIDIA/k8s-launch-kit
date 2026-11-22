// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
)

// acceptanceTenantStorageObjects reuses the concrete healthy storage topology
// from managementprobe/newTenantStorageFixture, observed through the real collector.
func acceptanceTenantStorageObjects(t *testing.T, namespace string) []unstructured.Unstructured {
	t.Helper()
	const (
		tenantDataStoreName = "default"
		tenantEtcdName      = "kamaji-etcd"
		tenantEtcdLabelName = "kamaji-etcd"
		tenantEtcdLabelInst = "kamaji"
		tenantStorageClass  = "local-path"
		tenantProvisionerNS = "local-path-provisioner"
		tenantProvisionerID = "local-path-provisioner"
		tenantProvisioner   = "cluster.local/local-path-provisioner"
		tenantEtcdPort      = 2379
	)
	const uid = "etcd-sts-uid"
	replicas := int32(2)
	trueValue := true
	dataStore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kamaji.clastix.io/v1alpha1", "kind": "DataStore",
		"metadata": map[string]interface{}{"name": tenantDataStoreName, "uid": "datastore-uid", "generation": int64(1)},
		"spec": map[string]interface{}{"driver": "etcd", "endpoints": []interface{}{
			fmt.Sprintf("kamaji-etcd-0.kamaji-etcd.%s.svc.cluster.local:2379", namespace),
			fmt.Sprintf("kamaji-etcd-1.kamaji-etcd.%s.svc.cluster.local:2379", namespace),
		}},
		"status": map[string]interface{}{"ready": true, "observedGeneration": int64(1), "conditions": []interface{}{map[string]interface{}{"type": "kamaji.clastix.io/DataStoreValidation", "status": "True", "observedGeneration": int64(1)}}},
	}}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": tenantEtcdLabelName, "app.kubernetes.io/instance": tenantEtcdLabelInst}}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: tenantEtcdName, Namespace: namespace, UID: types.UID(uid), Generation: 1}, Spec: appsv1.StatefulSetSpec{
		Replicas: &replicas, ServiceName: tenantEtcdName, Selector: selector,
		Template:             corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "etcd", Command: []string{"etcd", "--data-dir=/var/run/etcd"}, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/run/etcd"}}}}}},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(tenantStorageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}}},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 2, ReadyReplicas: 2, CurrentReplicas: 2, UpdatedReplicas: 2, CurrentRevision: "revision-a", UpdateRevision: "revision-a"}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: tenantEtcdName, Namespace: namespace, UID: types.UID("service-uid")}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{"app.kubernetes.io/name": tenantEtcdLabelName, "app.kubernetes.io/instance": tenantEtcdLabelInst}, Ports: []corev1.ServicePort{{Port: tenantEtcdPort, TargetPort: intstr.FromInt32(tenantEtcdPort), Protocol: corev1.ProtocolTCP}}}}
	claims := make([]*corev1.PersistentVolumeClaim, 0, replicas)
	volumes := make([]*corev1.PersistentVolume, 0, replicas)
	pods := make([]*corev1.Pod, 0, replicas)
	for i := int32(0); i < replicas; i++ {
		claimName := fmt.Sprintf("data-%s-%d", tenantEtcdName, i)
		claimUID := types.UID(fmt.Sprintf("claim-uid-%d", i))
		volumeName := fmt.Sprintf("pv-%d", i)
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace, UID: claimUID}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(tenantStorageClass), VolumeName: volumeName, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}}
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
	provisioner := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: tenantProvisionerID, Namespace: tenantProvisionerNS, Labels: map[string]string{"app.kubernetes.io/name": tenantProvisionerID}, UID: types.UID("provisioner-uid"), Generation: 3}, Spec: appsv1.DeploymentSpec{Replicas: &provisionerReplicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": tenantProvisionerID}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": tenantProvisionerID}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: tenantProvisionerID, Image: "docker.io/rancher/local-path-provisioner:v0.0.34"}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1}}
	objects := []struct {
		value            any
		apiVersion, kind string
	}{
		{dataStore, "kamaji.clastix.io/v1alpha1", "DataStore"}, {statefulSet, "apps/v1", "StatefulSet"}, {service, "v1", "Service"}, {class, "storage.k8s.io/v1", "StorageClass"}, {provisioner, "apps/v1", "Deployment"},
	}
	for _, value := range claims {
		objects = append(objects, struct {
			value            any
			apiVersion, kind string
		}{value, "v1", "PersistentVolumeClaim"})
	}
	for _, value := range volumes {
		objects = append(objects, struct {
			value            any
			apiVersion, kind string
		}{value, "v1", "PersistentVolume"})
	}
	for _, value := range pods {
		objects = append(objects, struct {
			value            any
			apiVersion, kind string
		}{value, "v1", "Pod"})
	}
	result := make([]unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object.value)
		if err != nil {
			t.Fatal(err)
		}
		value := unstructured.Unstructured{Object: data}
		value.SetAPIVersion(object.apiVersion)
		value.SetKind(object.kind)
		value.SetResourceVersion("1")
		result = append(result, value)
	}
	return result
}

func TestRunAcceptanceStorageFixtureUsesActualCollector(t *testing.T) {
	const namespace = "dpf-operator-system"
	objects := acceptanceTenantStorageObjects(t, namespace)
	var kubeObjects []runtime.Object
	var datastore *unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() == "DataStore" {
			datastore = object.DeepCopy()
			continue
		}
		typed, err := scheme.Scheme.New(schema.FromAPIVersionAndKind(object.GetAPIVersion(), object.GetKind()))
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, typed); err != nil {
			t.Fatal(err)
		}
		kubeObjects = append(kubeObjects, typed)
	}
	collector := managementprobe.Collector{Kubernetes: kubernetesfake.NewSimpleClientset(kubeObjects...), Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), datastore)}
	if result := collector.CollectTenantStorage(context.Background(), namespace); result.State != "passed" {
		t.Fatalf("actual storage fixture: %+v", result)
	}
}
