// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestValidationRejectsUnretainedSystemOverride(t *testing.T) {
	o := validationResourceFixture(t)
	release, _ := releases.Resolve(o.desired.plan.Intent().Release)
	role := systemArtifactRole{Component: "multus"}
	expected := systemArtifactValues(o.desired.plan.Intent(), release, role, false)
	changed := runtime.DeepCopyJSON(expected)
	changed["multus"].(map[string]any)["multusDaemonConfig"] = map[string]any{"daemonConfigJson": `{"type":"multus","delegates":[]}`}
	if systemValuesMatch(changed, expected, nil) {
		t.Fatal("unretained functional override passed")
	}
	defaults := map[string]any{"multus": map[string]any{"kubernetesClusterDomain": "cluster.local"}}
	explicit := runtime.DeepCopyJSON(expected)
	explicit["multus"].(map[string]any)["kubernetesClusterDomain"] = "cluster.local"
	if !systemValuesMatch(explicit, expected, defaults) {
		t.Fatal("independently known chart default was rejected")
	}
	explicit["multus"].(map[string]any)["kubernetesClusterDomain"] = "wrong.local"
	if systemValuesMatch(explicit, expected, defaults) {
		t.Fatal("changed chart default passed")
	}
}
func TestValidationSystemWorkloadRejectsSameImageCommandAndUnknownFields(t *testing.T) {
	one := int32(1)
	desired := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "selected"}, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dns"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "dns"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "coredns", Image: "registry.example/dns:v1", Command: []string{"/coredns"}}}}}}}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	want := &unstructured.Unstructured{Object: raw}
	current := want.DeepCopy()
	normalized, err := normalizedManagerSpec(want)
	if err != nil {
		t.Fatal(err)
	}
	normalizedData, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&normalized)
	current.Object["spec"] = normalizedData
	if !validationSystemWorkloadMatches(want, current) {
		t.Fatal("ordinary Kubernetes defaults were rejected")
	}
	containers, _, _ := unstructured.NestedSlice(current.Object, "spec", "template", "spec", "containers")
	containers[0].(map[string]any)["command"] = []any{"/bin/sh", "-c", "sleep infinity"}
	unstructured.SetNestedSlice(current.Object, containers, "spec", "template", "spec", "containers")
	if validationSystemWorkloadMatches(want, current) {
		t.Fatal("same-image sleep command passed independent projection")
	}
	ds := validationExpectedKubeProxy("v1.35.6")
	other := ds.DeepCopy()
	if !validationSystemWorkloadMatches(ds, other) {
		t.Fatal("healthy exact addon workload rejected")
	}
	unstructured.SetNestedField(other.Object, true, "spec", "unknownFutureField")
	if validationSystemWorkloadMatches(ds, other) {
		t.Fatal("DaemonSet strict decoding discarded an unknown spec field")
	}
}
func TestValidationConsumedConfigMapUsesIndependentContent(t *testing.T) {
	app := validationObject("argoproj.io/v1alpha1", "Application", "selected", "system-app")
	wanted := validationObject("v1", "ConfigMap", "selected", "multus-daemon-config")
	wanted.Object["data"] = map[string]any{"daemon-config.json": `{"type":"multus","delegates":[{"type":"flannel"}]}`}
	app.Object["status"] = map[string]any{"resources": []any{map[string]any{"kind": "ConfigMap", "group": "", "version": "v1", "namespace": "selected", "name": wanted.GetName(), "status": "Synced"}}}
	current := wanted.DeepCopy()
	current.SetUID("config")
	current.SetResourceVersion("1")
	current.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "system-app:/ConfigMap:selected/" + wanted.GetName()})
	reader := &validationReadFake{get: func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return current.DeepCopy(), nil
	}}
	expected := systemArtifactExpectation{ConfigMaps: map[string]*unstructured.Unstructured{wanted.GetName(): wanted}}
	if err := verifyValidationSystemConfiguration(context.Background(), reader, app, expected, systemArtifactRole{Component: "multus"}); err != nil {
		t.Fatal(err)
	}
	current.Object["data"].(map[string]any)["daemon-config.json"] = `{"type":"multus","delegates":[]}`
	if verifyValidationSystemConfiguration(context.Background(), reader, app, expected, systemArtifactRole{Component: "multus"}) == nil {
		t.Fatal("changed delegates passed because App and images remained healthy")
	}
}
func TestValidationSpecializationRequiresQualifiedArtifact(t *testing.T) {
	o := validationResourceFixture(t)
	if _, err := o.specializeSystemArtifact(systemArtifactRole{Component: "multus"}, systemArtifactExpectation{}, nil, nil); err == nil {
		t.Fatal("missing qualified archive bypassed projection")
	}
}
func TestValidationFiniteDNSAndAddonHealthyDependencyFixture(t *testing.T) {
	host := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns"), NodePort: 31001}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("dns-tcp"), NodePort: 31002}}}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", UID: "dns", ResourceVersion: "1", Labels: map[string]string{"k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)}}}}
	slice := &discoveryv1.EndpointSlice{TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", UID: "endpoint", ResourceVersion: "1", Labels: map[string]string{discoveryv1.LabelServiceName: "kube-dns", discoveryv1.LabelManagedBy: "kamaji-cluster-manager"}}, AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Name: ptr.To("dns"), Port: ptr.To(int32(31001)), Protocol: ptr.To(corev1.ProtocolUDP)}, {Name: ptr.To("dns-tcp"), Port: ptr.To(int32(31002)), Protocol: ptr.To(corev1.ProtocolTCP)}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.10"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(slice)
	objects := []unstructured.Unstructured{{Object: raw}}
	if err := validationTenantDNSService(service, "10.96.0.0/16"); err != nil {
		t.Fatal(err)
	}
	if err := validationTenantDNSEndpoints(objects, host, "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	wrong := slice.DeepCopy()
	wrong.Endpoints[0].Addresses = []string{"192.0.2.99"}
	changed, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(wrong)
	if validationTenantDNSEndpoints([]unstructured.Unstructured{{Object: changed}}, host, "192.0.2.10") == nil {
		t.Fatal("wrong DNS VIP passed healthy App/Pod evidence")
	}
	wrong = slice.DeepCopy()
	wrong.Ports[1].Port = ptr.To(int32(31001))
	changed, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(wrong)
	if validationTenantDNSEndpoints([]unstructured.Unstructured{{Object: changed}}, host, "192.0.2.10") == nil {
		t.Fatal("TCP/UDP NodePort substitution passed")
	}
	if validationTenantDNSEndpoints(nil, host, "192.0.2.10") == nil {
		t.Fatal("missing DNS endpoints passed")
	}
	service.Spec.Selector = map[string]string{"k8s-app": "unrelated"}
	if validationTenantDNSService(service, "10.96.0.0/16") == nil {
		t.Fatal("tenant DNS selector bypassed explicit forwarding")
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kubeadm:node-proxier", UID: "proxy-binding", ResourceVersion: "1"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:node-proxier"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-proxy", Namespace: "kube-system"}}}
	if err := validationKubeProxyBinding(binding); err != nil {
		t.Fatal(err)
	}
	owner := []metav1.OwnerReference{{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: binding.Name, UID: binding.UID, Controller: ptr.To(true)}}
	if !validationKubeProxyOwned(owner, binding.UID) || validationKubeProxyOwned(owner, types.UID("replacement")) {
		t.Fatal("addon owner replacement was not fenced")
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", UID: "proxy-config", ResourceVersion: "1"}, Data: map[string]string{"config.conf": "apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nbindAddress: 0.0.0.0\nclusterCIDR: 10.244.0.0/16\nclientConnection:\n  kubeconfig: /var/lib/kube-proxy/kubeconfig.conf\nmode: \"\"\n", "kubeconfig.conf": "apiVersion: v1\nkind: Config\nclusters:\n- name: default\n  cluster:\n    server: https://192.0.2.10:6443\n    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\ncontexts:\n- name: default\n  context: {cluster: default, user: default, namespace: default}\ncurrent-context: default\nusers:\n- name: default\n  user:\n    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\n"}}
	if err := validationKubeProxyConfiguration(config, "10.244.0.0/16", "https://192.0.2.10:6443"); err != nil {
		t.Fatal(err)
	}
	config.Data["config.conf"] = strings.Replace(config.Data["config.conf"], "mode: \"\"", "mode: ipvs", 1)
	if validationKubeProxyConfiguration(config, "10.244.0.0/16", "https://192.0.2.10:6443") == nil {
		t.Fatal("unsupported addon mode passed")
	}
}

type validationConditionsSnapshotObserver struct {
	o      *validationObserver
	checks []validation.CheckSpec
	calls  int
	object *unstructured.Unstructured
}

func (s *validationConditionsSnapshotObserver) ObserveResources(_ context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	s.calls++
	r := newValidationRecorder(s.checks, progress)
	if s.calls > 1 {
		unstructured.SetNestedField(s.object.Object, int64(2), "status", "observedGeneration")
		unstructured.SetNestedSlice(s.object.Object, []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}, "status", "conditions")
	}
	for _, spec := range s.checks {
		if spec.ID == "system/current" {
			r.run(spec.ID, func() error { return validationReady(s.o.desired.rules.ValidationConditions(s.object, "Ready")) })
		} else {
			r.record(spec.ID, validation.Passed, validation.ReasonNone, "Independent fixture dependency is ready.", "")
		}
	}
	return r.snapshot(), nil
}
func (*validationConditionsSnapshotObserver) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	panic("resource-only fixture")
}
func TestValidationObservedGenerationSnapshotsConvergeThroughRunner(t *testing.T) {
	o := validationResourceFixture(t)
	checks := []validation.CheckSpec{}
	for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaTenant, validation.AreaDevices, validation.AreaSoftware, validation.AreaHBN, validation.AreaWorkloads, validation.AreaPhysical} {
		id := string(area) + "/coverage"
		if area == validation.AreaSystem {
			id = "system/current"
		}
		spec := validationSpec(id, area, string(area), true)
		spec.FullOnly = area == validation.AreaPhysical
		checks = append(checks, spec)
	}
	scope := validation.RuntimeScope{Devices: []validation.DeviceID{"selected"}, Hosts: []validation.HostID{"selected"}}
	plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: checks, Runtime: scope})
	if err != nil {
		t.Fatal(err)
	}
	object := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", "selected", "system")
	object.SetGeneration(2)
	object.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}}
	active := []validation.CheckSpec{}
	for _, check := range checks {
		if !check.FullOnly {
			active = append(active, check)
		}
	}
	observer := &validationConditionsSnapshotObserver{o: o, checks: active, object: object}
	runner, err := validation.NewRunner(plan, observer, validation.Options{Wait: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if result.Verdict != validation.VerdictResourcesPassed || observer.calls != 2 {
		t.Fatalf("ordinary stale snapshots did not converge: verdict=%s calls=%d", result.Verdict, observer.calls)
	}
	for _, value := range []any{int64(3), int64(-1), "malformed"} {
		object.Object["status"].(map[string]any)["observedGeneration"] = value
		r := newValidationRecorder([]validation.CheckSpec{validationSpec("system/current", validation.AreaSystem, "system", false)}, nil)
		r.run("system/current", func() error { return validationReady(o.desired.rules.ValidationConditions(object, "Ready")) })
		if r.outcomes["system/current"].State != validation.Failed {
			t.Fatalf("invalid generation became progress: %v", value)
		}
	}
}

func TestValidationCoreDNSServiceDistinguishesMismatchFromAllocationProgress(t *testing.T) {
	app := validationObject("argoproj.io/v1alpha1", "Application", "selected", "dns-app")
	desired := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "selected"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: map[string]string{"app": "dns"}, Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns")}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("dns-tcp")}}}}
	raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	wanted := &unstructured.Unstructured{Object: raw}
	workload := validationObject("apps/v1", "Deployment", "selected", "dns")
	unstructured.SetNestedStringMap(workload.Object, map[string]string{"app": "dns"}, "spec", "template", "metadata", "labels")
	app.Object["status"] = map[string]any{"resources": []any{map[string]any{"kind": "Service", "group": "", "version": "v1", "namespace": "selected", "name": "dns", "status": "Synced"}}}
	expected := systemArtifactExpectation{Services: map[string]*unstructured.Unstructured{"dns": wanted}, Workload: workload}
	current := desired.DeepCopy()
	current.UID = "service"
	current.ResourceVersion = "1"
	current.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "dns-app:/Service:selected/dns"}
	current.Spec.ClusterIP = "10.96.10.10"
	current.Spec.Ports[0].NodePort = 31001
	current.Spec.Ports[1].NodePort = 31002
	observe := func(value *corev1.Service) error {
		reader := &validationReadFake{get: func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			raw, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
			return &unstructured.Unstructured{Object: raw}, nil
		}}
		_, err := verifyValidationCoreDNSService(context.Background(), reader, app, expected)
		return err
	}
	if err := observe(current); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		edit  func(*corev1.Service)
		state validation.State
	}{
		{"wrong type", func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeClusterIP }, validation.Failed},
		{"headless", func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone }, validation.Failed},
		{"not allocated", func(s *corev1.Service) { s.Spec.ClusterIP = "" }, validation.Pending},
		{"node port not allocated", func(s *corev1.Service) { s.Spec.Ports[0].NodePort = 0 }, validation.Pending},
		{"wrong selector", func(s *corev1.Service) { s.Spec.Selector["app"] = "wrong" }, validation.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := current.DeepCopy()
			test.edit(changed)
			err := observe(changed)
			failure, ok := err.(*validationCheckError)
			if !ok || failure.state != test.state {
				t.Fatalf("classification=%#v", err)
			}
		})
	}
	changed := current.DeepCopy()
	changed.ResourceVersion = "2"
	if err := validationDNSAllocationStable(current, changed); err != nil {
		t.Fatal("resourceVersion churn changed allocation")
	}
	changed.Spec.Ports[0].NodePort++
	if !validationIdentityChanged(validationDNSAllocationStable(current, changed)) {
		t.Fatal("cross-API NodePort drift was not fenced")
	}
}

