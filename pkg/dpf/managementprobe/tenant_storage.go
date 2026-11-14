// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	tenantDataStoreName = "default"
	tenantEtcdName      = "kamaji-etcd"
	tenantEtcdPort      = 2379
	tenantEtcdLabelName = "kamaji-etcd"
	tenantEtcdLabelInst = "kamaji"
	tenantStorageClass  = "local-path"
	tenantProvisioner   = "cluster.local/local-path-provisioner"
	tenantProvisionerNS = "local-path-provisioner"
	tenantProvisionerID = "local-path-provisioner"
	maxEtcdReplicas     = int32(16)
	maxEtcdClaims       = 16
)

var tenantDataStoreGVR = schema.GroupVersionResource{Group: "kamaji.clastix.io", Version: "v1alpha1", Resource: "datastores"}

// TenantStorageResult is a compact, safe summary of whether the shared
// Kamaji datastore and its current Kubernetes storage path are healthy.
type TenantStorageResult struct {
	State      string `json:"state" yaml:"state"`
	ReasonCode string `json:"reasonCode" yaml:"reasonCode"`
	Message    string `json:"message" yaml:"message"`
}

type storageRead struct {
	signature []string
}

// CollectTenantStorage observes the bounded shared Kamaji storage path. It
// does not read Secrets, create probe claims, or claim free-disk capacity.
func (c Collector) CollectTenantStorage(ctx context.Context, namespace string) TenantStorageResult {
	if ctx == nil || ctx.Err() != nil {
		return storageUnknown("TENANT_STORAGE_CANCELED", "Storage observation was canceled before it started.")
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 {
		return storageUnknown("TENANT_STORAGE_INVALID_NAMESPACE", "A valid Kamaji namespace is required.")
	}
	if c.Kubernetes == nil || c.Dynamic == nil {
		return storageUnknown("TENANT_STORAGE_CLIENT_UNAVAILABLE", "Required management API clients are unavailable.")
	}

	first, result := c.readTenantStorage(ctx, namespace)
	if result.State != "passed" {
		return result
	}
	second, result := c.readTenantStorage(ctx, namespace)
	if result.State != "passed" {
		return result
	}
	if strings.Join(first.signature, "\x00") != strings.Join(second.signature, "\x00") {
		return storageUnknown("TENANT_STORAGE_CHANGED_DURING_CHECK", "Storage identities or desired configuration changed during observation.")
	}
	if err := ctx.Err(); err != nil {
		return storageUnknown("TENANT_STORAGE_CANCELED", "The shared Kamaji storage observation was canceled or timed out.")
	}
	return storagePassed()
}

func (c Collector) readTenantStorage(ctx context.Context, namespace string) (storageRead, TenantStorageResult) {
	var result storageRead
	datastore, err := c.Dynamic.Resource(tenantDataStoreGVR).Get(ctx, tenantDataStoreName, metav1.GetOptions{})
	if err != nil {
		return result, storageAPIError("datastore", err)
	}
	if check := validateDataStore(datastore); check.State != "passed" {
		return result, check
	}
	appendStorageSignature(&result, datastoreSafeProjection(datastore))

	statefulSet, err := c.Kubernetes.AppsV1().StatefulSets(namespace).Get(ctx, tenantEtcdName, metav1.GetOptions{})
	if err != nil {
		return result, storageAPIError("etcd stateful set", err)
	}
	appendStorageSignature(&result, statefulSetProjection(statefulSet))
	if statefulSet.Namespace != namespace {
		return result, storageFailed("TENANT_ETCD_IDENTITY_INVALID", "The Kamaji etcd StatefulSet is outside the selected namespace.")
	}
	if check := validateEtcdStatefulSet(statefulSet); check.State != "passed" {
		return result, check
	}
	if check := validateDataStoreEndpoints(datastore, statefulSet, namespace); check.State != "passed" {
		return result, check
	}

	service, err := c.Kubernetes.CoreV1().Services(namespace).Get(ctx, statefulSet.Spec.ServiceName, metav1.GetOptions{})
	if err != nil {
		return result, storageAPIError("etcd service", err)
	}
	appendStorageSignature(&result, serviceProjection(service))
	if check := validateEtcdService(service, statefulSet); check.State != "passed" {
		return result, check
	}

	claims, volumes, pods, check := c.readEtcdResources(ctx, namespace, statefulSet, service)
	if check.State != "passed" {
		return result, check
	}
	for _, claim := range claims {
		appendStorageSignature(&result, claimProjection(claim))
	}
	for _, volume := range volumes {
		appendStorageSignature(&result, volumeProjection(volume))
	}
	for _, pod := range pods {
		appendStorageSignature(&result, podProjection(pod))
	}

	class, err := c.Kubernetes.StorageV1().StorageClasses().Get(ctx, tenantStorageClass, metav1.GetOptions{})
	if err != nil {
		return result, storageAPIError("storage class", err)
	}
	appendStorageSignature(&result, classProjection(class))
	if check := validateTenantStorageClass(class); check.State != "passed" {
		return result, check
	}

	provisioner, err := c.Kubernetes.AppsV1().Deployments(tenantProvisionerNS).Get(ctx, tenantProvisionerID, metav1.GetOptions{})
	if err != nil {
		return result, storageAPIError("local path provisioner", err)
	}
	appendStorageSignature(&result, deploymentProjection(provisioner))
	if check := validateProvisionerDeployment(provisioner); check.State != "passed" {
		return result, check
	}
	return result, storagePassed()
}

func validateDataStore(obj *unstructured.Unstructured) TenantStorageResult {
	if obj == nil || obj.GetName() != tenantDataStoreName || obj.GetUID() == "" || obj.GetGeneration() < 1 || obj.GetDeletionTimestamp() != nil {
		return storageFailed("TENANT_DATASTORE_IDENTITY_INVALID", "The shared Kamaji DataStore identity is missing, changing, or deleting.")
	}
	driver, found, err := unstructured.NestedString(obj.Object, "spec", "driver")
	if err != nil || !found || driver != "etcd" {
		return storageFailed("TENANT_DATASTORE_DRIVER_INVALID", "The shared Kamaji DataStore is not configured for the supported etcd backend.")
	}
	endpoints, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) == 0 {
		return storageFailed("TENANT_DATASTORE_ENDPOINTS_INVALID", "The shared Kamaji DataStore has no valid endpoint set.")
	}
	observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if err != nil || !found || observed != obj.GetGeneration() {
		return storageUnknown("TENANT_DATASTORE_STATUS_STALE", "The shared Kamaji DataStore status is not current.")
	}
	ready, found, err := unstructured.NestedBool(obj.Object, "status", "ready")
	if err != nil || !found || !ready {
		return storageFailed("TENANT_DATASTORE_NOT_READY", "The shared Kamaji DataStore is not ready.")
	}
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return storageUnknown("TENANT_DATASTORE_VALIDATION_UNKNOWN", "The shared Kamaji DataStore has no current validation condition.")
	}
	count := 0
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			return storageFailed("TENANT_DATASTORE_CONDITION_INVALID", "The shared Kamaji DataStore validation status is malformed.")
		}
		typeName, _, _ := unstructured.NestedString(condition, "type")
		if typeName != "kamaji.clastix.io/DataStoreValidation" {
			continue
		}
		count++
		status, statusFound, statusErr := unstructured.NestedString(condition, "status")
		conditionGeneration, generationFound, generationErr := unstructured.NestedInt64(condition, "observedGeneration")
		if statusErr != nil || generationErr != nil || !statusFound || !generationFound || conditionGeneration != obj.GetGeneration() {
			return storageUnknown("TENANT_DATASTORE_VALIDATION_STALE", "The shared Kamaji DataStore validation condition is not current.")
		}
		if status != "True" {
			return storageFailed("TENANT_DATASTORE_VALIDATION_FAILED", "Kamaji has not accepted the shared DataStore configuration.")
		}
	}
	if count != 1 {
		return storageFailed("TENANT_DATASTORE_VALIDATION_AMBIGUOUS", "The shared Kamaji DataStore must have exactly one validation condition.")
	}
	return storagePassed()
}

