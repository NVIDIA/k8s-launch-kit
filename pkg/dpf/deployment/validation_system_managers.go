// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// This finite functional contract is selected only for the retained v26.8 HBN
// renderer. Its process, credential and storage wiring follows the stable public
// f1b50e85122a3df6047708e41a8203f8cdce03a1 inventory/manifests and *_manifests.go,
// independently corroborated by the selected stable management observations.
// The operator chart records a different build commit: this is not a claim of
// byte-for-byte equivalence to that binary's unavailable embedded manifests.
func (o *validationObserver) verifyValidationDirectManager(ctx context.Context, management validationManagementReader, current *unstructured.Unstructured, role string) error {
	if ctx == nil || management == nil || o == nil || o.desired == nil {
		return fmt.Errorf("direct manager observation inputs unavailable")
	}
	expected, err := o.validationDirectManagerExpected(role)
	if err != nil {
		return err
	}
	if !directManagerSpecMatches(expected, current) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Direct manager process, environment, storage, credential wiring or retained configuration differs from the supported release contract.")
	}
	// All three controllers consume this config dynamically. Bind its current
	// content independently of the observed Deployment, without deriving desired
	// flags or mounts from the live manager itself.
	if err := management.VerifyIdentity(ctx); err != nil {
		return err
	}
	config, err := management.Get(ctx, o.desired.configObject.DeepCopy())
	if err != nil {
		return err
	}
	if !currentObjectIdentity(config, o.desired.configObject) || o.desired.verifier.Verify(o.desired.configObject, config) != nil {
		return validationFailure(validation.Failed, validation.ReasonHealth, "The direct manager's consumed DPFOperatorConfig differs from retained intent.")
	}
	if err := validationReady(o.desired.rules.ResourceReadiness(config)); err != nil {
		return err
	}
	return management.VerifyIdentity(ctx)
}

