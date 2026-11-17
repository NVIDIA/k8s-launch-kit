// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// These network configuration inputs follow DPF v26.8.0's
// internal/clustermanager/kamaji/manifests/keepalived.yaml at fc4974ed.
// Image provenance for the main keepalived container remains a separate check.
const tenantKeepalivedTemplate = `vrrp_script chk-control-plane {
  script "/usr/bin/curl -k https://NODE_IP:NODE_PORT"
  interval 2
  rise 3
  fall 2
  weight 0
}

vrrp_instance dpu-cluster-control-plane {
    state BACKUP
    interface INTERFACE
    virtual_router_id VIRTUAL_ROUTER_ID
    priority PRIORITY

    virtual_ipaddress {
       VIRTUAL_IP
    }

    track_script {
        chk-control-plane
    }
}
`

const tenantKeepalivedCommand = `#! /bin/sh
PRIORITY=$(echo ${NODE_IP} | cut -d. -f4)
sed "s/INTERFACE/${INTERFACE}/g; s/VIRTUAL_ROUTER_ID/${VIRTUAL_ROUTER_ID}/g; s/PRIORITY/${PRIORITY}/g; s/NODE_PORT/${NODE_PORT}/g; s/VIRTUAL_IP/${VIRTUAL_IP}/g; s/NODE_IP/${NODE_IP}/g" /conf-template/keepalived.conf.tmpl > /conf/keepalived.conf
`

