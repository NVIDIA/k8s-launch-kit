// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// Native objects have closed meaningful content. DPF normalization uses only
// pinned CRD defaults and listed typed-serialization zeros; supported Kubernetes
// kinds use their actual typed API and defaults. Metadata/status remain API/controller observations, while supplied
// labels and annotations must still match. No arbitrary native kind is accepted.
func nativeMatches(release string, desired, current *unstructured.Unstructured) bool {
	if desired == nil || current == nil || desired.GroupVersionKind() != current.GroupVersionKind() {
		return false
	}
	want, ok := nativeBody(release, desired)
	if !ok {
		return false
	}
	actual, ok := nativeBody(release, current)
	if !ok || !jsonEqual(want, actual) {
		return false
	}
	return contains(current.Object["metadata"], desired.Object["metadata"])
}

func nativeBody(release string, object *unstructured.Unstructured) (map[string]any, bool) {
	if object == nil {
		return nil, false
	}
	copy := object.DeepCopy()
	group := copy.GroupVersionKind().Group
	if group == "provisioning.dpu.nvidia.com" || group == "svc.dpu.nvidia.com" {
		defaulted, err := sentry.DefaultedCopy(release, copy)
		if err != nil {
			return nil, false
		}
		copy = defaulted
		if !normalizeNativeSerialization(copy) {
			return nil, false
		}
	} else {
		// API-owned observations are not part of declarative RBAC/ServiceAccount
		// content. Strict decoding below rejects every unknown meaningful field.
		delete(copy.Object, "metadata")
		delete(copy.Object, "status")
		data, err := json.Marshal(copy.Object)
		if err != nil {
			return nil, false
		}
		var typed any
		switch copy.GetAPIVersion() + "/" + copy.GetKind() {
		case "v1/ServiceAccount":
			typed = &corev1.ServiceAccount{} // v1.35 has no ServiceAccount API defaulter.
		case "rbac.authorization.k8s.io/v1/Role":
			typed = &rbacv1.Role{}
		case "rbac.authorization.k8s.io/v1/RoleBinding":
			typed = &rbacv1.RoleBinding{}
		default:
			return nil, false
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(typed) != nil {
			return nil, false
		}
		if binding, ok := typed.(*rbacv1.RoleBinding); ok {
			// Kubernetes v1.35 pkg/apis/rbac/v1/defaults.go.
			if binding.RoleRef.APIGroup == "" {
				binding.RoleRef.APIGroup = rbacv1.GroupName
			}
			for i := range binding.Subjects {
				subject := &binding.Subjects[i]
				if subject.APIGroup == "" && (subject.Kind == "User" || subject.Kind == "Group") {
					subject.APIGroup = rbacv1.GroupName
				}
			}
		}
		data, err = json.Marshal(typed)
		copy.Object = nil
		if err != nil || json.Unmarshal(data, &copy.Object) != nil {
			return nil, false
		}
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(copy.Object, key)
	}
	return copy.Object, true
}

// Only pinned non-pointer value fields whose zero value is serialized by Go
// are listed here. In particular, optional pointer objects (such as security.
// spiffe) are presence-gated configuration and must never be synthesized.
// rc.1 and GA have identical definitions at these paths.
type nativeSerializationZero struct {
	path  []string
	value any
}

var nativeSerializationZeros = map[string][]nativeSerializationZero{
	"DPUFlavor": { // provisioning/v1alpha1/dpuflavor_types.go:38,41,56,75
		{[]string{"spec", "grub"}, map[string]any{}},
		{[]string{"spec", "sysctl"}, map[string]any{}},
		{[]string{"spec", "ovs"}, map[string]any{}},
		{[]string{"spec", "containerdConfig"}, map[string]any{}},
	},
	"DPUServiceInterface": { // dpuserviceinterface_types.go:98; serviceinterfaceset_types.go:88
		{[]string{"spec", "template", "metadata"}, map[string]any{}},
		{[]string{"spec", "template", "spec", "template", "metadata"}, map[string]any{}},
	},
	"DPUServiceChain": { // dpuservicechain_types.go:91; servicechainset_types.go:81
		{[]string{"spec", "template", "metadata"}, map[string]any{}},
		{[]string{"spec", "template", "spec", "template", "metadata"}, map[string]any{}},
	},
	"DPUService": { // dpuservice_types.go:348 (optional without omitempty)
		{[]string{"spec", "helmChart", "source", "path"}, ""},
	},
}

// Fill absent zero-value fields only under existing required parents. Never
// roundtrip a DPF spec through a typed struct: that would discard unknown fields.
func normalizeNativeSerialization(object *unstructured.Unstructured) bool {
	for _, zero := range nativeSerializationZeros[object.GetKind()] {
		parent, found, err := unstructured.NestedFieldNoCopy(object.Object, zero.path[:len(zero.path)-1]...)
		fields, isObject := parent.(map[string]any)
		if err != nil || !found || !isObject {
			return false
		}
		if _, exists := fields[zero.path[len(zero.path)-1]]; !exists {
			if unstructured.SetNestedField(object.Object, zero.value, zero.path...) != nil {
				return false
			}
		}
	}
	return true
}

// Projections preserve ordered array membership recursively. For the two
// supported Sentry workload kinds, compare the entire typed, defaulted spec so
// injected containers, security settings, scheduling or volumes cannot hide in
// additional map keys. API metadata/status are observed separately.
func desiredMatches(desired, current *unstructured.Unstructured) bool {
	if desired == nil || current == nil {
		return false
	}
	want, actual := desired.DeepCopy(), current.DeepCopy()
	if desired.GroupVersionKind().Group == "apps" && (desired.GetKind() == "Deployment" || desired.GetKind() == "DaemonSet") {
		a, ok := workloadSpec(want)
		if !ok {
			return false
		}
		b, ok := workloadSpec(actual)
		if !ok || !jsonEqual(a, b) {
			return false
		}
		delete(want.Object, "spec")
	}
	return contains(actual.Object, want.Object)
}

func contains(actual, desired any) bool {
	switch expected := desired.(type) {
	case map[string]any:
		found, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range expected {
			if !contains(found[key], value) {
				return false
			}
		}
		return true
	case []any:
		found, ok := actual.([]any)
		if !ok || len(found) != len(expected) {
			return false
		}
		for i := range expected {
			if !contains(found[i], expected[i]) {
				return false
			}
		}
		return true
	default:
		return jsonEqual(actual, desired)
	}
}
func jsonEqual(a, b any) bool {
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && bytes.Equal(x, y)
}

// These bounded typed defaults follow Kubernetes v1.35 pkg/apis/apps/v1 and
// pkg/apis/core/v1 defaults.go, PodSpec conversion.go alias handling, and their
// generated PodTemplate traversal. They
// cover the pinned Sentry Deployment/DaemonSet charts. New/feature-gated defaults
// are intentionally not discarded; a changed API contract requires review.
// Strict typed decoding rejects unknown fields instead of silently dropping them.
func workloadSpec(o *unstructured.Unstructured) (any, bool) {
	data, err := json.Marshal(o.Object["spec"])
	if err != nil {
		return nil, false
	}
	decode := func(value any) bool {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		return decoder.Decode(value) == nil
	}
	switch o.GetKind() {
	case "Deployment":
		var spec appsv1.DeploymentSpec
		if !decode(&spec) {
			return nil, false
		}
		if spec.Replicas == nil {
			spec.Replicas = ptr.To(int32(1))
		}
		if spec.RevisionHistoryLimit == nil {
			spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		if spec.ProgressDeadlineSeconds == nil {
			spec.ProgressDeadlineSeconds = ptr.To(int32(600))
		}
		if spec.Strategy.Type == "" {
			spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		}
		if spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType {
			if spec.Strategy.RollingUpdate == nil {
				spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{}
			}
			if spec.Strategy.RollingUpdate.MaxUnavailable == nil {
				spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromString("25%"))
			}
			if spec.Strategy.RollingUpdate.MaxSurge == nil {
				spec.Strategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromString("25%"))
			}
		}
		if !normalizePodSpec(&spec.Template.Spec) {
			return nil, false
		}
		return spec, true
	case "DaemonSet":
		var spec appsv1.DaemonSetSpec
		if !decode(&spec) {
			return nil, false
		}
		if spec.RevisionHistoryLimit == nil {
			spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		if spec.UpdateStrategy.Type == "" {
			spec.UpdateStrategy.Type = appsv1.RollingUpdateDaemonSetStrategyType
		}
		if spec.UpdateStrategy.Type == appsv1.RollingUpdateDaemonSetStrategyType {
			if spec.UpdateStrategy.RollingUpdate == nil {
				spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{}
			}
			if spec.UpdateStrategy.RollingUpdate.MaxUnavailable == nil {
				spec.UpdateStrategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt32(1))
			}
			if spec.UpdateStrategy.RollingUpdate.MaxSurge == nil {
				spec.UpdateStrategy.RollingUpdate.MaxSurge = ptr.To(intstr.FromInt32(0))
			}
		}
		if !normalizePodSpec(&spec.Template.Spec) {
			return nil, false
		}
		return spec, true
	}
	return nil, false
}

