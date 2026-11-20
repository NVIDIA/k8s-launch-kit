// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"regexp"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const validationServiceAccountMount = "/var/run/secrets/kubernetes.io/serviceaccount"

var validationTokenVolumeName = regexp.MustCompile(`^kube-api-access-[a-z0-9]{5}$`)

// Source contract: Kubernetes v1.35.6 pkg/apis/core/v1/defaults.go,
// pkg/controller/daemon/util/daemonset_util.go, and the ServiceAccount,
// DefaultTolerationSeconds and Priority admission plugins. The selected managed
// recipes use built-in priorities and default eviction durations only.
//
// validationPodMatchesTemplate compares the complete admitted function against
// an independently qualified controller template. The caller binds the exact
// current owner and Node; their current names are the only dynamic identities
// used here. This is the supported Kubernetes admission contract, not a waiver
// for arbitrary admission webhooks. Unknown functional additions remain visible.
//
// alwaysPullImages must follow the independently selected control-plane recipe:
// the retained Kamaji tenant enables that admission plugin; management callers
// do not infer it from an observed image pull policy.
func validationPodMatchesTemplate(pod *corev1.Pod, template *corev1.PodSpec, controllerKind string, alwaysPullImages bool) error {
	if pod == nil || template == nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected Pod or independently qualified template is unavailable.")
	}
	if pod.Spec.NodeName == "" {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "Selected Pod has not been bound to its Node.")
	}
	want, got := template.DeepCopy(), pod.Spec.DeepCopy()
	switch controllerKind {
	case "Deployment", "ReplicaSet":
	case "DaemonSet":
		// The daemon controller replaces required Node affinity with the bound
		// metadata.name. Preferred affinity and all other affinity stay intact.
		if want.Affinity == nil {
			want.Affinity = &corev1.Affinity{}
		}
		if want.Affinity.NodeAffinity == nil {
			want.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		want.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: metav1.ObjectNameField, Operator: corev1.NodeSelectorOpIn, Values: []string{pod.Spec.NodeName}}}}}}
		for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
			validationDaemonToleration(want, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute})
		}
		for _, key := range []string{corev1.TaintNodeDiskPressure, corev1.TaintNodeMemoryPressure, corev1.TaintNodePIDPressure, corev1.TaintNodeUnschedulable} {
			validationDaemonToleration(want, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		}
		if want.HostNetwork {
			validationDaemonToleration(want, corev1.Toleration{Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		}
	default:
		return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Selected Pod controller has no supported admission contract.")
	}
	if want.NodeName != "" && want.NodeName != pod.Spec.NodeName {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected Pod Node binding differs from its controller template.")
	}
	want.NodeName = pod.Spec.NodeName
	if err := validationPodPriority(want); err != nil {
		return err
	}
	validationDefaultEvictionTolerations(want)
	admissionErr := validationPodTokenAdmission(want, got)
	for _, spec := range []*corev1.PodSpec{want, got} {
		normalizeManagerPodTemplate(spec)
		normalizeSystemPod(spec)
		if spec.ServiceAccountName == "" {
			spec.ServiceAccountName = "default"
		}
		if spec.DeprecatedServiceAccount == "" {
			spec.DeprecatedServiceAccount = spec.ServiceAccountName
		}
		if spec.EnableServiceLinks == nil {
			spec.EnableServiceLinks = ptr.To(true)
		}
		if spec.PreemptionPolicy == nil {
			spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
		}
		if spec.HostNetwork {
			for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
				for i := range containers {
					for j := range containers[i].Ports {
						if containers[i].Ports[j].HostPort == 0 {
							containers[i].Ports[j].HostPort = containers[i].Ports[j].ContainerPort
						}
					}
				}
			}
		}
	}
	if alwaysPullImages {
		// AlwaysPullImages is a known selected tenant admission transformation;
		// it cannot justify Never->Always or any reverse weakening.
		for _, containers := range [][]corev1.Container{want.Containers, want.InitContainers} {
			for i := range containers {
				if containers[i].ImagePullPolicy == corev1.PullIfNotPresent {
					containers[i].ImagePullPolicy = corev1.PullAlways
				}
			}
		}
	}
	if !jsonEqual(want, got) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected admitted Pod function differs from its independently verified controller template and supported admission defaults.")
	}
	return admissionErr
}

