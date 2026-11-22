// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	"encoding/json"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	kptr "k8s.io/utils/ptr"
)

const kamajiLabel = "app.kubernetes.io/name=kamaji"

// KamajiCompatibilityResult is a bounded conclusion about the current external
// producer, not proof of tenant addon correctness or runtime qualification.
type KamajiCompatibilityResult struct {
	State      string `json:"state" yaml:"state"`
	ReasonCode string `json:"reasonCode" yaml:"reasonCode"`
	Message    string `json:"message" yaml:"message"`
	Release    string `json:"release" yaml:"release"`
	Namespace  string `json:"namespace" yaml:"namespace"`
}

// CollectKamajiCompatibility resolves the trusted release rule on every call.
// Saved inventory, arbitrary image tables and readiness-only observations cannot
// authorize this prerequisite. Both complete reads must agree on semantic state.
func (c Collector) CollectKamajiCompatibility(ctx context.Context, namespace, releaseVersion string) KamajiCompatibilityResult {
	result := KamajiCompatibilityResult{State: "unknown", ReasonCode: "KAMAJI_COMPATIBILITY_UNOBSERVED", Message: "The current Kamaji producer has not been verified.", Release: releaseVersion, Namespace: namespace}
	fail := func(code, message string) KamajiCompatibilityResult {
		result.ReasonCode, result.Message = code, message
		return result
	}
	if ctx == nil || ctx.Err() != nil {
		return fail("KAMAJI_COMPATIBILITY_CANCELED", "Kamaji producer observation was canceled or timed out.")
	}
	if namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 || releaseVersion == "" {
		return fail("KAMAJI_COMPATIBILITY_INPUT_INVALID", "An explicit Kamaji namespace and selected release are required.")
	}
	release, err := releases.Resolve(releaseVersion)
	if err != nil || !release.SupportsUseCase(releases.UseCaseHBNBGP) || release.KamajiCompatibility == nil {
		return fail("KAMAJI_COMPATIBILITY_CONTRACT_MISSING", "The selected release has no reviewed HBN Kamaji compatibility exception.")
	}
	if c.Kubernetes == nil {
		return fail("KAMAJI_COMPATIBILITY_CLIENT_UNAVAILABLE", "The management Kubernetes client is unavailable.")
	}
	first, code, message := c.readKamajiCompatibility(ctx, namespace, *release.KamajiCompatibility)
	if code != "" {
		return fail(code, message)
	}
	second, code, message := c.readKamajiCompatibility(ctx, namespace, *release.KamajiCompatibility)
	if code != "" {
		return fail(code, message)
	}
	if ctx.Err() != nil {
		return fail("KAMAJI_COMPATIBILITY_CANCELED", "Kamaji producer observation was canceled or timed out.")
	}
	if first != second {
		return fail("KAMAJI_COMPATIBILITY_CHANGED", "Kamaji identity, desired process, ownership or current Pod set changed during observation; retry after a stable rollout.")
	}
	result.State, result.ReasonCode = "passed", "KAMAJI_COMPATIBILITY_VERIFIED"
	result.Message = "The current Ready linux/amd64 Kamaji manager matches the release-owned immutable upstream edge exception. This does not verify tenant addon configuration or NVIDIA vendor GA support."
	return result
}