func normalizePodSpec(p *corev1.PodSpec) bool {
	// Kubernetes v1.35 core/v1/conversion.go copies serviceAccountName to
	// serviceAccount on output, and accepts the deprecated alias on input.
	// Reject conflicting explicit aliases rather than discard supplied intent.
	if p.ServiceAccountName != "" && p.DeprecatedServiceAccount != "" && p.ServiceAccountName != p.DeprecatedServiceAccount {
		return false
	}
	if p.ServiceAccountName == "" {
		p.ServiceAccountName = p.DeprecatedServiceAccount
	}
	p.DeprecatedServiceAccount = p.ServiceAccountName
	if p.DNSPolicy == "" {
		p.DNSPolicy = corev1.DNSClusterFirst
	}
	if p.RestartPolicy == "" {
		p.RestartPolicy = corev1.RestartPolicyAlways
	}
	if p.SecurityContext == nil {
		p.SecurityContext = &corev1.PodSecurityContext{}
	}
	if p.TerminationGracePeriodSeconds == nil {
		p.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if p.SchedulerName == "" {
		p.SchedulerName = corev1.DefaultSchedulerName
	}
	for i := range p.Containers {
		defaultContainer(&p.Containers[i])
	}
	for i := range p.InitContainers {
		defaultContainer(&p.InitContainers[i])
	}
	for i := range p.Volumes {
		v := &p.Volumes[i]
		if reflect.DeepEqual(v.VolumeSource, corev1.VolumeSource{}) {
			v.EmptyDir = &corev1.EmptyDirVolumeSource{}
		}
		if v.HostPath != nil && v.HostPath.Type == nil {
			v.HostPath.Type = ptr.To(corev1.HostPathUnset)
		}
		if v.Secret != nil && v.Secret.DefaultMode == nil {
			v.Secret.DefaultMode = ptr.To(int32(420))
		}
		if v.ConfigMap != nil && v.ConfigMap.DefaultMode == nil {
			v.ConfigMap.DefaultMode = ptr.To(int32(420))
		}
		if v.DownwardAPI != nil {
			if v.DownwardAPI.DefaultMode == nil {
				v.DownwardAPI.DefaultMode = ptr.To(int32(420))
			}
			for j := range v.DownwardAPI.Items {
				defaultFieldRef(v.DownwardAPI.Items[j].FieldRef)
			}
		}
		if v.Projected != nil {
			if v.Projected.DefaultMode == nil {
				v.Projected.DefaultMode = ptr.To(int32(420))
			}
			for j := range v.Projected.Sources {
				source := &v.Projected.Sources[j]
				if source.ServiceAccountToken != nil && source.ServiceAccountToken.ExpirationSeconds == nil {
					source.ServiceAccountToken.ExpirationSeconds = ptr.To(int64(3600))
				}
				if source.DownwardAPI != nil {
					for k := range source.DownwardAPI.Items {
						defaultFieldRef(source.DownwardAPI.Items[k].FieldRef)
					}
				}
			}
		}
	}
	return true
}
func defaultFieldRef(r *corev1.ObjectFieldSelector) {
	if r != nil && r.APIVersion == "" {
		r.APIVersion = "v1"
	}
}
func defaultContainer(c *corev1.Container) {
	if c.ImagePullPolicy == "" {
		name := strings.SplitN(c.Image, "@", 2)[0]
		tag := name[strings.LastIndex(name, "/")+1:]
		if !strings.Contains(c.Image, "@") && (!strings.Contains(tag, ":") || strings.HasSuffix(tag, ":latest")) {
			c.ImagePullPolicy = corev1.PullAlways
		} else {
			c.ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	if c.TerminationMessagePath == "" {
		c.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for i := range c.Ports {
		if c.Ports[i].Protocol == "" {
			c.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range c.Env {
		if c.Env[i].ValueFrom != nil {
			defaultFieldRef(c.Env[i].ValueFrom.FieldRef)
		}
	}
	for _, p := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		if p == nil {
			continue
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = 1
		}
		if p.PeriodSeconds == 0 {
			p.PeriodSeconds = 10
		}
		if p.SuccessThreshold == 0 {
			p.SuccessThreshold = 1
		}
		if p.FailureThreshold == 0 {
			p.FailureThreshold = 3
		}
		defaultHTTP(p.HTTPGet)
		if p.GRPC != nil && p.GRPC.Service == nil {
			p.GRPC.Service = ptr.To("")
		}
	}
	if c.Lifecycle != nil {
		if c.Lifecycle.PostStart != nil {
			defaultHTTP(c.Lifecycle.PostStart.HTTPGet)
		}
		if c.Lifecycle.PreStop != nil {
			defaultHTTP(c.Lifecycle.PreStop.HTTPGet)
		}
	}
}
func defaultHTTP(h *corev1.HTTPGetAction) {
	if h != nil {
		if h.Path == "" {
			h.Path = "/"
		}
		if h.Scheme == "" {
			h.Scheme = corev1.URISchemeHTTP
		}
	}
}