func TestValidationMissingObservedGenerationRemainsProgressing(t *testing.T) {
	o := validationResourceFixture(t)
	for _, status := range []map[string]any{nil, {}, {"observedGeneration": int64(0)}} {
		object := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", "selected", "system")
		object.SetGeneration(2)
		if status != nil {
			object.Object["status"] = status
		}
		recorder := newValidationRecorder([]validation.CheckSpec{validationSpec("system/current", validation.AreaSystem, "system", false)}, nil)
		recorder.run("system/current", func() error { return validationReady(o.desired.rules.ValidationConditions(object, "Ready")) })
		if recorder.outcomes["system/current"].State != validation.Pending {
			t.Fatalf("absent/old status became terminal: %+v", recorder.outcomes["system/current"])
		}
	}
}

func TestValidationUnavailableChartDefaultsCannotAssertFunctionalDrift(t *testing.T) {
	o := validationResourceFixture(t)
	release, _ := releases.Resolve(o.desired.plan.Intent().Release)
	role := systemArtifactRole{Component: "multus"}
	expected := systemArtifactValues(o.desired.plan.Intent(), release, role, false)
	values := runtime.DeepCopyJSON(expected)
	delete(values["multus"].(map[string]any), "cniBinDir")
	object := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, "multus")
	object.SetUID("service")
	object.SetResourceVersion("1")
	object.SetGeneration(1)
	object.SetLabels(map[string]string{dpfComponentLabel: role.Component, "operator.dpu.nvidia.com/dpf-version": o.desired.operatorVersion, "applyset.kubernetes.io/part-of": validationApplySetID(role.Component, o.desired.operatorNamespace)})
	object.Object["spec"] = map[string]any{"helmChart": map[string]any{"source": map[string]any{"repoURL": release.SystemNetworking.ChartRepository, "chart": release.SystemNetworking.Chart, "version": release.SystemNetworking.ChartVersion}, "values": values}}
	conditions := []any{}
	for _, kind := range []string{"Ready", "ApplicationsReconciled", "ApplicationsReady"} {
		conditions = append(conditions, map[string]any{"type": kind, "status": "True", "observedGeneration": int64(1)})
	}
	object.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": conditions}
	selected, err := o.selectValidationSystemService([]unstructured.Unstructured{*object}, role, expected, release, nil)
	failure, ok := err.(*validationCheckError)
	if !ok || failure.state != validation.Unverified || failure.reason != validation.ReasonDependency || selected == nil {
		t.Fatalf("missing artifact asserted drift or lost independent identity: %#v", err)
	}
	defaults := map[string]any{"multus": map[string]any{"cniBinDir": "/opt/cni/bin"}}
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*object}, role, expected, release, defaults); err != nil {
		t.Fatal("verified default-equivalent producer omission was rejected")
	}
	values["multus"].(map[string]any)["mtu"] = "wrong"
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*object}, role, expected, release, defaults); err == nil {
		t.Fatal("qualified defaults weakened a concrete configuration contradiction")
	}
}

