// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/labels"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// seedAcceptanceResources adds current objects to the same completed operation
// served by Run. It does not substitute an observer or manufacture check outcomes.
func (h *nativeRuntimeHarness) seedAcceptanceResources() {
	t := h.t
	execution := h.fixture.prepared.execution
	desired, err := prepareValidationFixtureDesired(execution.retained)
	if err != nil {
		t.Fatal(err)
	}
	observer := &validationObserver{desired: desired}
	h.seedOperatorRelease(desired)
	h.discovery["/apis/apiextensions.k8s.io/v1"] = &metav1.APIResourceList{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition"}}}
	for _, value := range h.managementGets {
		object, ok := value.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if object.GetKind() == "CustomResourceDefinition" {
			nativeReadyCRD(t, object)
		}
	}
	configObject := h.managementGets[nativeResourcePath(desired.configObject, "dpfoperatorconfigs")].(*unstructured.Unstructured).DeepCopy()
	configGeneration := configObject.GetGeneration()
	configObject.Object["status"] = map[string]any{"observedGeneration": configGeneration, "version": desired.operatorVersion, "targetVersion": desired.operatorVersion, "conditions": []any{conditionMap("Ready", "True", "Ready", configGeneration)}}
	h.seedManagement(configObject)
	h.seedDeploymentPods(h.managementGets[nativeResourcePath(desired.manager, "deployments")].(*unstructured.Unstructured).DeepCopy(), nil)
	for _, manager := range validationDirectManagers {
		current := directManagerSourceFixture(t, observer, manager.role, execution.plan.Intent().Bootstrap.ArtifactPVC, false)
		current.SetLabels(map[string]string{dpfComponentLabel: manager.component, "operator.dpu.nvidia.com/dpf-version": desired.operatorVersion, "applyset.kubernetes.io/part-of": validationApplySetID(manager.role, desired.operatorNamespace)})
		h.seedDeploymentPods(current, nil)
	}
	// Published NVIDIA DPF chart, retained byte-for-byte from
	// https://helm.ngc.nvidia.com/nvidia/doca/charts/dpu-networking-v26.8.0.tgz
	// Catalog SHA256: c41c685380536ce5f2502ab9bc011cb99b3d869ed60aa9136d7eb8cf366fe090.
	archive, err := os.ReadFile("testdata/dpu-networking-v26.8.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defaulted, err := generation.DefaultedObject(desired.operator.renderedManifest, desired.configObject)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := projectSystemArtifacts(archive, execution.plan.Intent(), defaulted)
	if err != nil {
		t.Fatal(err)
	}
	h.systemArchive = archive
	releaseLock, err := releases.Resolve(execution.plan.Intent().Release)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range systemArtifactRoles() {
		h.seedSystemRole(observer, role, artifacts.Roles[role], releaseLock)
	}
	for _, crd := range artifacts.TenantCRDs {
		current := crd.DeepCopy()
		nativeReadyCRD(t, current)
		h.seedTenant(current, "customresourcedefinitions")
		group, _, _ := unstructured.NestedString(current.Object, "spec", "group")
		versions, _, _ := unstructured.NestedSlice(current.Object, "spec", "versions")
		plural, _, _ := unstructured.NestedString(current.Object, "spec", "names", "plural")
		kind, _, _ := unstructured.NestedString(current.Object, "spec", "names", "kind")
		scope, _, _ := unstructured.NestedString(current.Object, "spec", "scope")
		for _, raw := range versions {
			v := raw.(map[string]any)
			if v["served"] != true {
				continue
			}
			gv := group + "/" + v["name"].(string)
			path := "tenant /apis/" + gv
			if h.discovery[path] == nil {
				h.discovery[path] = &metav1.APIResourceList{GroupVersion: gv}
			}
			h.discovery[path].APIResources = append(h.discovery[path].APIResources, metav1.APIResource{Name: plural, Kind: kind, Namespaced: scope == "Namespaced"})
		}
	}
	h.seedRuntimeChildren(desired)
	h.seedAcceptanceDependencies(observer)
	for _, object := range acceptanceTenantStorageObjects(t, desired.operatorNamespace) {
		h.seedManagement(object.DeepCopy())
	}
	h.allowListSelector("management", "/apis/apps/v1/deployments", "app.kubernetes.io/name=local-path-provisioner")
	nodes := map[string]corev1.Node{}
	for _, node := range h.fixture.snapshot.TenantNodes {
		var typed corev1.Node
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(node.Object, &typed); err != nil {
			t.Fatal(err)
		}
		serial := typed.Labels["dpf.nvidia.com/serial-number"]
		if serial == "" {
			for _, selected := range h.fixture.activation.selected {
				if selected.dpuName == typed.Labels[corev1.LabelHostname] {
					serial = selected.item.device.Serial
				}
			}
		}
		nodes[serial] = typed
	}
	_, networking := tenantNetworkingObjects(t, execution.plan.Intent(), "https://192.0.2.20:30538", nodes)
	for path, object := range networking {
		h.tenantGets[path] = object
	}
	h.seedSystemCredentials(desired)
}

func nativeReadyCRD(t *testing.T, object *unstructured.Unstructured) {
	t.Helper()
	object.SetUID(types.UID("native-crd-" + object.GetName()))
	object.SetResourceVersion("1")
	versions, _, _ := unstructured.NestedSlice(object.Object, "spec", "versions")
	stored := []string{}
	for _, raw := range versions {
		v := raw.(map[string]any)
		if v["storage"] == true {
			stored = append(stored, v["name"].(string))
		}
	}
	_ = unstructured.SetNestedStringSlice(object.Object, stored, "status", "storedVersions")
	_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"type": "Established", "status": "True"}, map[string]any{"type": "NamesAccepted", "status": "True"}}, "status", "conditions")
}