func (o *validationObserver) validationDirectManagerExpected(role string) (*unstructured.Unstructured, error) {
	intent := o.desired.plan.Intent()
	if intent.Release != "26.8.0" || intent.UseCase != releases.UseCaseHBNBGP {
		return nil, directManagerUnsupported()
	}
	release, err := releases.Resolve(intent.Release)
	if err != nil {
		return nil, err
	}
	config, err := o.desired.rules.DefaultedCopy(o.desired.configObject)
	if err != nil {
		return nil, err
	}
	mode, _, err := unstructured.NestedString(config.Object, "spec", "deploymentMode")
	if err != nil || mode != "zero-trust" {
		return nil, directManagerUnsupported()
	}
	name, component, binary := "", "", ""
	switch role {
	case "provisioning-controller":
		name = "dpf-provisioning-controller-manager"
		component = name
		binary = "/provisioning"
	case "dpuservice-controller":
		name = "dpuservice-controller-manager"
		component = name
		binary = "/dpuservice"
	case "kamaji-cluster-manager":
		name = "kamaji-cm-controller-manager"
		component = role
		binary = "/kamaji-cluster-manager"
	default:
		return nil, directManagerUnsupported()
	}
	options, _, err := unstructured.NestedMap(config.Object, "spec", directManagerConfigKey(role))
	if err != nil {
		return nil, err
	}
	if value, exists := options["image"]; exists && value != "" && value != release.Operator.Image {
		return nil, directManagerUnsupported()
	}
	if raw, exists := options["controller"]; exists {
		controller, ok := raw.(map[string]any)
		if !ok {
			return nil, directManagerUnsupported()
		}
		if image, exists := controller["image"]; exists && image != "" && image != release.Operator.Image {
			return nil, directManagerUnsupported()
		}
	}
	if disabled, exists := options["disable"]; exists && disabled != false {
		return nil, directManagerUnsupported()
	}
	manager := corev1.Container{Name: "manager", Image: release.Operator.Image, LivenessProbe: directManagerHTTPProbe("/healthz", 15, 20), ReadinessProbe: directManagerHTTPProbe("/readyz", 5, 10)}
	pod := directManagerPod(name, component)
	var init []corev1.Container
	volumes := []corev1.Volume{directManagerTokenVolume()}
	mounts := []corev1.VolumeMount{{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}}
	switch role {
	case "provisioning-controller":
		manager.Command = []string{binary}
		if err := o.projectProvisioningManager(&manager, &volumes, &mounts, &init, config, options, release); err != nil {
			return nil, err
		}
	case "dpuservice-controller":
		feature, err := o.directManagerConfigPortsGate()
		if err != nil {
			return nil, err
		}
		ready, err := directManagerBool(options, "disableDPUReadyTaints", false)
		if err != nil {
			return nil, err
		}
		host, err := directManagerBool(options, "disableHostNetworkReadyNoExecuteTaints", true)
		if err != nil {
			return nil, err
		}
		manager.Command = []string{binary, "--leader-elect"}
		manager.Args = []string{fmt.Sprintf("--disable-dpu-ready-taints=%t", ready), fmt.Sprintf("--disable-host-network-ready-noexecute-taints=%t", host), fmt.Sprintf("--feature-gates=ConfigPortsOverHighSpeed=%t", feature)}
		manager.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Privileged: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
		manager.Ports = directManagerPorts()
		volumes = append(volumes, corev1.Volume{Name: "helm-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}, directManagerSecretVolume("cert", "dpuservice-webhook-server-cert", false))
		mounts = append(mounts, corev1.VolumeMount{Name: "helm-cache", MountPath: "/.cache/helm"}, corev1.VolumeMount{Name: "cert", MountPath: "/tmp/k8s-webhook-server/serving-certs", ReadOnly: true})
		pod.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532))}
	case "kamaji-cluster-manager":
		image, err := directManagerSiblingImage(release, "dpf-keepalived")
		if err != nil {
			return nil, err
		}
		manager.Args = []string{"--leader-elect", "--keepalived-image=" + image}
		manager.Command = []string{binary}
		manager.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
		pod.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532))}
	}
	manager.Resources, err = directManagerExpectedResources(role, options)
	if err != nil {
		return nil, err
	}
	manager.VolumeMounts = mounts
	pod.Containers = []corev1.Container{manager}
	if role == "provisioning-controller" {
		registry, err := directManagerSiblingImage(release, "bfb-registry")
		if err != nil {
			return nil, err
		}
		pod.Containers = append(pod.Containers, corev1.Container{Name: "pldmunpack", Image: registry, Command: []string{"/usr/local/bin/pldmunpackserver"}, Args: []string{"--socket-path=/var/run/dpf/pldm-unpack.sock", "--script-path=/opt/pldm-unpack/fwpkg_unpack.py"}, LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-S", "/var/run/dpf/pldm-unpack.sock"}}}, InitialDelaySeconds: 5, PeriodSeconds: 30}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-S", "/var/run/dpf/pldm-unpack.sock"}}}, InitialDelaySeconds: 3, PeriodSeconds: 10}, Resources: directManagerResources("500m", "256Mi", "10m", "64Mi"), SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, VolumeMounts: []corev1.VolumeMount{{Name: "bfb-volume", MountPath: "/bfb"}, {Name: "pldm-unpack-socket", MountPath: "/var/run/dpf"}}})
	}
	pod.InitContainers = init
	pod.Volumes = volumes
	secrets, _, err := unstructured.NestedStringSlice(config.Object, "spec", "imagePullSecrets")
	if err != nil {
		return nil, err
	}
	for _, secret := range secrets {
		pod.ImagePullSecrets = append(pod.ImagePullSecrets, corev1.LocalObjectReference{Name: secret})
	}
	replicas := int64(2)
	if raw, found := options["replicas"]; found {
		var valid bool
		replicas, valid = numericInt64(raw)
		if !valid || replicas < 1 || replicas > 3 {
			return nil, directManagerUnsupported()
		}
	}
	labels := map[string]string{"app.kubernetes.io/part-of": component, dpfComponentLabel: component}
	selector := map[string]string{"app.kubernetes.io/part-of": component, dpfComponentLabel: component}
	object := appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.desired.operatorNamespace}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(replicas)), Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"kubectl.kubernetes.io/default-container": "manager"}}, Spec: pod}}}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&object)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: data}, nil
}