func validateEtcdStatefulSet(set *appsv1.StatefulSet) TenantStorageResult {
	if set == nil || set.Name != tenantEtcdName || set.Namespace == "" || set.UID == "" || set.Generation < 1 || set.DeletionTimestamp != nil {
		return storageFailed("TENANT_ETCD_IDENTITY_INVALID", "The Kamaji etcd StatefulSet identity is missing, changing, or deleting.")
	}
	if set.Spec.ServiceName != tenantEtcdName || set.Spec.Selector == nil || set.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != tenantEtcdLabelName || set.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != tenantEtcdLabelInst {
		return storageFailed("TENANT_ETCD_SELECTOR_INVALID", "The Kamaji etcd StatefulSet does not match the expected service and workload labels.")
	}
	if set.Spec.Ordinals != nil && set.Spec.Ordinals.Start != 0 {
		return storageFailed("TENANT_ETCD_ORDINALS_UNSUPPORTED", "The Kamaji etcd StatefulSet uses an unsupported member ordinal range.")
	}
	if set.Spec.Replicas == nil || *set.Spec.Replicas < 1 || *set.Spec.Replicas > maxEtcdReplicas {
		return storageFailed("TENANT_ETCD_REPLICAS_INVALID", "The Kamaji etcd StatefulSet replica count is missing or outside the supported observation bound.")
	}
	desired := *set.Spec.Replicas
	if set.Status.ObservedGeneration != set.Generation || set.Status.Replicas != desired || set.Status.ReadyReplicas != desired || set.Status.CurrentReplicas != desired || set.Status.UpdatedReplicas != desired || set.Status.CurrentRevision == "" || set.Status.CurrentRevision != set.Status.UpdateRevision {
		return storageUnknown("TENANT_ETCD_NOT_CURRENT", "The Kamaji etcd StatefulSet is not fully current and ready.")
	}
	if len(set.Spec.VolumeClaimTemplates) == 0 || len(set.Spec.VolumeClaimTemplates)*int(desired) > maxEtcdClaims {
		return storageFailed("TENANT_ETCD_CLAIMS_INVALID", "The Kamaji etcd StatefulSet claim templates are missing or exceed the bounded observation limit.")
	}
	for _, template := range set.Spec.VolumeClaimTemplates {
		if template.Name != "data" || template.Spec.StorageClassName == nil || *template.Spec.StorageClassName != tenantStorageClass || template.Spec.Resources.Requests.Storage().Sign() <= 0 {
			return storageFailed("TENANT_ETCD_CLAIM_TEMPLATE_INVALID", "The Kamaji etcd StatefulSet has an invalid or unsupported persistent volume claim template.")
		}
	}
	if !etcdDataContainer(set.Spec.Template.Spec.Containers, "data") {
		return storageFailed("TENANT_ETCD_DATA_PATH_INVALID", "The Kamaji etcd StatefulSet template does not persist its configured data directory to the expected claim.")
	}
	return storagePassed()
}

