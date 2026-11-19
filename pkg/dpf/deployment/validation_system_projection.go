// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func (o *validationObserver) systemExpectedDaemonSetValues(role systemArtifactRole, service *unstructured.Unstructured) (map[string]any, error) {
	id := appSystemServiceID(service)
	if id == "" {
		return nil, validationFailure(validation.Pending, validation.ReasonProgressing, "Selected system service has no current service identity.")
	}
	labels := map[string]any{dpfComponentLabel: role.Component, hbnServiceIDLabel: id}
	tenant := o.desired.plan.Intent().Bootstrap.Tenant
	if role.Management {
		labels[dpfDPUClusterNameLabel] = tenant.Name
		labels[dpfDPUClusterNamespaceLabel] = tenant.Namespace
	}
	current, _, _ := unstructured.NestedMap(service.Object, "spec", "serviceDaemonSet")
	if !jsonEqual(current, map[string]any{"labels": labels}) {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "System service contains unsupported daemon-set controller additions.")
	}
	target := tenant.Name
	if role.Management {
		target = "in-cluster"
	}
	return map[string]any{"labels": labels, "annotations": map[string]any{dpfDPUClusterNameLabel: target, dpfDPUClusterNamespaceLabel: service.GetNamespace()}}, nil
}

// Specialization binds only already verified current names and service identity.
// All functional Helm values are independent retained/source expectations. It
// never renders arbitrary observed values as desired configuration.
func (o *validationObserver) specializeSystemArtifact(role systemArtifactRole, base systemArtifactExpectation, service, app *unstructured.Unstructured) (systemArtifactExpectation, error) {
	if len(base.archive) == 0 {
		return systemArtifactExpectation{}, validationFailure(validation.Unverified, validation.ReasonIncomplete, "Independent system chart bytes are unavailable for bound projection.")
	}
	global, err := o.systemExpectedDaemonSetValues(role, service)
	if err != nil {
		return systemArtifactExpectation{}, err
	}
	values := runtime.DeepCopyJSON(base.Values)
	values["global"] = map[string]any{"serviceDaemonSet": global}
	objects, err := renderSystemArtifactRoleAt(base.archive, base.source, role, values, base.version, app.GetName(), service.GetNamespace())
	if err != nil {
		return systemArtifactExpectation{}, err
	}
	result := base
	result.Workload = nil
	result.ConfigMaps = map[string]*unstructured.Unstructured{}
	result.Services = map[string]*unstructured.Unstructured{}
	result.Attachments = map[string]*unstructured.Unstructured{}
	for _, object := range objects {
		switch object.GetKind() {
		case "Deployment", "DaemonSet":
			if result.Workload != nil || object.GetKind() != base.WorkloadKind {
				return systemArtifactExpectation{}, fmt.Errorf("system workload projection shape changed")
			}
			result.Workload = object
		case "ConfigMap":
			result.ConfigMaps[object.GetName()] = object
		case "Service":
			if role.Component == "coredns" && role.Management {
				result.Services[object.GetName()] = object
			}
		case "NetworkAttachmentDefinition":
			result.Attachments[object.GetName()] = object
		}
	}
	if (base.Workload == nil) != (result.Workload == nil) {
		return systemArtifactExpectation{}, fmt.Errorf("system workload projection lost expected cardinality")
	}
	if result.Workload != nil {
		images, err := systemArtifactImages(result.Workload)
		if err != nil || !maps.Equal(images, base.Images) {
			return systemArtifactExpectation{}, fmt.Errorf("bound system projection changed independently locked images")
		}
	}
	return result, nil
}

// Compare the independently projected complete workload spec. The existing
// operator comparator supplies common Kubernetes Pod/Deployment defaults; only
// the DaemonSet-specific defaults are added here.
func validationSystemWorkloadMatches(expected, current *unstructured.Unstructured) bool {
	if expected == nil || current == nil || expected.GetKind() != current.GetKind() || expected.GetAPIVersion() != current.GetAPIVersion() {
		return false
	}
	if expected.GetName() != current.GetName() || expected.GetNamespace() != current.GetNamespace() {
		return false
	}
	switch expected.GetKind() {
	case "Deployment":
		want, we := normalizedManagerSpec(expected)
		got, ge := normalizedManagerSpec(current)
		if we != nil || ge != nil {
			return false
		}
		normalizeSystemPod(&want.Template.Spec)
		normalizeSystemPod(&got.Template.Spec)
		return jsonEqual(want, got)
	case "DaemonSet":
		var want, got appsv1.DaemonSet
		for _, entry := range []struct {
			object *unstructured.Unstructured
			target *appsv1.DaemonSet
		}{{expected, &want}, {current, &got}} {
			data, err := json.Marshal(entry.object.Object["spec"])
			if err != nil {
				return false
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&entry.target.Spec) != nil {
				return false
			}
		}
		for _, set := range []*appsv1.DaemonSet{&want, &got} {
			normalizeManagerPodTemplate(&set.Spec.Template.Spec)
			normalizeSystemPod(&set.Spec.Template.Spec)
			if set.Spec.RevisionHistoryLimit == nil {
				set.Spec.RevisionHistoryLimit = ptr.To(int32(10))
			}
			if set.Spec.UpdateStrategy.Type == "" {
				set.Spec.UpdateStrategy.Type = appsv1.RollingUpdateDaemonSetStrategyType
			}
			if set.Spec.UpdateStrategy.Type == appsv1.RollingUpdateDaemonSetStrategyType {
				if set.Spec.UpdateStrategy.RollingUpdate == nil {
					set.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{}
				}
				update := set.Spec.UpdateStrategy.RollingUpdate
				if update.MaxUnavailable == nil {
					update.MaxUnavailable = ptr.To(intstr.FromInt32(1))
				}
				if update.MaxSurge == nil {
					update.MaxSurge = ptr.To(intstr.FromInt32(0))
				}
			}
		}
		return jsonEqual(want.Spec, got.Spec)
	default:
		return false
	}
}
func normalizeSystemPod(pod *corev1.PodSpec) {
	for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
		for i := range containers {
			container := &containers[i]
			if container.ImagePullPolicy == "" {
				container.ImagePullPolicy = corev1.PullIfNotPresent
			}
			for j := range container.Ports {
				if container.Ports[j].Protocol == "" {
					container.Ports[j].Protocol = corev1.ProtocolTCP
				}
			}
			for j := range container.Env {
				if field := container.Env[j].ValueFrom; field != nil && field.FieldRef != nil && field.FieldRef.APIVersion == "" {
					field.FieldRef.APIVersion = "v1"
				}
			}
		}
	}
	for i := range pod.Volumes {
		volume := &pod.Volumes[i]
		if volume.ConfigMap != nil && volume.ConfigMap.DefaultMode == nil {
			volume.ConfigMap.DefaultMode = ptr.To(int32(420))
		}
		if volume.Secret != nil && volume.Secret.DefaultMode == nil {
			volume.Secret.DefaultMode = ptr.To(int32(420))
		}
		if volume.HostPath != nil && volume.HostPath.Type == nil {
			volume.HostPath.Type = ptr.To(corev1.HostPathUnset)
		}
	}
}