func (o *validationObserver) projectProvisioningManager(manager *corev1.Container, volumes *[]corev1.Volume, mounts *[]corev1.VolumeMount, init *[]corev1.Container, config *unstructured.Unstructured, options map[string]any, release releases.Release) error {
	for _, field := range []string{"bfCFGTemplateConfigMap", "customCASecretName"} {
		if value, found := options[field]; found && value != "" {
			return directManagerUnsupported()
		}
	}
	if dynamic, err := directManagerBool(options, "enableDynamicBFCFGTemplates", false); err != nil || dynamic {
		return directManagerUnsupported()
	}
	_, found, err := unstructured.NestedMap(options, "installInterface", "installViaRedfish")
	if err != nil || !found {
		return directManagerUnsupported()
	}
	dms, err := directManagerSiblingImage(release, "hostdriver")
	if err != nil {
		return err
	}
	registry, err := directManagerSiblingImage(release, "bfb-registry")
	if err != nil {
		return err
	}
	pvc, _ := options["bfbPVCName"].(string)
	manager.Args = []string{"--leader-elect", "--v=3", "--dms-image=" + dms, "--bfb-pvc=" + pvc, "--redfish-client-cert-dir=/etc/dpf/redfish-client-cert", "--dpu-install-interface=redfish", "--deployment-mode=zero-trust"}
	secrets, _, err := unstructured.NestedStringSlice(config.Object, "spec", "imagePullSecrets")
	if err != nil {
		return err
	}
	if len(secrets) > 0 {
		manager.Args = append(manager.Args, "--image-pull-secrets="+strings.Join(secrets, ","))
	}
	vip, _, err := unstructured.NestedString(config.Object, "spec", "overrides", "kubernetesAPIServerVIP")
	if err != nil {
		return err
	}
	port, found, err := unstructured.NestedInt64(config.Object, "spec", "overrides", "kubernetesAPIServerPort")
	if err != nil || !found || vip == "" {
		return directManagerUnsupported()
	}
	manager.Args = append(manager.Args, fmt.Sprintf("--dms-pod-envs=KUBERNETES_SERVICE_HOST=%s,KUBERNETES_SERVICE_PORT=%d", vip, port))
	for _, setting := range []struct{ field, flag string }{{"dmsTimeout", "dms-timeout"}, {"maxDPUParallelInstallations", "max-dpu-parallel-installations"}, {"maxUnavailableDPUNodes", "max-unavailable-dpu-nodes"}, {"osInstallRetries", "os-install-retries"}} {
		if value, found := options[setting.field]; found {
			integer, valid := numericInt64(value)
			if !valid || integer < 1 {
				return directManagerUnsupported()
			}
			manager.Args = append(manager.Args, fmt.Sprintf("--%s=%d", setting.flag, integer))
		}
	}
	for _, setting := range []struct{ field, flag string }{{"multiDPUOperationsSyncWaitTime", "multi-dpu-operations-sync-wait-time"}, {"osInstallTimeout", "os-install-timeout"}, {"firmwareUpdateTimeout", "firmware-update-timeout"}, {"preInstallAgentRegistrationTimeout", "pre-install-agent-registration-timeout"}, {"nodeEffectRemovalTimeout", "node-effect-removal-timeout"}} {
		if value, found := options[setting.field]; found {
			text, valid := value.(string)
			duration, err := time.ParseDuration(text)
			if !valid || err != nil || duration < 0 {
				return directManagerUnsupported()
			}
			manager.Args = append(manager.Args, "--"+setting.flag+"="+duration.String())
		}
	}
	if policy, found := options["hostAgentDNSPolicy"]; found {
		text, ok := policy.(string)
		if !ok {
			return directManagerUnsupported()
		}
		manager.Args = append(manager.Args, "--hostagent-dns-policy="+text)
	}
	address, _, err := unstructured.NestedString(options, "registry", "loadBalancerAddress")
	if err != nil {
		return err
	}
	manager.Args = append(manager.Args, "--bfb-registry-load-balancer-address="+address)
	manager.Env = []corev1.EnvVar{{Name: "GRPC_ENFORCE_ALPN_ENABLED", Value: "false"}, {Name: "PLDM_UNPACK_SOCKET_PATH", Value: "/var/run/dpf/pldm-unpack.sock"}, {Name: "PLDM_UNPACK_ENDPOINT", Value: "/v1/unpack"}}
	for _, value := range []struct{ name, path string }{{"NODE_NAME", "spec.nodeName"}, {"NODE_IP", "status.hostIP"}, {"POD_NAME", "metadata.name"}, {"POD_NAMESPACE", "metadata.namespace"}} {
		manager.Env = append(manager.Env, corev1.EnvVar{Name: value.name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: value.path}}})
	}
	manager.Env = append(manager.Env, corev1.EnvVar{Name: "BFB_REGISTRY_IMAGE", Value: registry})
	manager.Ports = directManagerPorts()
	manager.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true), RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	*volumes = append(*volumes, directManagerSecretVolume("cert", "webhook-server-cert", false), corev1.Volume{Name: "pldm-unpack-socket", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}, directManagerSecretVolume("redfish-client-cert", "dpf-provisioning-redfish-client-secret", true), directManagerSecretVolume("redfish-client-cert-bf4", "dpf-provisioning-redfish-client-secret-bf4", true))
	*mounts = append(*mounts, corev1.VolumeMount{Name: "cert", MountPath: "/tmp/k8s-webhook-server/serving-certs", ReadOnly: true}, corev1.VolumeMount{Name: "bfb-volume", MountPath: "/bfb"}, corev1.VolumeMount{Name: "pldm-unpack-socket", MountPath: "/var/run/dpf"}, corev1.VolumeMount{Name: "redfish-client-cert", MountPath: "/etc/dpf/redfish-client-cert", ReadOnly: true}, corev1.VolumeMount{Name: "redfish-client-cert-bf4", MountPath: "/etc/dpf/redfish-client-cert-bf4", ReadOnly: true})
	if pvc != "" {
		*volumes = append(*volumes, corev1.Volume{Name: "bfb-volume", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}}})
	} else {
		*volumes = append(*volumes, corev1.Volume{Name: "bfb-volume", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/nvidia/dpf/bfb", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
		*init = []corev1.Container{{Name: "prepare-local-storage", Image: "busybox:1.36", Command: []string{"sh", "-c", "mkdir -p /bfb && chown -R 65532:65532 /bfb"}, SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), Privileged: ptr.To(true)}, VolumeMounts: []corev1.VolumeMount{{Name: "bfb-volume", MountPath: "/bfb"}}}}
	}
	return nil
}