func validateDataStoreEndpoints(obj *unstructured.Unstructured, set *appsv1.StatefulSet, namespace string) TenantStorageResult {
	endpoints, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "endpoints")
	if err != nil || !found || int32(len(endpoints)) != *set.Spec.Replicas {
		return storageFailed("TENANT_DATASTORE_ENDPOINTS_MISMATCH", "The DataStore endpoint set does not match the current etcd members.")
	}
	const expectedSuffix = ".svc.cluster.local"
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		host, port, splitErr := net.SplitHostPort(endpoint)
		if splitErr != nil || port != strconv.Itoa(tenantEtcdPort) {
			return storageFailed("TENANT_DATASTORE_ENDPOINTS_MISMATCH", "The DataStore endpoint set does not match the current etcd service.")
		}
		matched := false
		for ordinal := int32(0); ordinal < *set.Spec.Replicas; ordinal++ {
			wantHost := fmt.Sprintf("%s-%d.%s.%s%s", tenantEtcdName, ordinal, set.Spec.ServiceName, namespace, expectedSuffix)
			if host == wantHost {
				if len(validation.IsDNS1123Subdomain(host)) != 0 {
					return storageFailed("TENANT_DATASTORE_ENDPOINTS_MISMATCH", "The DataStore contains a malformed etcd service endpoint.")
				}
				if _, duplicate := seen[host]; duplicate {
					return storageFailed("TENANT_DATASTORE_ENDPOINTS_MISMATCH", "The DataStore contains duplicate etcd member endpoints.")
				}
				seen[host] = struct{}{}
				matched = true
				break
			}
		}
		if !matched {
			return storageFailed("TENANT_DATASTORE_ENDPOINTS_MISMATCH", "A DataStore endpoint does not identify a member of the current etcd StatefulSet.")
		}
	}
	return storagePassed()
}