func (h *nativeRuntimeHarness) seedTenant(object *unstructured.Unstructured, plural string) {
	path := nativeResourcePath(object, plural)
	h.tenantGets[path] = object.DeepCopy()
	listPath := strings.TrimSuffix(path, "/"+object.GetName())
	list := h.tenantLists[listPath]
	list.apiVersion, list.kind = object.GetAPIVersion(), object.GetKind()
	replaced := false
	for i := range list.items {
		if list.items[i].GetName() == object.GetName() {
			list.items[i] = *object.DeepCopy()
			replaced = true
		}
	}
	if !replaced {
		list.items = append(list.items, *object.DeepCopy())
	}
	h.tenantLists[listPath] = list
	if object.GetKind() == "ServiceChain" || object.GetKind() == "NodeServiceInterfaces" {
		h.allowListSelector("tenant", listPath, labels.Set(object.GetLabels()).AsSelector().String())
	}
}

func (h *nativeRuntimeHarness) seedOperatorRelease(desired *managementDesired) {
	operator := desired.operator
	chart, err := loader.LoadArchive(bytes.NewReader(operator.archive))
	if err != nil {
		h.t.Fatal(err)
	}
	values, err := jsonMap(operator.valuesYAML)
	if err != nil {
		h.t.Fatal(err)
	}
	installed := &release.Release{Name: operator.releaseName, Namespace: operator.namespace, Version: 1, Chart: chart, Config: values, Manifest: string(operator.renderedManifest), Info: &release.Info{Status: release.StatusDeployed}}
	raw, err := json.Marshal(installed)
	if err != nil {
		h.t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(raw); err != nil {
		h.t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		h.t.Fatal(err)
	}
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + operator.releaseName + ".v1", Namespace: operator.namespace, UID: "native-helm-secret", ResourceVersion: "1", Labels: map[string]string{"owner": "helm", "name": operator.releaseName, "status": "deployed", "version": "1"}}, Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))}}
	h.seedManagement(nativeObject(h.t, secret))
	h.allowListSelector("management", "/api/v1/namespaces/"+operator.namespace+"/secrets", "name="+operator.releaseName+",owner=helm")
}

// Deployment ownership is explicit through a current ReplicaSet and ready Pod.
func (h *nativeRuntimeHarness) seedDeploymentPods(current *unstructured.Unstructured, app *unstructured.Unstructured) {
	if current.GetUID() == "" {
		current.SetUID(types.UID("native-" + current.GetName()))
	}
	current.SetResourceVersion("1")
	current.SetGeneration(1)
	n := int64(1)
	if replicas, ok, _ := unstructured.NestedInt64(current.Object, "spec", "replicas"); ok {
		n = replicas
	}
	current.Object["status"] = map[string]any{"observedGeneration": int64(1), "replicas": n, "updatedReplicas": n, "readyReplicas": n, "availableReplicas": n, "conditions": []any{map[string]any{"type": "Available", "status": "True"}, map[string]any{"type": "Progressing", "status": "True", "reason": "NewReplicaSetAvailable"}}}
	if app != nil {
		current.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": app.GetName() + ":apps/Deployment:" + current.GetNamespace() + "/" + current.GetName()})
	}
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &deployment); err != nil {
		h.t.Fatal(err)
	}
	// Match the API's typed JSON omission rules on both controller levels.
	h.seedManagement(nativeObject(h.t, &deployment))
	replicaSet := &appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}, ObjectMeta: metav1.ObjectMeta{Name: current.GetName() + "-replica", Namespace: current.GetNamespace(), UID: types.UID("native-rs-" + current.GetName()), ResourceVersion: "1", Labels: deployment.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: current.GetName(), UID: current.GetUID(), Controller: ptr.To(true)}}}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(n)), Selector: deployment.Spec.Selector, Template: deployment.Spec.Template}}
	replicaSet.Generation = 1
	replicaSet.Status = appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: int32(n), ReadyReplicas: int32(n), AvailableReplicas: int32(n)}
	h.seedManagement(nativeObject(h.t, replicaSet))
	for i := int64(0); i < n; i++ {
		pod := nativeReadyPod(fmt.Sprintf("%s-%d", current.GetName(), i), current.GetNamespace(), "management-node", deployment.Spec.Template)
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name, UID: replicaSet.UID, Controller: ptr.To(true)}}
		pod.Spec.Priority = ptr.To(nativePriority(pod.Spec.PriorityClassName))
		pod.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
		pod.Spec.EnableServiceLinks = ptr.To(true)
		for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
			pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
		}
		nativeAdmitManagementCredentials(&pod)
		if current.GetLabels()["app.kubernetes.io/name"] == "kamaji" {
			locked, err := releases.Resolve(h.fixture.prepared.execution.plan.Intent().Release)
			if err != nil {
				h.t.Fatal(err)
			}
			pod.Status.ContainerStatuses[0].ImageID = "docker-pullable://docker.io/clastix/kamaji@" + locked.KamajiCompatibility.PlatformManifest
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://native-kamaji-manager"
		}
		h.seedManagement(nativeObject(h.t, &pod))
	}
}