// API defaults are shared with the operator/system comparators. Only named
// lists whose order cannot change these fixed programs' meaning are canonical;
// duplicate environment names/flags are rejected rather than guessed last-wins.
func directManagerSpecMatches(expected, current *unstructured.Unstructured) bool {
	if expected == nil || current == nil || expected.GroupVersionKind() != current.GroupVersionKind() || expected.GetName() != current.GetName() || expected.GetNamespace() != current.GetNamespace() {
		return false
	}
	want, we := normalizedManagerSpec(expected)
	got, ge := normalizedManagerSpec(current)
	if we != nil || ge != nil {
		return false
	}
	for _, spec := range []*appsv1.DeploymentSpec{&want, &got} {
		normalizeSystemPod(&spec.Template.Spec)
		if !canonicalDirectManagerPod(&spec.Template.Spec) {
			return false
		}
	}
	return jsonEqual(want, got)
}

func canonicalDirectManagerPod(pod *corev1.PodSpec) bool {
	volumes := map[string]bool{}
	for _, volume := range pod.Volumes {
		if volumes[volume.Name] {
			return false
		}
		volumes[volume.Name] = true
	}
	slices.SortFunc(pod.Volumes, func(a, b corev1.Volume) int { return strings.Compare(a.Name, b.Name) })
	for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
		names := map[string]bool{}
		for i := range containers {
			container := &containers[i]
			if names[container.Name] {
				return false
			}
			names[container.Name] = true
			flags := map[string]bool{}
			for _, argument := range container.Args {
				key, _, _ := strings.Cut(argument, "=")
				if !strings.HasPrefix(key, "--") || flags[key] {
					return false
				}
				flags[key] = true
			}
			slices.Sort(container.Args)
			envNames := map[string]bool{}
			for _, env := range container.Env {
				if envNames[env.Name] {
					return false
				}
				envNames[env.Name] = true
			}
			slices.SortFunc(container.Env, func(a, b corev1.EnvVar) int { return strings.Compare(a.Name, b.Name) })
			slices.SortFunc(container.VolumeMounts, func(a, b corev1.VolumeMount) int {
				return strings.Compare(a.Name+"/"+a.MountPath, b.Name+"/"+b.MountPath)
			})
		}
	}
	slices.SortFunc(pod.Containers, func(a, b corev1.Container) int { return strings.Compare(a.Name, b.Name) })
	return true
}