func validateEtcdService(service *corev1.Service, set *appsv1.StatefulSet) TenantStorageResult {
	if service == nil || service.Name != set.Spec.ServiceName || service.Namespace != set.Namespace || service.UID == "" || service.DeletionTimestamp != nil || service.Spec.ClusterIP != corev1.ClusterIPNone {
		return storageFailed("TENANT_ETCD_SERVICE_INVALID", "The Kamaji etcd member service is missing, changing, or not headless.")
	}
	for key, value := range set.Spec.Selector.MatchLabels {
		if service.Spec.Selector[key] != value {
			return storageFailed("TENANT_ETCD_SERVICE_SELECTOR_MISMATCH", "The Kamaji etcd service does not select the expected StatefulSet members.")
		}
	}
	for _, port := range service.Spec.Ports {
		if port.Port == tenantEtcdPort && port.Protocol == corev1.ProtocolTCP && port.TargetPort.IntVal == tenantEtcdPort && port.TargetPort.StrVal == "" {
			return storagePassed()
		}
	}
	return storageFailed("TENANT_ETCD_SERVICE_PORT_MISMATCH", "The Kamaji etcd member service does not expose the expected client port.")
}

func (c Collector) readEtcdResources(ctx context.Context, namespace string, set *appsv1.StatefulSet, service *corev1.Service) ([]*corev1.PersistentVolumeClaim, []*corev1.PersistentVolume, []*corev1.Pod, TenantStorageResult) {
	claims := make([]*corev1.PersistentVolumeClaim, 0, len(set.Spec.VolumeClaimTemplates)*int(*set.Spec.Replicas))
	volumes := make([]*corev1.PersistentVolume, 0, cap(claims))
	expectedClaims := make(map[string]string, cap(claims))
	for _, template := range set.Spec.VolumeClaimTemplates {
		for ordinal := int32(0); ordinal < *set.Spec.Replicas; ordinal++ {
			name := fmt.Sprintf("%s-%s-%d", template.Name, set.Name, ordinal)
			expectedClaims[name] = template.Name
			claim, err := c.Kubernetes.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, nil, nil, storageAPIError("etcd persistent volume claim "+name, err)
			}
			if claim.UID == "" || claim.DeletionTimestamp != nil || claim.Status.Phase != corev1.ClaimBound || claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != tenantStorageClass || claim.Spec.VolumeName == "" {
				return nil, nil, nil, storageUnknown("TENANT_ETCD_CLAIM_NOT_BOUND", "Kamaji etcd claim "+name+" is missing, changing, or not bound.")
			}
			templateRequest := template.Spec.Resources.Requests.Storage()
			request, hasRequest := claim.Spec.Resources.Requests[corev1.ResourceStorage]
			if !hasRequest || request.Sign() <= 0 || request.Cmp(*templateRequest) < 0 {
				return nil, nil, nil, storageFailed("TENANT_ETCD_CLAIM_REQUEST_MISMATCH", "Kamaji etcd claim "+name+" does not match its StatefulSet request.")
			}
			claimCapacity, hasClaimCapacity := claim.Status.Capacity[corev1.ResourceStorage]
			if !hasClaimCapacity || claimCapacity.Cmp(request) < 0 {
				return nil, nil, nil, storageUnknown("TENANT_ETCD_CLAIM_CAPACITY_UNKNOWN", "Bound Kamaji etcd claim "+name+" has no sufficient reported capacity.")
			}
			volume, err := c.Kubernetes.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
			if err != nil {
				return nil, nil, nil, storageAPIError("etcd persistent volume "+claim.Spec.VolumeName, err)
			}
			if volume.UID == "" || volume.DeletionTimestamp != nil || volume.Status.Phase != corev1.VolumeBound || volume.Spec.StorageClassName != tenantStorageClass || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.UID != claim.UID || volume.Spec.ClaimRef.Name != claim.Name || volume.Spec.ClaimRef.Namespace != namespace {
				return nil, nil, nil, storageFailed("TENANT_ETCD_VOLUME_BINDING_MISMATCH", "Kamaji etcd volume "+volume.Name+" is not bound to expected claim "+name+".")
			}
			volumeCapacity, hasVolumeCapacity := volume.Spec.Capacity[corev1.ResourceStorage]
			if !hasVolumeCapacity || volumeCapacity.Cmp(claimCapacity) < 0 || volumeCapacity.Cmp(request) < 0 {
				return nil, nil, nil, storageFailed("TENANT_ETCD_VOLUME_CAPACITY_MISMATCH", "Kamaji etcd volume "+volume.Name+" capacity does not match its bound claim.")
			}
			claims = append(claims, claim)
			volumes = append(volumes, volume)
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil || selector.Empty() {
		return nil, nil, nil, storageFailed("TENANT_ETCD_SELECTOR_INVALID", "The Kamaji etcd StatefulSet selector is invalid.")
	}
	pods := make([]*corev1.Pod, 0, len(expectedClaims))
	serviceSelector := labels.Set(service.Spec.Selector).AsSelectorPreValidated()
	for ordinal := int32(0); ordinal < *set.Spec.Replicas; ordinal++ {
		pod, err := c.Kubernetes.CoreV1().Pods(namespace).Get(ctx, fmt.Sprintf("%s-%d", set.Name, ordinal), metav1.GetOptions{})
		if err != nil {
			return nil, nil, nil, storageAPIError("etcd member pod "+fmt.Sprintf("%s-%d", set.Name, ordinal), err)
		}
		if !podReadyForEtcd(pod, set, service, selector, serviceSelector, expectedClaims, ordinal) {
			return nil, nil, nil, storageUnknown("TENANT_ETCD_POD_NOT_READY", "Kamaji etcd member pod "+pod.Name+" is not current, ready, or bound to its expected storage.")
		}
		pods = append(pods, pod)
	}
	return claims, volumes, pods, storagePassed()
}

func podReadyForEtcd(pod *corev1.Pod, set *appsv1.StatefulSet, service *corev1.Service, setSelector, serviceSelector labels.Selector, expectedClaims map[string]string, ordinal int32) bool {
	if pod == nil || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" || pod.Spec.Hostname != pod.Name || pod.Spec.Subdomain != service.Name || !serviceSelector.Matches(labels.Set(pod.Labels)) {
		return false
	}
	if !setSelector.Matches(labels.Set(pod.Labels)) || pod.Labels["controller-revision-hash"] != set.Status.UpdateRevision {
		return false
	}
	ownerFound := false
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			if ownerFound || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" || owner.Name != set.Name || owner.UID != set.UID {
				return false
			}
			ownerFound = true
		}
	}
	if !ownerFound {
		return false
	}
	readyCondition := 0
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			readyCondition++
			if condition.Status != corev1.ConditionTrue {
				return false
			}
		}
	}
	if readyCondition != 1 || len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) {
		return false
	}
	for _, container := range pod.Status.ContainerStatuses {
		if !container.Ready {
			return false
		}
	}
	usedClaims := map[string]bool{}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		claimName := volume.PersistentVolumeClaim.ClaimName
		templateName, expected := expectedClaims[claimName]
		if !expected || volume.PersistentVolumeClaim.ReadOnly || expectedClaimForOrdinal(claimName, set.Name, ordinal) == "" || volume.Name != templateName || usedClaims[claimName] {
			return false
		}
		usedClaims[claimName] = true
	}
	for claimName := range expectedClaims {
		if claimName == expectedClaimForOrdinal(claimName, set.Name, ordinal) && !usedClaims[claimName] {
			return false
		}
	}
	return etcdDataContainer(pod.Spec.Containers, "data")
}