func nativeReadyPod(name, namespace, node string, template corev1.PodTemplateSpec) corev1.Pod {
	pod := corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("native-pod-" + name), ResourceVersion: "1", Labels: template.Labels, Annotations: template.Annotations}, Spec: *template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	pod.Spec.NodeName = node
	for _, container := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: container.Name, Image: container.Image, ImageID: "sha256:native-" + container.Name, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	}
	for _, container := range pod.Spec.InitContainers {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{Name: container.Name, Image: container.Image, ImageID: "sha256:native-init-" + container.Name, Ready: true, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}})
	}
	return pod
}

func (h *nativeRuntimeHarness) seedSystemRole(o *validationObserver, role systemArtifactRole, expected systemArtifactExpectation, lock releases.Release) {
	intent := o.desired.plan.Intent()
	target := intent.Bootstrap.Tenant.Name
	suffix := "tenant"
	if role.Management {
		target = "in-cluster"
		suffix = "management"
	}
	name := role.Component + "-" + suffix
	id := "native-" + name
	service := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, name)
	service.SetUID(types.UID("native-service-" + name))
	service.SetResourceVersion("1")
	service.SetGeneration(1)
	labels := map[string]string{dpfComponentLabel: role.Component, "operator.dpu.nvidia.com/dpf-version": o.desired.operatorVersion, "applyset.kubernetes.io/part-of": validationApplySetID(role.Component, o.desired.operatorNamespace)}
	daemonLabels := map[string]any{dpfComponentLabel: role.Component, hbnServiceIDLabel: id}
	if role.Management {
		labels[dpfDPUClusterNameLabel] = intent.Bootstrap.Tenant.Name
		labels[dpfDPUClusterNamespaceLabel] = intent.Bootstrap.Tenant.Namespace
		daemonLabels[dpfDPUClusterNameLabel] = intent.Bootstrap.Tenant.Name
		daemonLabels[dpfDPUClusterNamespaceLabel] = intent.Bootstrap.Tenant.Namespace
	}
	service.SetLabels(labels)
	service.Object["spec"] = map[string]any{"deployInCluster": role.Management, "helmChart": map[string]any{"source": map[string]any{"repoURL": lock.SystemNetworking.ChartRepository, "chart": lock.SystemNetworking.Chart, "version": lock.SystemNetworking.ChartVersion}, "values": runtime.DeepCopyJSON(expected.Values)}, "serviceDaemonSet": map[string]any{"labels": daemonLabels}}
	service.Object["status"] = map[string]any{"serviceID": id, "observedGeneration": int64(1), "conditions": []any{conditionMap("Ready", "True", "Success", 1), conditionMap("ApplicationsReconciled", "True", "Success", 1), conditionMap("ApplicationsReady", "True", "Success", 1)}}
	h.seedManagement(service)
	app := validationObject("argoproj.io/v1alpha1", "Application", o.desired.operatorNamespace, dpfApplicationName(target, name))
	app.SetUID(types.UID("native-app-" + name))
	app.SetResourceVersion("1")
	app.SetGeneration(1)
	app.SetFinalizers([]string{argoApplicationFinalizer})
	appLabels := map[string]string{dpfComponentLabel: dpfServiceManagerComponent, dpfDPUServiceNameLabel: name, dpfDPUServiceNamespaceLabel: service.GetNamespace(), dpfDPUClusterNameLabel: target}
	if !role.Management {
		appLabels[dpfDPUClusterNamespaceLabel] = intent.Bootstrap.Tenant.Namespace
	}
	app.SetLabels(appLabels)
	global, err := o.systemExpectedDaemonSetValues(role, service)
	if err != nil {
		h.t.Fatal(err)
	}
	values := runtime.DeepCopyJSON(expected.Values)
	values["global"] = map[string]any{"serviceDaemonSet": global}
	source := map[string]any{"repoURL": lock.SystemNetworking.ChartRepository, "chart": lock.SystemNetworking.Chart, "targetRevision": lock.SystemNetworking.ChartVersion, "helm": map[string]any{"valuesObject": values}}
	destination := map[string]any{"name": target, "namespace": service.GetNamespace()}
	app.Object["spec"] = map[string]any{"source": source, "destination": destination}
	app.Object["status"] = map[string]any{"sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": runtime.DeepCopyJSON(source), "destination": runtime.DeepCopyJSON(destination)}}, "health": map[string]any{"status": "Healthy"}, "reconciledAt": time.Now().Add(-time.Minute).Format(time.RFC3339Nano), "resources": []any{}}
	bound, err := o.specializeSystemArtifact(role, expected, service, app)
	if err != nil {
		h.t.Fatal(err)
	}
	resources := []any{}
	seed := func(object *unstructured.Unstructured, plural string) {
		current := object.DeepCopy()
		current.SetUID(types.UID("native-" + current.GetKind() + "-" + current.GetName()))
		current.SetResourceVersion("1")
		current.SetGeneration(1)
		current.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": app.GetName() + ":" + current.GroupVersionKind().Group + "/" + current.GetKind() + ":" + current.GetNamespace() + "/" + current.GetName()})
		resources = append(resources, map[string]any{"group": current.GroupVersionKind().Group, "version": current.GroupVersionKind().Version, "kind": current.GetKind(), "name": current.GetName(), "namespace": current.GetNamespace(), "status": "Synced"})
		if current.GetKind() == "Service" {
			_ = unstructured.SetNestedField(current.Object, "10.96.0.20", "spec", "clusterIP")
			ports, _, _ := unstructured.NestedSlice(current.Object, "spec", "ports")
			for i, raw := range ports {
				raw.(map[string]any)["nodePort"] = int64(31001 + i)
			}
			_ = unstructured.SetNestedSlice(current.Object, ports, "spec", "ports")
		}
		if role.Management {
			h.seedManagement(current)
		} else {
			h.seedTenant(current, plural)
		}
	}
	for _, object := range bound.ConfigMaps {
		seed(object, "configmaps")
	}
	for _, object := range bound.Attachments {
		seed(object, "network-attachment-definitions")
	}
	for _, object := range bound.Services {
		seed(object, "services")
	}
	if bound.Workload != nil {
		seed(bound.Workload, strings.ToLower(bound.Workload.GetKind())+"s")
		if role.Management {
			h.seedDeploymentPods(bound.Workload.DeepCopy(), app)
		} else {
			h.seedSystemDaemonSet(bound.Workload, app)
		}
	}
	_ = unstructured.SetNestedSlice(app.Object, resources, "status", "resources")
	h.managementGets[nativeResourcePath(app, "applications")] = app
}