func directManagerConfigKey(role string) string {
	switch role {
	case "provisioning-controller":
		return "provisioningController"
	case "dpuservice-controller":
		return "dpuServiceController"
	case "kamaji-cluster-manager":
		return "kamajiClusterManager"
	default:
		return ""
	}
}

func directManagerExpectedResources(role string, options map[string]any) (corev1.ResourceRequirements, error) {
	resourceMap, found, err := unstructured.NestedMap(options, "controller", "resources")
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if found {
		var resources corev1.ResourceRequirements
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resourceMap, &resources); err != nil {
			return resources, err
		}
		if len(resources.Limits) > 0 || len(resources.Requests) > 0 {
			return resources, nil
		}
	}
	switch role {
	case "provisioning-controller":
		return directManagerResources("2", "2Gi", "1", "1Gi"), nil
	case "kamaji-cluster-manager":
		return directManagerResources("500m", "512Mi", "10m", "64Mi"), nil
	default:
		return corev1.ResourceRequirements{}, nil
	}
}
func directManagerUnsupported() error {
	return validationFailure(validation.Unverified, validation.ReasonUnsupported, "The retained direct-manager configuration is outside the finite supported stable source contract.")
}
func directManagerBool(options map[string]any, key string, fallback bool) (bool, error) {
	value, found := options[key]
	if !found {
		return fallback, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, directManagerUnsupported()
	}
	return boolean, nil
}
func directManagerSiblingImage(release releases.Release, name string) (string, error) {
	suffix := "/dpf-system:" + release.Operator.Version
	if !strings.HasSuffix(release.Operator.Image, suffix) {
		return "", directManagerUnsupported()
	}
	return strings.TrimSuffix(release.Operator.Image, suffix) + "/" + name + ":" + release.Operator.Version, nil
}
func directManagerResources(cpuLimit, memoryLimit, cpuRequest, memoryRequest string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLimit), corev1.ResourceMemory: resource.MustParse(memoryLimit)}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuRequest), corev1.ResourceMemory: resource.MustParse(memoryRequest)}}
}
func directManagerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{{Name: "webhook-server", ContainerPort: 9443, Protocol: corev1.ProtocolTCP}, {Name: "metrics", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}}
}
func directManagerHTTPProbe(path string, delay, period int32) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(8081)}}, InitialDelaySeconds: delay, PeriodSeconds: period}
}
func directManagerSecretVolume(name, secret string, optional bool) corev1.Volume {
	source := &corev1.SecretVolumeSource{SecretName: secret}
	if optional {
		source.Optional = ptr.To(true)
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: source}}
}
func directManagerTokenVolume() corev1.Volume {
	return corev1.Volume{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}, {ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}}, {DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}}}}}}
}
func directManagerPod(name, component string) corev1.PodSpec {
	return corev1.PodSpec{ServiceAccountName: name, AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(10)), Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node-role.kubernetes.io/master", Operator: corev1.NodeSelectorOpExists}}}, {MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.NodeSelectorOpExists}}}}}}, PodAntiAffinity: &corev1.PodAntiAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/part-of": component, dpfComponentLabel: component}}}}}}}, Tolerations: []corev1.Toleration{{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}, {Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}}
}
func (o *validationObserver) directManagerConfigPortsGate() (bool, error) {
	containers, _, err := unstructured.NestedSlice(o.desired.manager.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return false, err
	}
	enabled := false
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			return false, directManagerUnsupported()
		}
		if container["name"] != "manager" {
			continue
		}
		args, _, err := unstructured.NestedStringSlice(container, "args")
		if err != nil {
			return false, err
		}
		seen := false
		for _, arg := range args {
			if !strings.HasPrefix(arg, "--feature-gates=") {
				continue
			}
			if seen {
				return false, directManagerUnsupported()
			}
			seen = true
			gates := map[string]bool{}
			for _, entry := range strings.Split(strings.TrimPrefix(arg, "--feature-gates="), ",") {
				key, value, found := strings.Cut(entry, "=")
				flag, err := strconv.ParseBool(value)
				if !found || err != nil || gates[key] {
					return false, directManagerUnsupported()
				}
				gates[key] = true
				switch key {
				case "ConfigPortsOverHighSpeed":
					enabled = flag
				case "NSIPathForSFC", "NSIPathForVPC":
				default:
					return false, directManagerUnsupported()
				}
			}
		}
	}
	return enabled, nil
}