func etcdDataContainer(containers []corev1.Container, volumeName string) bool {
	var etcd *corev1.Container
	for index := range containers {
		if containers[index].Name == "etcd" {
			if etcd != nil {
				return false
			}
			etcd = &containers[index]
		}
	}
	if etcd == nil || len(etcd.Command) == 0 || etcd.Command[0] != "etcd" || len(etcd.EnvFrom) != 0 {
		return false
	}
	for _, variable := range etcd.Env {
		if variable.Name == "ETCD_CONFIG_FILE" || variable.Name == "ETCD_DATA_DIR" {
			return false
		}
	}
	// A configuration file can override the command-line data directory. Accept
	// only the pinned explicit flag, without alternate spelling or extra overrides.
	dataDirCount := 0
	arguments := append(append([]string{}, etcd.Command[1:]...), etcd.Args...)
	for _, argument := range arguments {
		if argument == "--" {
			return false
		}
		key := strings.SplitN(strings.TrimLeft(argument, "-"), "=", 2)[0]
		switch key {
		case "config-file":
			return false
		case "data-dir":
			if argument != "--data-dir=/var/run/etcd" {
				return false
			}
			dataDirCount++
		}
	}
	if dataDirCount != 1 {
		return false
	}
	mountCount := 0
	for _, mount := range etcd.VolumeMounts {
		mountPath := path.Clean(mount.MountPath)
		if mountPath != "/var/run/etcd" && (mountPath == "/" || strings.HasPrefix(mountPath, "/var/run/etcd/") || strings.HasPrefix("/var/run/etcd", mountPath+"/")) {
			return false
		}
		if mountPath == "/var/run/etcd" {
			if mount.Name != volumeName || mount.ReadOnly {
				return false
			}
			mountCount++
		}
	}
	return mountCount == 1
}