func (h *nativeRuntimeHarness) seedSystemDaemonSet(object, app *unstructured.Unstructured) {
	var set appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &set); err != nil {
		h.t.Fatal(err)
	}
	set.UID = types.UID("native-" + set.Name)
	set.ResourceVersion = "1"
	set.Generation = 1
	set.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": app.GetName() + ":apps/DaemonSet:" + set.Namespace + "/" + set.Name}
	n := int32(len(h.fixture.snapshot.TenantNodes))
	set.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: n, CurrentNumberScheduled: n, UpdatedNumberScheduled: n, NumberReady: n, NumberAvailable: n}
	h.seedTenant(nativeObject(h.t, &set), "daemonsets")
	for _, node := range h.fixture.snapshot.TenantNodes {
		pod := nativeReadyPod(set.Name+"-"+node.GetName(), set.Namespace, node.GetName(), set.Spec.Template)
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}}
		nativeAdmitDaemonPod(&pod)
		h.seedTenant(nativeObject(h.t, &pod), "pods")
	}
}

func (h *nativeRuntimeHarness) allowListSelector(domain, path, selector string) {
	key := domain + " " + path
	if h.listSelectors[key] == nil {
		h.listSelectors[key] = map[string]bool{}
	}
	h.listSelectors[key][selector] = true
}

func (h *nativeRuntimeHarness) seedAcceptanceDependencies(o *validationObserver) {
	for _, requirement := range pinnedHBNAPIs() {
		crd := validationObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", requirement.crd)
		scope := "Cluster"
		if requirement.namespaced {
			scope = "Namespaced"
		}
		crd.Object["spec"] = map[string]any{"group": requirement.group, "names": map[string]any{"plural": requirement.resource, "kind": requirement.kind}, "scope": scope, "versions": []any{map[string]any{"name": requirement.version, "served": true, "storage": true}}}
		nativeReadyCRD(h.t, crd)
		h.seedManagement(crd)
		path := "/apis/" + requirement.group + "/" + requirement.version
		if h.discovery[path] == nil {
			h.discovery[path] = &metav1.APIResourceList{GroupVersion: requirement.group + "/" + requirement.version}
		}
		h.discovery[path].APIResources = append(h.discovery[path].APIResources, metav1.APIResource{Name: requirement.resource, Kind: requirement.kind, Namespaced: requirement.namespaced})
	}
	node := &corev1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: "management-node", UID: "native-management-node", ResourceVersion: "1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}}}
	h.seedManagement(nativeObject(h.t, node))
	for _, workload := range o.validationExternalWorkloads() {
		selector, err := labels.ConvertSelectorToLabelsMap(workload.selector)
		if err != nil {
			h.t.Fatal(err)
		}
		name := strings.ReplaceAll(workload.id, ".", "-")
		pod := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: selector}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: "example.test/controller:v1"}}}}
		if workload.id == "kamaji.controller" {
			locked, err := releases.Resolve(o.desired.plan.Intent().Release)
			if err != nil {
				h.t.Fatal(err)
			}
			pod.Spec.ServiceAccountName = "kamaji"
			pod.Spec.Containers = []corev1.Container{{Name: "manager", Image: locked.KamajiCompatibility.Image, Args: []string{"manager", "--health-probe-bind-address=:8081", "--leader-elect", "--metrics-bind-address=:8080", "--tmp-directory=/tmp/kamaji", "--datastore=default"}, Env: []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}, {Name: "SERVICE_ACCOUNT", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}}}, ReadinessProbe: &corev1.Probe{}, LivenessProbe: &corev1.Probe{}}}
		}
		if workload.kind == "StatefulSet" {
			pod.Spec.Containers[0].Name = "application-controller"
			pod.Spec.Containers[0].Command = []string{"argocd-application-controller"}
			pod.Spec.Containers[0].Args = []string{"--application-namespaces=" + workload.namespace}
			set := &appsv1.StatefulSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workload.namespace, UID: types.UID(name), Generation: 1, ResourceVersion: "1", Labels: selector}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: pod}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "current", UpdateRevision: "current"}}
			h.seedManagement(nativeObject(h.t, set))
		} else {
			deployment := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workload.namespace, Labels: selector}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: pod}}
			h.seedDeploymentPods(nativeObject(h.t, deployment), nil)
		}
		plural := "deployments"
		if workload.kind == "StatefulSet" {
			plural = "statefulsets"
		}
		h.allowListSelector("management", "/apis/apps/v1/namespaces/"+workload.namespace+"/"+plural, workload.selector)
	}
}

