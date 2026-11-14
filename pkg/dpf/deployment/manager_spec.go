// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// normalizedManagerSpec compares the complete manager spec, including pod
// metadata, credentials, environment, mounts and security settings. Decode
// strictly so fields unknown to this client cannot disappear from comparison.
// Defaults cover the retained operator chart's Deployment, not arbitrary Pods:
// https://github.com/kubernetes/kubernetes/blob/v1.35.4/pkg/apis/apps/v1/defaults.go
// https://github.com/kubernetes/kubernetes/blob/v1.35.4/pkg/apis/core/v1/defaults.go
// Pod-only admission defaults (such as resource requests copied from limits)
// do not apply to a Deployment template and are deliberately not synthesized.
func normalizedManagerSpec(object *unstructured.Unstructured) (appsv1.DeploymentSpec, error) {
	var spec appsv1.DeploymentSpec
	if object == nil || object.Object["spec"] == nil {
		return spec, fmt.Errorf("manager Deployment spec is required")
	}
	data, err := json.Marshal(object.Object["spec"])
	if err != nil {
		return spec, fmt.Errorf("encode manager Deployment spec")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, fmt.Errorf("manager Deployment spec is malformed or contains unsupported fields")
	}
	if spec.Replicas == nil {
		spec.Replicas = ptr.To(int32(1))
	}
	if *spec.Replicas <= 0 || spec.Selector == nil || len(spec.Template.Spec.Containers) == 0 {
		return spec, fmt.Errorf("manager Deployment needs positive replicas, a selector and containers")
	}
	if spec.Strategy.Type == "" {
		spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType {
		if spec.Strategy.RollingUpdate == nil {
			spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{}
		}
		if spec.Strategy.RollingUpdate.MaxSurge == nil {
			spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromString("25%"))
		}
		if spec.Strategy.RollingUpdate.MaxUnavailable == nil {
			spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromString("25%"))
		}
	}
	if spec.RevisionHistoryLimit == nil {
		spec.RevisionHistoryLimit = ptr.To(int32(10))
	}
	if spec.ProgressDeadlineSeconds == nil {
		spec.ProgressDeadlineSeconds = ptr.To(int32(600))
	}
	pod := &spec.Template.Spec
	if pod.RestartPolicy == "" {
		pod.RestartPolicy = corev1.RestartPolicyAlways
	}
	if pod.DNSPolicy == "" {
		pod.DNSPolicy = corev1.DNSClusterFirst
	}
	if pod.SchedulerName == "" {
		pod.SchedulerName = corev1.DefaultSchedulerName
	}
	if pod.TerminationGracePeriodSeconds == nil {
		pod.TerminationGracePeriodSeconds = ptr.To(int64(corev1.DefaultTerminationGracePeriodSeconds))
	}
	if pod.SecurityContext == nil {
		pod.SecurityContext = &corev1.PodSecurityContext{}
	}
	// API conversion mirrors the canonical service account in its legacy alias.
	if pod.DeprecatedServiceAccount == "" {
		pod.DeprecatedServiceAccount = pod.ServiceAccountName
	}
	for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
		for i := range containers {
			container := &containers[i]
			for _, resources := range []corev1.ResourceList{container.Resources.Limits, container.Resources.Requests} {
				for name, quantity := range resources {
					quantity.RoundUp(-3)
					resources[name] = quantity
				}
			}
			// The pinned chart supplies an explicit imagePullPolicy. Do not guess
			// defaults for an unrecognized chart image or additional container.
			if container.TerminationMessagePath == "" {
				container.TerminationMessagePath = corev1.TerminationMessagePathDefault
			}
			if container.TerminationMessagePolicy == "" {
				container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
			}
			for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
				defaultManagerProbe(probe)
			}
		}
	}
	for i := range pod.Volumes {
		projected := pod.Volumes[i].Projected
		if projected == nil {
			continue
		}
		if projected.DefaultMode == nil {
			projected.DefaultMode = ptr.To(int32(corev1.ProjectedVolumeSourceDefaultMode))
		}
		for j := range projected.Sources {
			token := projected.Sources[j].ServiceAccountToken
			if token != nil && token.ExpirationSeconds == nil {
				token.ExpirationSeconds = ptr.To(int64(3600))
			}
		}
	}
	return spec, nil
}

func defaultManagerProbe(probe *corev1.Probe) {
	if probe == nil {
		return
	}
	if probe.TimeoutSeconds == 0 {
		probe.TimeoutSeconds = 1
	}
	if probe.PeriodSeconds == 0 {
		probe.PeriodSeconds = 10
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = 1
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = 3
	}
	if probe.HTTPGet != nil {
		if probe.HTTPGet.Path == "" {
			probe.HTTPGet.Path = "/"
		}
		if probe.HTTPGet.Scheme == "" {
			probe.HTTPGet.Scheme = corev1.URISchemeHTTP
		}
	}
}