func expectedClaimForOrdinal(claimName, statefulSet string, ordinal int32) string {
	suffix := fmt.Sprintf("-%s-%d", statefulSet, ordinal)
	if strings.HasSuffix(claimName, suffix) {
		return claimName
	}
	return ""
}

func datastoreSafeProjection(obj *unstructured.Unstructured) any {
	driver, _, _ := unstructured.NestedString(obj.Object, "spec", "driver")
	endpoints, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "endpoints")
	observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	ready, _, _ := unstructured.NestedBool(obj.Object, "status", "ready")
	status := ""
	conditionGeneration := int64(0)
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typeName, _, _ := unstructured.NestedString(condition, "type")
		if typeName == "kamaji.clastix.io/DataStoreValidation" {
			status, _, _ = unstructured.NestedString(condition, "status")
			conditionGeneration, _, _ = unstructured.NestedInt64(condition, "observedGeneration")
		}
	}
	return []any{obj.GetUID(), obj.GetGeneration(), driver, endpoints, observed, ready, status, conditionGeneration}
}

func statefulSetProjection(set *appsv1.StatefulSet) any {
	claims := make([][]string, 0, len(set.Spec.VolumeClaimTemplates))
	for _, claim := range set.Spec.VolumeClaimTemplates {
		class := ""
		if claim.Spec.StorageClassName != nil {
			class = *claim.Spec.StorageClassName
		}
		quantity := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		claims = append(claims, []string{claim.Name, class, quantity.String()})
	}
	var replicas int32
	if set.Spec.Replicas != nil {
		replicas = *set.Spec.Replicas
	}
	return []any{set.UID, set.Generation, set.Spec.ServiceName, set.Spec.Selector, replicas, set.Status.ObservedGeneration, set.Status.Replicas, set.Status.ReadyReplicas, set.Status.CurrentReplicas, set.Status.UpdatedReplicas, set.Status.CurrentRevision, set.Status.UpdateRevision, claims}
}

func serviceProjection(service *corev1.Service) any {
	return []any{service.UID, service.Spec.ClusterIP, service.Spec.Selector, service.Spec.Ports}
}

func claimProjection(claim *corev1.PersistentVolumeClaim) any {
	request := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	capacity := claim.Status.Capacity[corev1.ResourceStorage]
	return []any{claim.UID, claim.Name, claim.Namespace, valueOrEmpty(claim.Spec.StorageClassName), claim.Spec.VolumeName, claim.Status.Phase, request.String(), capacity.String()}
}

func volumeProjection(volume *corev1.PersistentVolume) any {
	capacity := volume.Spec.Capacity[corev1.ResourceStorage]
	var claimUID, claimName, claimNamespace string
	if claim := volume.Spec.ClaimRef; claim != nil {
		claimUID, claimName, claimNamespace = string(claim.UID), claim.Name, claim.Namespace
	}
	return []any{volume.UID, volume.Name, volume.Spec.StorageClassName, volume.Status.Phase, claimUID, claimName, claimNamespace, capacity.String()}
}

func podProjection(pod *corev1.Pod) any {
	type podVolume struct {
		Name, ClaimName string
		ReadOnly        bool
	}
	type podContainer struct {
		Name    string
		Command []string
		Mounts  []string
		Ready   bool
	}
	volumes := make([]podVolume, 0, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		claim := ""
		if volume.PersistentVolumeClaim != nil {
			claim = volume.PersistentVolumeClaim.ClaimName
		}
		volumes = append(volumes, podVolume{Name: volume.Name, ClaimName: claim, ReadOnly: volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ReadOnly})
	}
	owners := make([]string, 0, len(pod.OwnerReferences))
	for _, owner := range pod.OwnerReferences {
		owners = append(owners, fmt.Sprintf("%s/%s/%s/%s/%t", owner.APIVersion, owner.Kind, owner.Name, owner.UID, owner.Controller != nil && *owner.Controller))
	}
	containers := make([]podContainer, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		entry := podContainer{Name: container.Name, Command: container.Command}
		for _, mount := range container.VolumeMounts {
			entry.Mounts = append(entry.Mounts, fmt.Sprintf("%s:%s:%t", mount.Name, mount.MountPath, mount.ReadOnly))
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container.Name {
				entry.Ready = status.Ready
			}
		}
		containers = append(containers, entry)
	}
	return []any{pod.UID, pod.Name, pod.Namespace, pod.Labels, pod.Spec.Hostname, pod.Spec.Subdomain, pod.Spec.NodeName, pod.Status.Phase, owners, volumes, containers}
}