func (h *nativeRuntimeHarness) seedSystemCredentials(desired *managementDesired) {
	tenant := desired.plan.Intent().Bootstrap.Tenant
	suffix := systemClusterSuffix(tenant.Name, tenant.Namespace)
	source := h.fixture.client.sources.sources.secret
	kubeconfig, err := clientcmd.Load(source.Data["super-admin.conf"])
	if err != nil {
		h.t.Fatal(err)
	}
	ca := kubeconfig.Clusters[kubeconfig.Contexts[kubeconfig.CurrentContext].Cluster].CertificateAuthorityData
	for _, role := range systemArtifactRoles() {
		if !role.Management {
			continue
		}
		name := systemCredentialRequestStem(role) + "-" + suffix
		secretName := role.Component + "-credentials-" + suffix
		request := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUServiceCredentialRequest", desired.operatorNamespace, name)
		request.SetUID(types.UID("native-" + name))
		request.SetResourceVersion("1")
		request.SetGeneration(1)
		request.Object["spec"] = map[string]any{"targetCluster": map[string]any{"name": tenant.Name, "namespace": tenant.Namespace}, "serviceAccount": map[string]any{"name": role.Component + "-" + suffix, "namespace": desired.operatorNamespace}, "secret": map[string]any{"name": secretName, "namespace": desired.operatorNamespace}, "type": "tokenFile"}
		request.Object["status"] = map[string]any{"observedGeneration": int64(1), "expirationTimestamp": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "conditions": []any{conditionMap("Ready", "True", "Success", 1), conditionMap("ServiceAccountReconciled", "True", "Success", 1), conditionMap("SecretReconciled", "True", "Success", 1)}}
		h.seedManagement(request)
		secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: desired.operatorNamespace, UID: types.UID("native-" + secretName), ResourceVersion: "1", Labels: map[string]string{"svc.dpu.nvidia.com/managed-by-controller": "dpuservicecredentialrequestcontroller", "svc.dpu.nvidia.com/credential-request-name": name, "svc.dpu.nvidia.com/credential-request-namespace": desired.operatorNamespace}}, Data: map[string][]byte{"KUBERNETES_SERVICE_HOST": []byte("192.0.2.20"), "KUBERNETES_SERVICE_PORT": []byte("30538"), "KUBERNETES_CA_DATA": ca, "TOKEN_FILE": []byte("native-fixture-token")}}
		h.seedManagement(nativeObject(h.t, secret))
	}
}

