// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	sigsyaml "sigs.k8s.io/yaml"
)

// This current addon contract follows kubeadm's pinned v1.36.2 proxy manifest
// builder (the Kubernetes module embedded in the selected Kamaji image), plus
// the corroborated Kamaji ClusterRoleBinding controller anchor. It does not
// assert equivalence with the unavailable private fork reconciliation source.
func (o *validationObserver) observeValidationKubeProxy(ctx context.Context, management validationManagementReader, tenant *tenantClient, r *validationRecorder) error {
	if tenant == nil {
		return nil
	}
	returnErr := r.run("tenant/kube-proxy", func() error {
		expected, _, err := tenantPlanInputs(o.desired.plan)
		if err != nil {
			return err
		}
		sources, err := management.observeTenantSources(ctx, expected)
		if err != nil {
			return err
		}
		if sources.observed.TenantControlPlane == nil {
			return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected current TenantControlPlane was not observed.")
		}
		addon, found, err := unstructured.NestedMap(sources.observed.TenantControlPlane.Object, "spec", "addons", "kubeProxy")
		if err != nil || !found || len(addon) != 0 {
			return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Selected Kamaji kube-proxy addon is disabled or has unsupported overrides.")
		}
		reader := validationTenantResources{tenant: tenant}
		roleObject, err := reader.Get(ctx, validationObject("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "kubeadm:node-proxier"))
		if err != nil {
			return err
		}
		var binding rbacv1.ClusterRoleBinding
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(roleObject.Object, &binding); err != nil {
			return err
		}
		if err := validationKubeProxyBinding(&binding); err != nil {
			return err
		}
		accountObject, err := reader.Get(ctx, validationObject("v1", "ServiceAccount", metav1.NamespaceSystem, "kube-proxy"))
		if err != nil {
			return err
		}
		if !validationKubeProxyOwned(accountObject.GetOwnerReferences(), binding.UID) || accountObject.GetUID() == "" || accountObject.GetDeletionTimestamp() != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy ServiceAccount does not belong to the selected addon anchor.")
		}
		configObject, err := reader.Get(ctx, validationObject("v1", "ConfigMap", metav1.NamespaceSystem, "kube-proxy"))
		if err != nil {
			return err
		}
		var config corev1.ConfigMap
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(configObject.Object, &config); err != nil {
			return err
		}
		endpoint, _, err := tenant.bootstrapCredentialSource(ctx)
		if err != nil {
			return err
		}
		if err := validationKubeProxyConfiguration(&config, o.desired.plan.Intent().Bootstrap.PodCIDR, endpoint); err != nil {
			return err
		}
		current, err := reader.Get(ctx, validationObject("apps/v1", "DaemonSet", metav1.NamespaceSystem, "kube-proxy"))
		if err != nil {
			return err
		}
		if !validationKubeProxyOwned(current.GetOwnerReferences(), binding.UID) || current.GetUID() == "" || current.GetDeletionTimestamp() != nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy DaemonSet does not belong to the current addon anchor.")
		}
		expectedSet := validationExpectedKubeProxy(o.desired.plan.Intent().Bootstrap.TenantKubernetesVersion)
		if !validationSystemWorkloadMatches(expectedSet, current) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy image, command or functional Pod configuration differs from the selected addon recipe.")
		}
		set, err := validationDaemonSetFromObject(current)
		if err != nil {
			return err
		}
		nodes, err := o.validationSelectedTenantNodes(ctx, management, tenant)
		if err != nil {
			return err
		}
		if err := validationDaemonSetReady(set, len(nodes)); err != nil {
			return err
		}
		owned, err := validationKubeProxyOwnedPods(ctx, tenant, set, nodes)
		if err != nil {
			return err
		}
		var coverageErr error
		for serial := range nodes {
			err := r.run(validation.CheckID("tenant/kube-proxy/device/"+serial), func() error {
				pod := owned[serial]
				if pod == nil {
					return validationFailure(validation.Failed, validation.ReasonMissing, "Selected tenant Node requires exactly one current owned Ready kube-proxy Pod.")
				}
				if err := validationPodNetworkRequestsMatch(pod, &set.Spec.Template); err != nil {
					return err
				}
				// The selected Kamaji TCP explicitly enables AlwaysPullImages.
				if err := validationPodMatchesTemplate(pod, &set.Spec.Template.Spec, "DaemonSet", true); err != nil {
					return err
				}
				return validationPodReady(pod, &set.Spec.Template.Spec)
			})
			coverageErr = validationPodCoverageError(coverageErr, err)
		}
		if coverageErr != nil {
			return coverageErr
		}

		after, err := reader.Get(ctx, validationObject("apps/v1", "DaemonSet", metav1.NamespaceSystem, "kube-proxy"))
		if err != nil {
			return err
		}
		if !sameValidationSemantic(current, after) {
			return fmt.Errorf("kube-proxy changed: %w", errValidationResourceChanged)
		}
		return tenant.VerifyIdentity(ctx)
	})
	r.pendingDependents("tenant/kube-proxy/device/", "tenant/kube-proxy")
	if validationIdentityChanged(returnErr) {
		return returnErr
	}
	return nil
}
func validationKubeProxyBinding(binding *rbacv1.ClusterRoleBinding) error {
	want := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:node-proxier"}
	subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-proxy", Namespace: metav1.NamespaceSystem}}
	if binding.Name != "kubeadm:node-proxier" || binding.UID == "" || binding.ResourceVersion == "" || binding.DeletionTimestamp != nil || binding.RoleRef != want || !jsonEqual(binding.Subjects, subjects) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy current role binding does not grant the selected ServiceAccount the node-proxier role.")
	}
	return nil
}
func validationKubeProxyOwned(owners []metav1.OwnerReference, uid types.UID) bool {
	controllers := 0
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller {
			controllers++
			if owner.APIVersion != "rbac.authorization.k8s.io/v1" || owner.Kind != "ClusterRoleBinding" || owner.Name != "kubeadm:node-proxier" || owner.UID != uid {
				return false
			}
		}
	}
	return controllers == 1
}
func validationExpectedKubeProxy(version string) *unstructured.Unstructured {
	yes := true
	set := appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: metav1.NamespaceSystem}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-proxy"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-proxy"}}, Spec: corev1.PodSpec{PriorityClassName: "system-node-critical", HostNetwork: true, ServiceAccountName: "kube-proxy", NodeSelector: map[string]string{"kubernetes.io/os": "linux"}, Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, Containers: []corev1.Container{{Name: "kube-proxy", Image: "registry.k8s.io/kube-proxy:" + version, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/usr/local/bin/kube-proxy", "--config=/var/lib/kube-proxy/config.conf", "--hostname-override=$(NODE_NAME)"}, SecurityContext: &corev1.SecurityContext{Privileged: &yes}, Env: []corev1.EnvVar{{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}}, VolumeMounts: []corev1.VolumeMount{{Name: "kube-proxy", MountPath: "/var/lib/kube-proxy"}, {Name: "xtables-lock", MountPath: "/run/xtables.lock"}, {Name: "lib-modules", MountPath: "/lib/modules", ReadOnly: true}}}}, Volumes: []corev1.Volume{{Name: "kube-proxy", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-proxy"}}}}, {Name: "xtables-lock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/xtables.lock", Type: ptr.To(corev1.HostPathFileOrCreate)}}}, {Name: "lib-modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"}}}}}}}}
	object, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&set)
	return &unstructured.Unstructured{Object: object}
}
func validationKubeProxyConfiguration(config *corev1.ConfigMap, podCIDR, endpoint string) error {
	if config.Name != "kube-proxy" || config.Namespace != metav1.NamespaceSystem || config.UID == "" || config.ResourceVersion == "" || config.DeletionTimestamp != nil || len(config.Data) != 2 {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy configuration identity or key coverage differs.")
	}
	raw := config.Data["config.conf"]
	var settings map[string]any
	if len(raw) > 65536 || sigsyaml.UnmarshalStrict([]byte(raw), &settings) != nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Kube-proxy configuration could not be decoded completely.")
	}
	if !validationProxyConfigShape(settings, "") {
		return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Kube-proxy configuration contains fields outside the pinned component schema.")
	}
	// The managed recipe serializes zero component settings and lets kube-proxy
	// default them. Nonzero tuning or feature overrides are unsupported here.
	for key, value := range settings {
		switch key {
		case "apiVersion":
			if value != "kubeproxy.config.k8s.io/v1alpha1" {
				return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Kube-proxy configuration API is unsupported.")
			}
		case "kind":
			if value != "KubeProxyConfiguration" {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy configuration kind differs.")
			}
		case "bindAddress":
			if value != "0.0.0.0" {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy IPv4 bind address differs.")
			}
		case "clusterCIDR":
			if value == "" {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy clusterCIDR is empty; CIDR-based source NAT classification for off-cluster Service traffic is disabled. Use a supported Kamaji release/backport fixing upstream #1291, then verify reconciled configuration and running Pods.")
			}
			if value != podCIDR {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy cluster CIDR differs from retained tenant networking.")
			}
		case "clientConnection":
			connection, ok := value.(map[string]any)
			if !ok || connection["kubeconfig"] != "/var/lib/kube-proxy/kubeconfig.conf" {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy consumes an unexpected client configuration.")
			}
			copy := map[string]any{}
			for k, v := range connection {
				if k != "kubeconfig" {
					copy[k] = v
				}
			}
			if !validationZeroProxyValue(copy) {
				return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Kube-proxy client tuning is outside the selected recipe.")
			}
		default:
			if !validationZeroProxyValue(value) {
				return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Kube-proxy has unsupported nondefault functional settings.")
			}
		}
	}
	for _, key := range []string{"apiVersion", "kind", "bindAddress", "clusterCIDR", "clientConnection"} {
		if _, ok := settings[key]; !ok {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "Kube-proxy configuration omits a required selected field.")
		}
	}
	client, err := clientcmd.Load([]byte(config.Data["kubeconfig.conf"]))
	if err != nil || len(client.Clusters) != 1 || len(client.Contexts) != 1 || len(client.AuthInfos) != 1 {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy kubeconfig identity coverage differs.")
	}
	expectedClient := map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": []any{map[string]any{"name": "default", "cluster": map[string]any{"server": endpoint, "certificate-authority": "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"}}}, "contexts": []any{map[string]any{"name": "default", "context": map[string]any{"cluster": "default", "namespace": "default", "user": "default"}}}, "current-context": "default", "users": []any{map[string]any{"name": "default", "user": map[string]any{"tokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token"}}}}
	var rawClient map[string]any
	if sigsyaml.UnmarshalStrict([]byte(config.Data["kubeconfig.conf"]), &rawClient) != nil || !jsonEqual(rawClient, expectedClient) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy kubeconfig contains unsupported fields or differs from the selected endpoint and credential recipe.")
	}
	expectedBytes, err := json.Marshal(expectedClient)
	if err != nil {
		return err
	}
	wanted, err := clientcmd.Load(expectedBytes)
	if err != nil {
		return err
	}
	if !jsonEqual(client, wanted) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Kube-proxy client endpoint, trust, auth or context differs from the bound tenant's kubeadm recipe.")
	}

	return nil
}
func validationZeroProxyValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case float64:
		return value == 0
	case string:
		return value == "" || value == "0" || value == "0s"
	case []any:
		return len(value) == 0
	case map[string]any:
		for _, v := range value {
			if !validationZeroProxyValue(v) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validationProxyConfigShape(settings map[string]any, scope string) bool {
	fields := map[string]string{
		"":                     "apiVersion kind bindAddress bindAddressHardFail clientConnection clusterCIDR configSyncPeriod conntrack detectLocal detectLocalMode enableProfiling featureGates healthzBindAddress hostnameOverride iptables ipvs logging metricsBindAddress mode nftables nodePortAddresses oomScoreAdj portRange showHiddenMetricsForVersion winkernel",
		"clientConnection":     "acceptContentTypes burst contentType kubeconfig qps",
		"conntrack":            "maxPerCore min tcpBeLiberal tcpCloseWaitTimeout tcpEstablishedTimeout udpStreamTimeout udpTimeout",
		"detectLocal":          "bridgeInterface interfaceNamePrefix",
		"iptables":             "localhostNodePorts masqueradeAll masqueradeBit minSyncPeriod syncPeriod",
		"ipvs":                 "excludeCIDRs minSyncPeriod scheduler strictARP syncPeriod tcpFinTimeout tcpTimeout udpTimeout",
		"nftables":             "masqueradeAll masqueradeBit minSyncPeriod syncPeriod",
		"logging":              "flushFrequency format options verbosity vmodule",
		"logging.options":      "json text",
		"logging.options.json": "infoBufferSize splitStream",
		"logging.options.text": "infoBufferSize splitStream",
		"winkernel":            "enableDSR forwardHealthCheckVip networkName rootHnsEndpointName sourceVip",
		"featureGates":         "",
	}
	allowed, ok := fields[scope]
	if !ok {
		return false
	}
	for key, value := range settings {
		if !strings.Contains(" "+allowed+" ", " "+key+" ") {
			return false
		}
		if nested, ok := value.(map[string]any); ok {
			next := key
			if scope != "" {
				next = scope + "." + key
			}
			if !validationProxyConfigShape(nested, next) {
				return false
			}
		}
	}
	return true
}

func validationKubeProxyOwnedPods(ctx context.Context, tenant *tenantClient, set *appsv1.DaemonSet, nodes map[string]corev1.Node) (map[string]*corev1.Pod, error) {
	// A selector-filtered API read would hide contradictory exact-owner Pods.
	_, pods, err := tenant.listRuntimeObjects(ctx, metav1.NamespaceSystem, "")
	if err != nil {
		return nil, err
	}
	return validationDaemonSetPodMembership(set, pods, nodes)
}