func (c Collector) readKamajiCompatibility(ctx context.Context, namespace string, rule releases.KamajiCompatibility) (string, string, string) {
	invalid := func(code, message string) (string, string, string) { return "", code, message }
	var issues []Issue
	deployments, coverage := boundedPages(ctx, "kamaji-deployments", &issues, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		options.LabelSelector = kamajiLabel
		page, err := c.Kubernetes.AppsV1().Deployments(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if coverage != CollectionComplete {
		return invalid("KAMAJI_COMPATIBILITY_READ_INCOMPLETE", "Complete access to scoped Kamaji Deployments is required; check list permissions and API availability.")
	}
	if len(deployments) != 1 {
		return invalid("KAMAJI_COMPATIBILITY_AMBIGUOUS", "Exactly one scoped Kamaji Deployment is required.")
	}
	d := &deployments[0]
	if d.Namespace != namespace || d.UID == "" || d.Generation < 1 || d.DeletionTimestamp != nil || d.Spec.Selector == nil {
		return invalid("KAMAJI_COMPATIBILITY_IDENTITY_INVALID", "Kamaji Deployment identity is missing, deleting or malformed.")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(d.Spec.Template.Labels)) || d.Spec.Template.Labels["app.kubernetes.io/name"] != "kamaji" {
		return invalid("KAMAJI_COMPATIBILITY_SELECTOR_INVALID", "Kamaji Deployment selector and manager template do not agree.")
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || *d.Spec.Replicas > 16 {
		return invalid("KAMAJI_COMPATIBILITY_ROLLOUT_INVALID", "Kamaji replica count is missing or outside the supported bound.")
	}
	n := *d.Spec.Replicas
	if d.Status.ObservedGeneration != d.Generation || d.Status.Replicas != n || d.Status.UpdatedReplicas != n || d.Status.ReadyReplicas != n || d.Status.AvailableReplicas != n || d.Status.UnavailableReplicas != 0 {
		return invalid("KAMAJI_COMPATIBILITY_ROLLOUT_PENDING", "Kamaji must have a complete current Ready rollout before provisioning.")
	}
	_, ok := kamajiManager(d.Spec.Template.Spec, rule)
	if !ok {
		return invalid("KAMAJI_COMPATIBILITY_PROCESS_UNSUPPORTED", "The named manager must run the exact release-owned digest and reviewed manager command/arguments; install the external prerequisite explicitly.")
	}
	sets, coverage := boundedPages(ctx, "kamaji-replicasets", &issues, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
		page, err := c.Kubernetes.AppsV1().ReplicaSets(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if coverage != CollectionComplete {
		return invalid("KAMAJI_COMPATIBILITY_READ_INCOMPLETE", "Complete scoped ReplicaSet list access is required.")
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
	var current *appsv1.ReplicaSet
	owned := map[string]bool{}
	signature := []any{d.UID, d.Generation, d.Spec, d.Status.ObservedGeneration, n}
	for i := range sets {
		rs := &sets[i]
		if !kamajiOwner(rs.OwnerReferences, "Deployment", d.Name, string(d.UID)) {
			continue
		}
		if rs.UID == "" || rs.Namespace != namespace || rs.DeletionTimestamp != nil || rs.Spec.Replicas == nil {
			return invalid("KAMAJI_COMPATIBILITY_REPLICASET_INVALID", "An owned Kamaji ReplicaSet has invalid identity or is deleting.")
		}
		owned[string(rs.UID)] = true
		signature = append(signature, []any{rs.UID, rs.Generation, rs.OwnerReferences, rs.Spec, rs.Status.ObservedGeneration, rs.Status.Replicas, rs.Status.ReadyReplicas, rs.Status.AvailableReplicas})
		if *rs.Spec.Replicas == 0 && rs.Status.Replicas == 0 && rs.Status.ReadyReplicas == 0 && rs.Status.AvailableReplicas == 0 {
			continue
		}
		if current != nil || *rs.Spec.Replicas != n || rs.Status.ObservedGeneration != rs.Generation || rs.Status.Replicas != n || rs.Status.ReadyReplicas != n || rs.Status.AvailableReplicas != n || !kamajiTemplateEqual(rs.Spec.Template, d.Spec.Template) {
			return invalid("KAMAJI_COMPATIBILITY_MIXED_ROLLOUT", "Kamaji has mixed, stale or incomplete ReplicaSets.")
		}
		current = rs
	}
	if current == nil {
		return invalid("KAMAJI_COMPATIBILITY_REPLICASET_MISSING", "The current Deployment-owned Kamaji ReplicaSet was not observed.")
	}
	pods, coverage := boundedPages(ctx, "kamaji-pods", &issues, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Pod, string, error) {
		page, err := c.Kubernetes.CoreV1().Pods(namespace).List(ctx, options)
		if err != nil {
			return nil, "", err
		}
		return page.Items, page.Continue, nil
	})
	if coverage != CollectionComplete {
		return invalid("KAMAJI_COMPATIBILITY_READ_INCOMPLETE", "Complete scoped manager Pod list access is required.")
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	count := int32(0)
	for i := range pods {
		pod := &pods[i]
		relevant := selector.Matches(labels.Set(pod.Labels))
		for _, owner := range pod.OwnerReferences {
			if owned[string(owner.UID)] {
				relevant = true
			}
		}
		if !relevant {
			continue
		}
		if pod.UID == "" || pod.Namespace != namespace || pod.DeletionTimestamp != nil || !kamajiOwner(pod.OwnerReferences, "ReplicaSet", current.Name, string(current.UID)) || !selector.Matches(labels.Set(pod.Labels)) || pod.Status.Phase != corev1.PodRunning || !kamajiPodReady(pod) {
			return invalid("KAMAJI_COMPATIBILITY_PODS_NOT_CURRENT", "Every selected Kamaji Pod must be current, Ready and owned by the current ReplicaSet; old or foreign Pods block provisioning.")
		}
		_, valid := kamajiManager(pod.Spec, rule)
		if !valid || !kamajiPodProcessMatches(pod.Spec, d.Spec.Template.Spec) {
			return invalid("KAMAJI_COMPATIBILITY_PROCESS_UNSUPPORTED", "A current manager Pod differs from the reviewed Deployment process.")
		}
		if !kamajiRuntimeImage(pod, rule) {
			return invalid("KAMAJI_COMPATIBILITY_RUNTIME_UNVERIFIED", "The named running manager imageID does not identify the reviewed index, linux/amd64 manifest or image config.")
		}
		if pod.Spec.NodeName == "" {
			return invalid("KAMAJI_COMPATIBILITY_PLATFORM_UNVERIFIED", "The running manager has no bound management node.")
		}
		node, err := c.Kubernetes.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil || node == nil || node.UID == "" || node.DeletionTimestamp != nil || node.Status.NodeInfo.OperatingSystem != "linux" || node.Status.NodeInfo.Architecture != "amd64" {
			return invalid("KAMAJI_COMPATIBILITY_PLATFORM_UNVERIFIED", "Only the reviewed linux/amd64 manager binary is covered; node identity and platform read access are required.")
		}
		signature = append(signature, []any{pod.UID, pod.Name, pod.OwnerReferences, pod.Labels, pod.Spec, pod.Status.Phase, pod.Status.ContainerStatuses, node.UID, node.Name, node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture})
		count++
	}
	if count != n {
		return invalid("KAMAJI_COMPATIBILITY_PODS_NOT_CURRENT", "The complete current manager Pod count does not match the Deployment rollout.")
	}
	encoded, _ := json.Marshal(signature)
	return string(encoded), "", ""
}

func kamajiOwner(owners []metav1.OwnerReference, kind, name, uid string) bool {
	controllers := 0
	matched := false
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller {
			controllers++
			matched = owner.APIVersion == "apps/v1" && owner.Kind == kind && owner.Name == name && string(owner.UID) == uid
		}
	}
	return controllers == 1 && matched
}
func kamajiTemplateEqual(a, b corev1.PodTemplateSpec) bool {
	a = *a.DeepCopy()
	b = *b.DeepCopy()
	delete(a.Labels, "pod-template-hash")
	delete(b.Labels, "pod-template-hash")
	return reflect.DeepEqual(a, b)
}
func kamajiManager(spec corev1.PodSpec, rule releases.KamajiCompatibility) (corev1.Container, bool) {
	var manager corev1.Container
	count := 0
	for _, container := range spec.Containers {
		if container.Name == "manager" {
			manager = container
			count++
		}
	}
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 || count != 1 || manager.Image != rule.Image || len(manager.EnvFrom) != 0 || spec.ServiceAccountName == "" {
		return manager, false
	}
	if len(manager.Command) > 0 && !reflect.DeepEqual(manager.Command, []string{"/ko-app/kamaji"}) {
		return manager, false
	}
	expected := []string{"manager", "--health-probe-bind-address=:8081", "--leader-elect", "--metrics-bind-address=:8080", "--tmp-directory=/tmp/kamaji", "--datastore=default"}
	if !reflect.DeepEqual(manager.Args, expected) {
		return manager, false
	}
	if len(manager.Env) != 2 {
		return manager, false
	}
	env := map[string]string{"POD_NAMESPACE": "metadata.namespace", "SERVICE_ACCOUNT": "spec.serviceAccountName"}
	for _, v := range manager.Env {
		field, ok := env[v.Name]
		if !ok || v.Value != "" || v.ValueFrom == nil || v.ValueFrom.FieldRef == nil || v.ValueFrom.FieldRef.FieldPath != field {
			return manager, false
		}
		delete(env, v.Name)
	}
	if len(env) != 0 || manager.ReadinessProbe == nil || manager.LivenessProbe == nil {
		return manager, false
	}
	for _, mount := range manager.VolumeMounts {
		mountPath := path.Clean(mount.MountPath)
		if mountPath == "/" || mountPath == "/ko-app" || strings.HasPrefix(mountPath, "/ko-app/") {
			return manager, false
		}
	}
	return manager, true
}
func kamajiPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
func kamajiRuntimeImage(pod *corev1.Pod, rule releases.KamajiCompatibility) bool {
	count := 0
	valid := false
	index := strings.Split(rule.Image, "@")[1]
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "manager" {
			continue
		}
		count++
		if !status.Ready || status.State.Running == nil || status.ContainerID == "" {
			continue
		}
		id := status.ImageID
		for _, prefix := range []string{"docker-pullable://", "containerd://", "docker://", "cri-o://"} {
			id = strings.TrimPrefix(id, prefix)
		}
		if strings.HasPrefix(id, "docker.io/clastix/kamaji@") {
			id = strings.TrimPrefix(id, "docker.io/clastix/kamaji@")
		} else if strings.Contains(id, "/") {
			continue
		}
		valid = id == index || id == rule.PlatformManifest || id == rule.ConfigDigest
	}
	return count == 1 && valid
}

// The observed management admission contract adds only a standard bound API
// token, the two default eviction tolerations, and ordinary scheduling defaults.
// Compare the entire functional PodSpec; stable but unreviewed security, DNS,
// networking, lifecycle or process changes must never disappear in normalization.
func kamajiPodProcessMatches(pod, template corev1.PodSpec) bool {
	got, want := pod.DeepCopy(), template.DeepCopy()
	if got.NodeName == "" || want.NodeName != "" && want.NodeName != got.NodeName {
		return false
	}
	want.NodeName = got.NodeName // The caller independently verifies this Node.
	if !kamajiTokenAdmission(got, want) {
		return false
	}
	for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
		covered := false
		for _, tolerance := range want.Tolerations {
			if (tolerance.Key == key || tolerance.Key == "") && (tolerance.Effect == corev1.TaintEffectNoExecute || tolerance.Effect == "") {
				covered = true
			}
		}
		if !covered {
			want.Tolerations = append(want.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: kptr.To(int64(300))})
		}
	}
	for _, spec := range []*corev1.PodSpec{want, got} {
		if spec.DeprecatedServiceAccount == "" {
			spec.DeprecatedServiceAccount = spec.ServiceAccountName
		}
		if spec.EnableServiceLinks == nil {
			spec.EnableServiceLinks = kptr.To(true)
		}
		// Only the observed built-in default priority is qualified. A custom
		// PriorityClass cannot supply an unobserved admission value.
		if spec.PriorityClassName != "" || spec.Priority != nil && *spec.Priority != 0 || spec.PreemptionPolicy != nil && *spec.PreemptionPolicy != corev1.PreemptLowerPriority {
			return false
		}
		if spec.Priority == nil {
			spec.Priority = kptr.To(int32(0))
		}
		if spec.PreemptionPolicy == nil {
			spec.PreemptionPolicy = kptr.To(corev1.PreemptLowerPriority)
		}
	}
	return reflect.DeepEqual(want, got)
}

var kamajiTokenVolumeName = regexp.MustCompile(`^kube-api-access-[a-z0-9]{5}$`)

// Add exactly the observed Kubernetes ServiceAccount admission projection to
// desired state. Matching only a few union fields could hide alternate sources,
// permission modes or extra mounts, so validate the complete volume and mount.
func kamajiTokenAdmission(got, want *corev1.PodSpec) bool {
	existing := make(map[string]bool, len(want.Volumes))
	for _, volume := range want.Volumes {
		if existing[volume.Name] {
			return false
		}
		existing[volume.Name] = true
	}
	var tokenName string
	for _, volume := range got.Volumes {
		if existing[volume.Name] {
			continue
		}
		if tokenName != "" || !kamajiTokenVolumeName.MatchString(volume.Name) || want.AutomountServiceAccountToken != nil && !*want.AutomountServiceAccountToken {
			return false
		}
		if !reflect.DeepEqual(volume, kamajiTokenVolume(volume.Name)) {
			return false
		}
		tokenName = volume.Name
	}
	if tokenName == "" {
		return true // No transformation to waive.
	}
	want.Volumes = append(want.Volumes, kamajiTokenVolume(tokenName))
	want.Containers[0].VolumeMounts = append(want.Containers[0].VolumeMounts, corev1.VolumeMount{Name: tokenName, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	return true
}

func kamajiTokenVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: kptr.To(int32(420)), Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: kptr.To(int64(3607))}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
	}}}}
}