func (h *nativeRuntimeHarness) acceptancePhysicalReaders() validationPhysicalReaders {
	read := func(_ context.Context, target config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, host := range h.inventory.Hosts {
			if host.SSHEndpoint == target.Endpoint {
				return cloneValidationDevice(h.t, host), nil, nil
			}
		}
		for _, device := range h.inventory.DPUs {
			if device.BMCEndpoint == target.Endpoint {
				current := cloneValidationDevice(h.t, device)
				current.NetworkFunctionMACs = nativePhysicalMACs(current.SerialNumber)
				return current, nil, nil
			}
		}
		return inventory.Device{}, nil, fmt.Errorf("unselected current physical source")
	}
	return validationPhysicalReaders{bmc: read, host: read, fabric: func(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot { return h.physicalSnapshot }}
}

// Reuse the established SFC child geometry on this harness's retained workload.
func (h *nativeRuntimeHarness) seedRuntimeChildren(desired *managementDesired) {
	f := h.fixture
	objects := map[string]*unstructured.Unstructured{}
	for key, value := range h.managementGets {
		if object, ok := value.(*unstructured.Unstructured); ok {
			objects[key] = object
		}
	}
	children := runtimeValidationChildren(h.t, f, desired, objects)
	resources := map[string]string{"ServiceChainSet": "servicechainsets", "ServiceChain": "servicechains", "ServiceInterfaceSet": "serviceinterfacesets", "NodeServiceInterfaces": "nodeserviceinterfaces", "CIDRPool": "cidrpools"}
	for _, child := range children {
		if child.GetKind() == "CIDRPool" {
			if current, ok := h.tenantGets[nativeResourcePath(child, "cidrpools")].(*unstructured.Unstructured); ok {
				allocations, found, err := unstructured.NestedSlice(current.Object, "status", "allocations")
				if err != nil {
					h.t.Fatal(err)
				}
				if found {
					_ = unstructured.SetNestedSlice(child.Object, allocations, "status", "allocations")
				}
				child.SetGeneration(current.GetGeneration())
			}
		}
		if child.GetKind() == "ServiceInterfaceSet" || child.GetKind() == "ServiceChainSet" {
			_ = unstructured.SetNestedField(child.Object, int64(len(f.snapshot.TenantNodes)), "status", "numberApplied")
		}
		h.seedTenant(child, resources[child.GetKind()])
	}
	for _, object := range objects {
		if strings.HasPrefix(object.GetKind(), "DPUService") {
			n3cReadyWorkload(object)
			h.seedManagement(object)
		}
	}
}

func (h *nativeRuntimeHarness) verifyAcceptanceHostNetwork(ctx context.Context, target config.Target, request sshprobe.HostNetworkRequest, timeout time.Duration) (sshprobe.HostNetworkValidation, error) {
	h.mu.Lock()
	serial := request.Identity.AdapterSerial
	saved, ok := h.fixture.prepared.operation.hostTransaction(serial)
	if !ok || !reflect.DeepEqual(saved.Request.Identity, request.Identity) || saved.Request.HostAddressPrefix != request.HostAddressPrefix || saved.Request.Gateway != request.Gateway || saved.Request.WorkloadCIDR != request.WorkloadCIDR || saved.Request.MTU != request.MTU || target.InputRef != saved.Result.HostInputRef {
		h.mu.Unlock()
		return sshprobe.HostNetworkValidation{}, fmt.Errorf("current network qualification changed selected host or retained desired network")
	}
	if h.hostNetworkTargets == nil {
		h.hostNetworkTargets = map[string]config.Target{}
	}
	local, exists := h.hostNetworkTargets[serial]
	if !exists {
		local = h.newHostNetworkSnapshotServer(request)
		h.hostNetworkTargets[serial] = local
	}
	h.mu.Unlock()
	return sshprobe.VerifyHostWorkloadNetwork(ctx, local, request, timeout)
}

// The source protocol matches sshprobe/configuredHostNetworkSnapshot. Only the
// snapshot and SSH connection reads are served; every effect is rejected.
func (h *nativeRuntimeHarness) newHostNetworkSnapshotServer(request sshprobe.HostNetworkRequest) config.Target {
	t := h.t
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverConfig := &ssh.ServerConfig{PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if metadata.User() != "reader" || string(password) != "native-snapshot-password" {
			return nil, fmt.Errorf("unexpected snapshot authentication")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(signer)
	snapshot := nativeHostNetworkSnapshot(request)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				server, channels, requests, err := ssh.NewServerConn(connection, serverConfig)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					go func() {
						defer channel.Close()
						for req := range requests {
							var payload struct{ Command string }
							if req.Type != "exec" || ssh.Unmarshal(req.Payload, &payload) != nil {
								_ = req.Reply(false, nil)
								continue
							}
							_ = req.Reply(true, nil)
							command := payload.Command
							switch {
							case strings.Contains(command, `printf '%s' "$SSH_CONNECTION"`):
								_, _ = channel.Write([]byte("192.0.2.10 40000 192.0.2.20 22"))
							case strings.Contains(command, "' -- snapshot "):
								_, _ = channel.Write(snapshot)
							default:
								t.Errorf("host qualification attempted non-snapshot command")
								_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
								return
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	if err = os.WriteFile(known, []byte(knownhosts.Line([]string{listener.Addr().String()}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(dir, "password")
	if err = os.WriteFile(password, []byte("native-snapshot-password"), 0600); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf("credentials:\n  reader: {username: reader, passwordRef: {file: %s}}\ndiscovery:\n  hosts:\n    - credentialRef: reader\n      ssh: {address: %s, knownHostsFile: %s}\n  probes: {mode: inventory}\n", password, listener.Addr().String(), known)
	input, err := config.Decode([]byte(document), filepath.Join(dir, "source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Targets[0]
}

func nativeHostNetworkSnapshot(request sshprobe.HostNetworkRequest) []byte {
	identity := request.Identity
	address := netip.MustParsePrefix(request.HostAddressPrefix)
	merged := fmt.Sprintf("network:\n  version: 2\n  renderer: networkd\n  ethernets:\n    data0:\n      match: {macaddress: '%s'}\n      set-name: %s\n      mtu: %d\n      addresses: [%s]\n      routes: [{to: %s, via: %s}]\n", identity.PermanentMAC, identity.InterfaceName, request.MTU, request.HostAddressPrefix, request.WorkloadCIDR, request.Gateway)
	snapshot := map[string]any{
		"waitOnlineOtherMatchOnly": false, "os": "ubuntu:24.04", "netplanVersion": "1.1.2-8ubuntu1~24.04.3", "systemdVersion": "systemd 255 (255.4)", "ipVersion": "ip utility, iproute2-6.1.0", "networkd": "active", "otherManager": "inactive",
		"uuid": identity.SystemUUID, "bootID": "123e4567-e89b-12d3-a456-426614174001", "vpd": base64.StdEncoding.EncodeToString([]byte("[SN] Serial number: " + identity.AdapterSerial + "\n")), "binding": "/sys/devices/pci0000:20/" + identity.PCIAddress, "physicalPort": identity.PhysicalPort, "permanentMAC": "Permanent address: " + identity.PermanentMAC,
		"link":             []any{map[string]any{"ifname": identity.InterfaceName, "link_type": "ether", "mtu": request.MTU, "min_mtu": 68, "max_mtu": 9700, "flags": []any{"UP"}}},
		"addresses":        []any{map[string]any{"addr_info": []any{map[string]any{"family": "inet", "local": address.Addr().String(), "prefixlen": address.Bits()}}}},
		"routes":           []any{map[string]any{"dst": "default", "gateway": "192.0.2.1", "dev": "eth0"}, map[string]any{"dst": request.WorkloadCIDR, "gateway": request.Gateway, "dev": identity.InterfaceName}},
		"rules4":           []any{map[string]any{"priority": 0, "src": "all", "table": "local"}, map[string]any{"priority": 32766, "src": "all", "table": "main"}, map[string]any{"priority": 32767, "src": "all", "table": "default"}},
		"rules6":           []any{map[string]any{"priority": 0, "src": "all", "table": "local"}, map[string]any{"priority": 32766, "src": "all", "table": "main"}},
		"managementDevice": "eth0", "networkFile": "/run/systemd/network/10-netplan-data0.network", "defaultRoutes6": []any{}, "merged": base64.StdEncoding.EncodeToString([]byte(merged)), "sourceDigest": strings.Repeat("b", 64), "fence": strings.Repeat("c", 64), "runtimeDigest": strings.Repeat("d", 64),
	}
	data, _ := json.Marshal(snapshot)
	return data
}

// This is the same physical geometry as newPhysicalFixtureInputs, declared
// before generation so validation retains its independent expected identity.
func nativeAcceptancePhysicalInputs(t *testing.T, input generation.Inputs) (config.Request, *inventory.Inventory, fabric.Snapshot, []byte) {
	devices := make([]generation.ResolvedDevice, 0, len(input.Workload.Devices))
	for i, device := range input.Workload.Devices {
		devices = append(devices, generation.ResolvedDevice{Serial: device.Serial, PSID: device.PSID, BMCIP: input.Operator.Bootstrap.Devices[i].BMCIP, BMCPort: 443})
	}
	prepared := newPhysicalFixtureInputs(t, devices, false)
	enrichHostNetworkInventory(t, prepared.inventory)
	dir := filepath.Dir(prepared.request.Input.Path)
	known := filepath.Join(dir, "native-known-hosts")
	if err := os.WriteFile(known, []byte("*.example.test ssh-ed25519 AAAA\n"), 0600); err != nil {
		t.Fatal(err)
	}
	site := prepared.request.Input.Site
	site.DPF.UseCase = ptr.To(releases.UseCaseHBNBGP)
	site.Discovery.Hosts = nil
	site.Discovery.DPUs = nil
	ports := []config.SwitchPort{}
	for i, device := range devices {
		prepared.inventory.DPUs[i].InputRef = fmt.Sprintf("/discovery/dpus/%d", i)
		prepared.inventory.DPUs[i].BMCEndpoint = "https://" + device.BMCIP
		prepared.inventory.DPUs[i].ObservationState = "observed"
		prepared.inventory.DPUs[i].PSID = device.PSID
		prepared.inventory.DPUs[i].Model = "B4240"
		prepared.inventory.DPUs[i].RecordKey = prepared.inventory.DPUs[i].ID
		prepared.inventory.DPUs[i].ReaderAdapter = "bluefield-card-v1"
		prepared.inventory.DPUs[i].Observations = map[string]inventory.Observation{"psid": {State: "observed", ReasonCode: "PSID_OBSERVED", Tool: "redfish", Values: []string{device.PSID}}, "bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}}
		prepared.inventory.Hosts[i].RecordKey = prepared.inventory.Hosts[i].ID
		prepared.inventory.Hosts[i].ReaderAdapter = "linux-host-ssh-v1"
		prepared.inventory.Hosts[i].ObservationState = "observed"
		prepared.inventory.Hosts[i].InputRef = fmt.Sprintf("/discovery/hosts/%d", i)
		site.Discovery.DPUs = append(site.Discovery.DPUs, config.Endpoint{BMCEndpoint: "https://" + device.BMCIP, CredentialRef: "bmc"})
		site.Discovery.Hosts = append(site.Discovery.Hosts, config.Endpoint{CredentialRef: "bmc", SSH: &config.SSH{Address: strings.TrimPrefix(prepared.inventory.Hosts[i].SSHEndpoint, "ssh://"), KnownHostsFile: known}})
		for p := 0; p < 2; p++ {
			ports = append(ports, config.SwitchPort{Name: fmt.Sprintf("swp%d", i*2+p+1), DPUSerial: device.Serial, DPUInterface: fmt.Sprintf("p%d", p)})
		}
	}
	site.Discovery.Switches = []config.Switch{{Name: "leaf-a", CredentialRef: "bmc", SSH: config.SSH{Address: "leaf-a.example.test", KnownHostsFile: known}, Ports: ports}}
	request, err := config.Resolve(config.Input{Path: prepared.request.Input.Path, Site: site})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := nativeFabricSnapshot(input, ports)
	sw := request.Switches[0]
	prepared.inventory.HBN = &inventory.HBNInventory{Namespace: input.Workload.Namespace, Profile: input.Workload.Profile, Switches: []inventory.FabricSwitch{{Name: sw.Name, Endpoint: sw.Endpoint, Platform: sw.Platform, Ports: ports}}}
	prepared.inventory.Collection.Mode = request.Mode
	prepared.inventory.ScopeDigest = physicalScopeTestDigest(request, prepared.inventory)
	site.Inventory = prepared.inventory
	source, err := yaml.Marshal(site)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := config.Decode(source, prepared.request.Input.Path)
	if err != nil {
		t.Fatal(err)
	}
	request.Input = decoded
	return request, decoded.Site.Inventory, snapshot, source
}

func nativePhysicalMACs(serial string) []string {
	if serial == "SERIAL-A" {
		return []string{"20:4d:52:3e:9a:b8", "20:4d:52:3e:9a:b9"}
	}
	return []string{"20:4d:52:3e:9a:ba", "20:4d:52:3e:9a:bb"}
}

func nativeFabricSnapshot(input generation.Inputs, ports []config.SwitchPort) fabric.Snapshot {
	profile := input.Workload.Profile
	frr := fmt.Sprintf("frr defaults datacenter\nrouter bgp %d vrf %s\n", profile.FabricASN, profile.FabricVRF)
	for _, port := range ports {
		frr += " neighbor " + port.Name + " interface remote-as external\n"
	}
	frr += " address-family ipv4 unicast\n exit-address-family\n"
	result := fabric.Snapshot{Revision: &fabric.NVUERevisionState{AppliedApplyID: "native", SavedApplyID: "native"}, Configured: fabric.ConfiguredFacts{BGP: &fabric.BGPConfig{Enabled: ptr.To(true)}, FRRPolicy: fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(frr)}).Configured.FRRPolicy}}
	neighbors := []fabric.BGPNeighborConfig{}
	for _, port := range ports {
		neighbors = append(neighbors, fabric.BGPNeighborConfig{Name: port.Name, Type: ptr.To("unnumbered"), EffectiveRemoteAS: ptr.To("external"), EffectiveAddressFamilies: []string{"ipv4-unicast"}})
		result.Configured.Interfaces = append(result.Configured.Interfaces, fabric.InterfaceConfig{Name: port.Name, MTU: ptr.To(profile.UplinkMTU), VRF: ptr.To(profile.FabricVRF), BridgeDomain: ptr.To("none")})
		result.Operational.Interfaces = append(result.Operational.Interfaces, fabric.InterfaceState{Name: port.Name, AdminState: ptr.To("up"), OperState: ptr.To("up"), MTU: ptr.To(profile.UplinkMTU), VRF: ptr.To(profile.FabricVRF)})
		index := 0
		if port.DPUInterface == "p1" {
			index = 1
		}
		result.Operational.LLDP = append(result.Operational.LLDP, fabric.LLDPPeerHint{LocalInterface: port.Name, PortID: ptr.To(nativePhysicalMACs(port.DPUSerial)[index]), PortDescription: ptr.To(port.DPUInterface)})
	}
	result.Configured.VRFs = []fabric.VRFConfig{{Name: profile.FabricVRF, ASN: ptr.To(profile.FabricASN), BGPEnabled: ptr.To(true), AddressFamilies: []fabric.AddressFamilyConfig{{Name: "ipv4-unicast", Enabled: ptr.To(true)}}, Neighbors: &neighbors}}
	return result
}

func nativeAcceptanceDiagnostics(result RunResult) string {
	if result.Validation == nil {
		return "no canonical validation result"
	}
	rows := []string{}
	for _, check := range result.Checks {
		if check.State != "passed" {
			rows = append(rows, check.ID+" "+check.State+" "+check.ReasonCode+" "+check.Message)
		}
	}
	rows = append(rows, fmt.Sprintf("verdict=%s fatal=%s report=%s", result.Validation.Verdict, result.Validation.Fatal, result.Validation.Report.Status))
	if result.Validation.Cause != nil {
		rows = append(rows, "private cause: "+nativeFixtureCause(result.Validation.Cause))
	}
	for _, row := range result.Validation.Checks {
		if row.Required && row.Outcome.State != validation.Passed {
			rows = append(rows, fmt.Sprintf("%s %s %s %s", row.ID, row.Outcome.State, row.Outcome.Reason, row.Outcome.Summary))
		}
	}
	return strings.Join(rows, "\n")
}

// Standard service-account admission is independent of Pod functional matching.
func nativeAdmitManagementCredentials(pod *corev1.Pod) {
	if pod.Spec.AutomountServiceAccountToken != nil && !*pod.Spec.AutomountServiceAccountToken {
		return
	}
	needsToken := false
	for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, container := range containers {
			mounted := false
			for _, mount := range container.VolumeMounts {
				mounted = mounted || mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount"
			}
			needsToken = needsToken || !mounted
		}
	}
	if !needsToken {
		return
	}
	const name = "kube-api-access-abc12"
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(420)), Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
	}}}})
	for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for i := range containers {
			mounted := false
			for _, mount := range containers[i].VolumeMounts {
				mounted = mounted || mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount"
			}
			if !mounted {
				containers[i].VolumeMounts = append(containers[i].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
			}
		}
	}
}

func nativeFixtureCause(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range value.Unwrap() {
			text += " -> " + nativeFixtureCause(child)
		}
	case interface{ Unwrap() error }:
		text += " -> " + nativeFixtureCause(value.Unwrap())
	}
	return text
}

// Independent daemon-controller/admission facts, preserving explicit chart choices.
func nativeAdmitDaemonPod(pod *corev1.Pod) {
	if pod.Spec.ServiceAccountName == "" {
		pod.Spec.ServiceAccountName = "default"
	}
	pod.Spec.DeprecatedServiceAccount = pod.Spec.ServiceAccountName
	pod.Spec.Priority = ptr.To(nativePriority(pod.Spec.PriorityClassName))
	pod.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
	pod.Spec.EnableServiceLinks = ptr.To(true)
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: metav1.ObjectNameField, Operator: corev1.NodeSelectorOpIn, Values: []string{pod.Spec.NodeName}}}}}}
	add := func(key string, effect corev1.TaintEffect) {
		wanted := corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: effect}
		for i, current := range pod.Spec.Tolerations {
			if current.Key == key && current.Operator == wanted.Operator && current.Value == wanted.Value && current.Effect == effect {
				pod.Spec.Tolerations[i] = wanted
				return
			}
		}
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, wanted)
	}
	for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
		add(key, corev1.TaintEffectNoExecute)
	}
	for _, key := range []string{corev1.TaintNodeDiskPressure, corev1.TaintNodeMemoryPressure, corev1.TaintNodePIDPressure, corev1.TaintNodeUnschedulable} {
		add(key, corev1.TaintEffectNoSchedule)
	}
	if pod.Spec.HostNetwork {
		add(corev1.TaintNodeNetworkUnavailable, corev1.TaintEffectNoSchedule)
	}
	nativeAdmitManagementCredentials(pod)
	for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for i := range containers {
			if containers[i].ImagePullPolicy != corev1.PullNever {
				containers[i].ImagePullPolicy = corev1.PullAlways
			}
		}
	}
}

func nativePriority(class string) int32 {
	switch class {
	case "system-node-critical":
		return 2000001000
	case "system-cluster-critical":
		return 2000000000
	default:
		return 0
	}
}