func classProjection(class *storagev1.StorageClass) any {
	return []any{class.UID, class.Name, class.Provisioner, class.VolumeBindingMode}
}

func deploymentProjection(deployment *appsv1.Deployment) any {
	var replicas int32
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return []any{deployment.UID, deployment.Namespace, deployment.Name, deployment.Generation, replicas, deployment.Spec.Selector, deployment.Status.ObservedGeneration, deployment.Status.Replicas, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func validateTenantStorageClass(class *storagev1.StorageClass) TenantStorageResult {
	if class == nil || class.Name != tenantStorageClass || class.UID == "" || class.DeletionTimestamp != nil || class.Provisioner != tenantProvisioner || class.VolumeBindingMode == nil || *class.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return storageFailed("TENANT_STORAGE_CLASS_MISMATCH", "The local-path StorageClass is missing or does not match the selected DPF storage contract.")
	}
	return storagePassed()
}

func validateProvisionerDeployment(deployment *appsv1.Deployment) TenantStorageResult {
	if deployment == nil || deployment.Name != tenantProvisionerID || deployment.Namespace != tenantProvisionerNS || deployment.UID == "" || deployment.Generation < 1 || deployment.DeletionTimestamp != nil {
		return storageFailed("TENANT_PROVISIONER_IDENTITY_INVALID", "The local-path provisioner Deployment identity is missing, changing, or deleting.")
	}
	if deployment.Spec.Selector == nil || deployment.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != tenantProvisionerID || deployment.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != tenantProvisionerID {
		return storageFailed("TENANT_PROVISIONER_SELECTOR_INVALID", "The local-path provisioner Deployment does not match the expected release labels.")
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 1 || *deployment.Spec.Replicas > maxEtcdReplicas {
		return storageFailed("TENANT_PROVISIONER_REPLICAS_INVALID", "The local-path provisioner replica count is missing or outside the supported observation bound.")
	}
	desired := *deployment.Spec.Replicas
	if deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.Replicas != desired || deployment.Status.ReadyReplicas != desired || deployment.Status.AvailableReplicas != desired || deployment.Status.UpdatedReplicas != desired {
		return storageUnknown("TENANT_PROVISIONER_NOT_READY", "The local-path provisioner Deployment is not fully current and ready.")
	}
	return storagePassed()
}

func appendStorageSignature(read *storageRead, projection any) {
	encoded, _ := json.Marshal(projection)
	read.signature = append(read.signature, string(encoded))
}

func storageAPIError(resource string, err error) TenantStorageResult {
	if err == nil {
		return storagePassed()
	}
	switch {
	case apierrors.IsNotFound(err):
		return storageFailed("TENANT_STORAGE_RESOURCE_MISSING", "A required shared Kamaji storage resource is missing: "+resource+".")
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return storageUnknown("TENANT_STORAGE_ACCESS_DENIED", "Access to a required shared Kamaji storage resource was denied: "+resource+".")
	case errorsIsContext(err):
		return storageUnknown("TENANT_STORAGE_CANCELED", "The shared Kamaji storage observation was canceled or timed out.")
	default:
		return storageUnknown("TENANT_STORAGE_OBSERVATION_FAILED", "A required shared Kamaji storage resource could not be observed: "+resource+".")
	}
}

func errorsIsContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func storagePassed() TenantStorageResult {
	return TenantStorageResult{State: "passed", ReasonCode: "TENANT_STORAGE_READY", Message: "Current Kamaji DataStore validation, etcd workload, and bound-volume checks passed; etcd transactions and remaining storage capacity were not tested."}
}

func storageFailed(code, message string) TenantStorageResult {
	return TenantStorageResult{State: "failed", ReasonCode: code, Message: message}
}

func storageUnknown(code, message string) TenantStorageResult {
	return TenantStorageResult{State: "unknown", ReasonCode: code, Message: message}
}