// Kubernetes daemon/util.AddOrUpdateDaemonPodTolerations replaces an exact
// key/operator/value/effect match (including its duration), otherwise appends.
func validationDaemonToleration(spec *corev1.PodSpec, addition corev1.Toleration) {
	for i := range spec.Tolerations {
		if spec.Tolerations[i].MatchToleration(&addition) {
			spec.Tolerations[i] = addition
			return
		}
	}
	spec.Tolerations = append(spec.Tolerations, addition)
}

func validationDefaultEvictionTolerations(spec *corev1.PodSpec) {
	for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
		found := false
		for _, toleration := range spec.Tolerations {
			if (toleration.Key == key || toleration.Key == "") && (toleration.Effect == corev1.TaintEffectNoExecute || toleration.Effect == "") {
				found = true
			}
		}
		if !found {
			spec.Tolerations = append(spec.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
		}
	}
}

func validationPodPriority(spec *corev1.PodSpec) error {
	if spec.Priority != nil {
		return nil
	}
	priority := int32(0)
	switch spec.PriorityClassName {
	case "":
	case "system-cluster-critical":
		priority = 2000000000
	case "system-node-critical":
		priority = 2000001000
	default:
		return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Selected Pod uses a custom PriorityClass without an independently qualified admission value.")
	}
	spec.Priority = &priority
	return nil
}

func validationPodTokenAdmission(want, got *corev1.PodSpec) error {
	if want.AutomountServiceAccountToken != nil && !*want.AutomountServiceAccountToken {
		return nil
	}
	needsMount := false
	for _, containers := range [][]corev1.Container{want.Containers, want.InitContainers} {
		for _, container := range containers {
			mounted := false
			for _, mount := range container.VolumeMounts {
				if mount.MountPath == validationServiceAccountMount {
					mounted = true
				}
			}
			needsMount = needsMount || !mounted
		}
	}
	if !needsMount {
		return nil
	}
	existing := map[string]bool{}
	for _, volume := range want.Volumes {
		existing[volume.Name] = true
	}
	var name string
	for _, volume := range got.Volumes {
		if existing[volume.Name] || !validationTokenVolumeName.MatchString(volume.Name) {
			continue
		}
		if name != "" || !jsonEqual(volume, validationPodTokenVolume(volume.Name)) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Selected Pod has an unexpected automatic service-account credential projection.")
		}
		name = volume.Name
	}
	if name == "" {
		if want.AutomountServiceAccountToken != nil && *want.AutomountServiceAccountToken {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Selected Pod explicitly requests automatic credentials but its admitted bound token projection is missing.")
		}
		// ServiceAccount policy can also disable automount. Without that
		// independent policy source, absence cannot establish credential health.
		return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Selected Pod automatic credential mount is absent and its ServiceAccount admission policy is not independently qualified.")
	}
	want.Volumes = append(want.Volumes, validationPodTokenVolume(name))
	for _, containers := range [][]corev1.Container{want.Containers, want.InitContainers} {
		for i := range containers {
			mounted := false
			for _, mount := range containers[i].VolumeMounts {
				if mount.MountPath == validationServiceAccountMount {
					mounted = true
				}
			}
			if !mounted {
				containers[i].VolumeMounts = append(containers[i].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: validationServiceAccountMount, ReadOnly: true})
			}
		}
	}
	return nil
}

func validationPodTokenVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(420)), Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
	}}}}
}

// Requested networks are functional input; network-status is observed CNI output
// and cannot become desired state. HBN has its own equivalent binding check.
func validationPodNetworkRequestsMatch(pod *corev1.Pod, template *corev1.PodTemplateSpec) error {
	if pod == nil || template == nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected Pod network request or template is unavailable.")
	}
	for _, key := range []string{"k8s.v1.cni.cncf.io/networks", "v1.multus-cni.io/default-network"} {
		actual, actualPresent := pod.Annotations[key]
		expected, expectedPresent := template.Annotations[key]
		if actualPresent != expectedPresent || actual != expected {
			return validationFailure(validation.Failed, validation.ReasonHealth, "Selected admitted Pod network request differs from its independently verified controller template.")
		}
	}
	return nil
}