func TestValidationProducerRoleSpecificValues(t *testing.T) {
	o := validationResourceFixture(t)
	release, _ := releases.Resolve(o.desired.plan.Intent().Release)
	monitoring := systemArtifactValues(o.desired.plan.Intent(), release, systemArtifactRole{Component: "dpu-monitoring"}, false)
	enabled, found, err := unstructured.NestedBool(monitoring, "dpu-monitoring", "deployDPUManifests")
	if err != nil || !found || !enabled {
		t.Fatal("source-defined monitoring DPU mode missing")
	}
	role := systemArtifactRole{Component: "servicechainset-controller", Management: true}
	values := systemArtifactValues(o.desired.plan.Intent(), release, role, true)
	request, _, _ := unstructured.NestedString(values, role.Component, "serviceAccount", "labels", "svc.dpu.nvidia.com/credential-request-name")
	account, _, _ := unstructured.NestedString(values, role.Component, "serviceAccount", "name")
	suffix := systemClusterSuffix(o.desired.plan.Intent().Bootstrap.Tenant.Name, o.desired.plan.Intent().Bootstrap.Tenant.Namespace)
	if request != "servicesetcontroller-"+suffix || account != "servicechainset-controller-"+suffix {
		t.Fatal("source legacy request stem was conflated with account identity")
	}
}
func TestValidationApplicationStatusGenerationIsNotDesiredIdentity(t *testing.T) {
	before := validationObject("argoproj.io/v1alpha1", "Application", "selected", "system")
	before.SetUID("same")
	before.SetGeneration(4)
	before.Object["spec"] = map[string]any{"source": map[string]any{"targetRevision": "v26.8.0"}}
	after := before.DeepCopy()
	after.SetGeneration(8)
	after.SetResourceVersion("new")
	after.Object["status"] = map[string]any{"health": map[string]any{"status": "Healthy"}}
	if !sameValidationApplicationSource(before, after) {
		t.Fatal("status-only Argo generation advance changed desired identity")
	}
	if sameValidationSemantic(before, after) {
		t.Fatal("ordinary resource generation fence was globally weakened")
	}
	after.SetUID("replacement")
	if sameValidationApplicationSource(before, after) {
		t.Fatal("App replacement accepted")
	}
	after.SetUID(before.GetUID())
	_ = unstructured.SetNestedField(after.Object, "changed", "spec", "source", "targetRevision")
	if sameValidationApplicationSource(before, after) {
		t.Fatal("App source drift accepted")
	}
}
func TestValidationKSMConfigSemanticWhitespacePreservesFunctionalDrift(t *testing.T) {
	role := systemArtifactRole{Component: "kube-state-metrics", Management: true}
	wanted := validationObject("v1", "ConfigMap", "selected", "metrics")
	wanted.Object["data"] = map[string]any{"config.yaml": "spec:\n  resources: []\n  \n"}
	current := wanted.DeepCopy()
	current.Object["data"].(map[string]any)["config.yaml"] = "spec:\n  resources: []\n"
	if !validationSystemConfigMapDataMatches(role, wanted, current) {
		t.Fatal("equivalent trailing whitespace rejected")
	}
	current.Object["data"].(map[string]any)["config.yaml"] = "spec:\n  resources: [{kind: Wrong}]\n"
	if validationSystemConfigMapDataMatches(role, wanted, current) {
		t.Fatal("functional YAML drift accepted")
	}
	current.Object["data"].(map[string]any)["config.yaml"] = "spec: {}\nspec: {}\n"
	if validationSystemConfigMapDataMatches(role, wanted, current) {
		t.Fatal("duplicate YAML key accepted")
	}
	current = wanted.DeepCopy()
	current.Object["data"].(map[string]any)["unexpected"] = "value"
	if validationSystemConfigMapDataMatches(role, wanted, current) {
		t.Fatal("unreviewed payload key accepted")
	}
}