func compareKeepalivedNetwork(ds *unstructured.Unstructured, expected TenantReadinessExpected, port int64) error {
	var typed appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(ds.Object, &typed); err != nil {
		return fmt.Errorf("keepalived DaemonSet is malformed")
	}
	pod := typed.Spec.Template.Spec
	if !pod.HostNetwork || typed.Spec.Template.Labels["app.kubernetes.io/name"] != expected.Name+"-keepalived" {
		return fmt.Errorf("keepalived requires its selected pod identity and management host network")
	}
	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil || !reflect.DeepEqual(pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution, tenantControlPlaneAffinity()) {
		return fmt.Errorf("keepalived control-plane node affinity differs")
	}
	for _, key := range []string{"node-role.kubernetes.io/master", "node-role.kubernetes.io/control-plane"} {
		found := false
		for _, toleration := range pod.Tolerations {
			if toleration.Key == key && toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoSchedule && toleration.Value == "" && toleration.TolerationSeconds == nil {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("keepalived is missing a required control-plane toleration")
		}
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "create-conf" {
		return fmt.Errorf("keepalived configuration init container is missing or ambiguous")
	}
	init := pod.InitContainers[0]
	if init.Image != "alpine:3.20.3@sha256:1e42bbe2508154c9126d48c2b8a75420c3544343bf86fd041fb7527e017a4b4a" || len(init.Args) != 0 || !reflect.DeepEqual(init.Command, []string{"/bin/sh", "-c", tenantKeepalivedCommand}) {
		return fmt.Errorf("keepalived configuration renderer differs from the selected controller")
	}
	values := map[string]string{"INTERFACE": expected.Interface, "VIRTUAL_ROUTER_ID": strconv.FormatInt(expected.VirtualRouterID, 10), "NODE_PORT": strconv.FormatInt(port, 10), "VIRTUAL_IP": expected.VIP}
	if len(init.Env) != len(values)+1 || len(init.EnvFrom) != 0 {
		return fmt.Errorf("keepalived endpoint environment is missing or ambiguous")
	}
	seen := map[string]bool{}
	for _, env := range init.Env {
		if seen[env.Name] {
			return fmt.Errorf("keepalived endpoint environment contains duplicate names")
		}
		seen[env.Name] = true
		if env.Name == "NODE_IP" {
			if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.FieldRef == nil {
				return fmt.Errorf("keepalived node IP must come from status.hostIP")
			}
			reference := *env.ValueFrom
			field := reference.FieldRef
			reference.FieldRef = nil
			if reference != (corev1.EnvVarSource{}) || field.FieldPath != "status.hostIP" || field.APIVersion != "" && field.APIVersion != "v1" {
				return fmt.Errorf("keepalived node IP reference differs")
			}
		} else if want, ok := values[env.Name]; !ok || env.ValueFrom != nil || env.Value != want {
			return fmt.Errorf("keepalived endpoint environment differs")
		}
	}
	if !reflect.DeepEqual(init.VolumeMounts, []corev1.VolumeMount{{Name: "conf-template", MountPath: "/conf-template", ReadOnly: true}, {Name: "conf", MountPath: "/conf"}}) {
		return fmt.Errorf("keepalived configuration renderer mounts differ")
	}
	if len(pod.Volumes) != 2 {
		return fmt.Errorf("keepalived configuration volumes are missing or ambiguous")
	}
	volumeSeen := map[string]bool{}
	for _, volume := range pod.Volumes {
		if volumeSeen[volume.Name] {
			return fmt.Errorf("keepalived configuration volume is duplicated")
		}
		volumeSeen[volume.Name] = true
		source := volume.VolumeSource
		switch volume.Name {
		case "conf":
			if source.EmptyDir == nil || source.EmptyDir.Medium != "" || source.EmptyDir.SizeLimit == nil || source.EmptyDir.SizeLimit.Value() != 50<<20 {
				return fmt.Errorf("keepalived generated configuration storage differs")
			}
			source.EmptyDir = nil
		case "conf-template":
			if source.ConfigMap == nil || source.ConfigMap.Name != expected.Name+"-keepalived" || len(source.ConfigMap.Items) != 0 || source.ConfigMap.Optional != nil && *source.ConfigMap.Optional || source.ConfigMap.DefaultMode != nil && *source.ConfigMap.DefaultMode != 420 {
				return fmt.Errorf("keepalived configuration template reference differs")
			}
			source.ConfigMap = nil
		default:
			return fmt.Errorf("keepalived configuration volume is unexpected")
		}
		if !reflect.DeepEqual(source, corev1.VolumeSource{}) {
			return fmt.Errorf("keepalived configuration volume has conflicting sources")
		}
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != "keepalived" || len(pod.Containers[0].Command) != 0 || len(pod.Containers[0].Args) != 0 || !reflect.DeepEqual(pod.Containers[0].VolumeMounts, []corev1.VolumeMount{{Name: "conf", MountPath: "/usr/local/etc/keepalived"}}) {
		return fmt.Errorf("keepalived does not consume the generated network configuration as expected")
	}
	privileged, root := true, int64(0)
	security := &corev1.SecurityContext{Privileged: &privileged, Capabilities: &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN", "NET_RAW", "NET_BROADCAST"},
	}}
	if !reflect.DeepEqual(pod.Containers[0].SecurityContext, security) || !reflect.DeepEqual(pod.SecurityContext, &corev1.PodSecurityContext{RunAsUser: &root}) {
		return fmt.Errorf("keepalived privileges differ from the selected controller's network configuration")
	}
	if len(pod.Containers[0].Env) != 0 || len(pod.Containers[0].EnvFrom) != 0 {
		return fmt.Errorf("keepalived has unexpected runtime environment overrides")
	}
	return nil
}

func tenantControlPlaneAffinity() *corev1.NodeSelector {
	terms := make([]corev1.NodeSelectorTerm, 0, 2)
	for _, key := range []string{"node-role.kubernetes.io/master", "node-role.kubernetes.io/control-plane"} {
		terms = append(terms, corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpExists}}})
	}
	return &corev1.NodeSelector{NodeSelectorTerms: terms}
}

func sameKeepalivedTemplate(value string) bool {
	// Trailing whitespace is immaterial to keepalived, but comments containing
	// placeholders cannot stand in for the selected functional configuration.
	normalize := func(text string) string {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " \t\r")
		}
		return strings.Join(lines, "\n")
	}
	return normalize(value) == normalize(tenantKeepalivedTemplate)
}
