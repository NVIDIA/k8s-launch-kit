// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	sigsyaml "sigs.k8s.io/yaml"
)

// Hand-written source fixture: the independent template below and the admitted
// Pod encode ordinary Kubernetes defaults separately, not via the comparator.
func validationAdmittedPodFixture(t *testing.T) (*corev1.Pod, *corev1.PodSpec) {
	t.Helper()
	template := &corev1.PodSpec{ServiceAccountName: "selected-account", AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "manager", Image: "registry.example/controller:v1", Command: []string{"/manager"}, Args: []string{"--mode=selected"}, Env: []corev1.EnvVar{{Name: "NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}}, Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(8080)}}}, SecurityContext: &corev1.SecurityContext{RunAsNonRoot: ptr.To(true)}, VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/controller", ReadOnly: true}}}}, Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "selected-config"}}}}}}
	var pod corev1.Pod
	if err := sigsyaml.UnmarshalStrict([]byte(`
apiVersion: v1
kind: Pod
metadata: {name: current, namespace: selected, uid: current-pod, resourceVersion: "10"}
spec:
  nodeName: selected-node
  serviceAccountName: selected-account
  serviceAccount: selected-account
  automountServiceAccountToken: false
  restartPolicy: Always
  dnsPolicy: ClusterFirst
  schedulerName: default-scheduler
  terminationGracePeriodSeconds: 30
  securityContext: {}
  enableServiceLinks: true
  priority: 0
  preemptionPolicy: PreemptLowerPriority
  tolerations:
  - {key: node.kubernetes.io/not-ready, operator: Exists, effect: NoExecute, tolerationSeconds: 300}
  - {key: node.kubernetes.io/unreachable, operator: Exists, effect: NoExecute, tolerationSeconds: 300}
  containers:
  - name: manager
    image: registry.example/controller:v1
    imagePullPolicy: IfNotPresent
    command: [/manager]
    args: [--mode=selected]
    env:
    - name: NODE
      valueFrom: {fieldRef: {apiVersion: v1, fieldPath: spec.nodeName}}
    ports: [{name: metrics, containerPort: 8080, protocol: TCP}]
    readinessProbe:
      httpGet: {path: /readyz, port: 8080, scheme: HTTP}
      timeoutSeconds: 1
      periodSeconds: 10
      successThreshold: 1
      failureThreshold: 3
    securityContext: {runAsNonRoot: true}
    terminationMessagePath: /dev/termination-log
    terminationMessagePolicy: File
    volumeMounts: [{name: config, mountPath: /etc/controller, readOnly: true}]
  volumes:
  - name: config
    configMap: {name: selected-config, defaultMode: 420}
status:
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses:
  - {name: manager, imageID: "sha256:running", ready: true, state: {running: {}}}
`), &pod); err != nil {
		t.Fatal(err)
	}
	return &pod, template
}

func TestValidationAdmittedPodFunctionRejectsPodOnlyDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1.Pod)
	}{
		{"command", func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"sleep", "infinity"} }},
		{"arguments", func(p *corev1.Pod) { p.Spec.Containers[0].Args = []string{"--mode=wrong"} }},
		{"environment reference", func(p *corev1.Pod) { p.Spec.Containers[0].Env[0].ValueFrom.FieldRef.FieldPath = "metadata.name" }},
		{"extra environment", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "UNEXPECTED", Value: "true"})
		}},
		{"service account", func(p *corev1.Pod) {
			p.Spec.ServiceAccountName = "unprivileged"
			p.Spec.DeprecatedServiceAccount = "unprivileged"
		}},
		{"credential policy", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) }},
		{"configuration source", func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Name = "other" }},
		{"mount", func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].ReadOnly = false }},
		{"readiness probe", func(p *corev1.Pod) { p.Spec.Containers[0].ReadinessProbe.HTTPGet.Path = "/fake" }},
		{"startup probe", func(p *corev1.Pod) {
			p.Spec.Containers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
		}},
		{"security", func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) }},
		{"host network", func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
		{"DNS", func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSDefault }},
		{"host PID", func(p *corev1.Pod) { p.Spec.HostPID = true }},
		{"host port", func(p *corev1.Pod) { p.Spec.Containers[0].Ports[0].HostPort = 8080 }},
		{"scheduler", func(p *corev1.Pod) { p.Spec.SchedulerName = "other-scheduler" }},
		{"priority", func(p *corev1.Pod) { p.Spec.Priority = ptr.To(int32(100)) }},
		{"preemption", func(p *corev1.Pod) { p.Spec.PreemptionPolicy = ptr.To(corev1.PreemptNever) }},
		{"pull policy without selected admission", func(p *corev1.Pod) { p.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways }},
		{"extra init process", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "extra", Image: "registry.example/extra:v1"}}
		}},
		{"unexpected toleration", func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod, template := validationAdmittedPodFixture(t)
			test.change(pod)
			beforePod, beforeTemplate := pod.DeepCopy(), template.DeepCopy()
			err := validationPodMatchesTemplate(pod, template, "ReplicaSet", false)
			failure, ok := err.(*validationCheckError)
			if !ok || failure.state != validation.Failed {
				t.Fatalf("Pod-only functional drift accepted or not terminal: %#v", err)
			}
			if !reflect.DeepEqual(pod, beforePod) || !reflect.DeepEqual(template, beforeTemplate) {
				t.Fatal("comparison mutated caller-owned Pod/template")
			}
		})
	}
}

func TestValidationAdmittedPodDefaultsAndSelectedPullAdmission(t *testing.T) {
	pod, template := validationAdmittedPodFixture(t)
	if err := validationPodMatchesTemplate(pod, template, "ReplicaSet", false); err != nil {
		t.Fatalf("independent API-default fixture rejected: %#v", err)
	}
	pod.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
	if err := validationPodMatchesTemplate(pod, template, "ReplicaSet", true); err != nil {
		t.Fatalf("explicit selected AlwaysPullImages admission rejected: %#v", err)
	}
	template.Containers[0].ImagePullPolicy = corev1.PullNever
	if validationPodMatchesTemplate(pod, template, "ReplicaSet", true) == nil {
		t.Fatal("Never->Always was silently waived")
	}
	template.Containers[0].ImagePullPolicy = corev1.PullAlways
	pod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	if validationPodMatchesTemplate(pod, template, "ReplicaSet", true) == nil {
		t.Fatal("reverse pull-policy weakening was waived")
	}
}

func TestValidationAdmittedPodBoundTokenProjection(t *testing.T) {
	pod, template := validationAdmittedPodFixture(t)
	pod.Spec.AutomountServiceAccountToken, template.AutomountServiceAccountToken = nil, nil
	// Literal standard ServiceAccount admission projection from upstream source.
	var volume corev1.Volume
	if err := sigsyaml.UnmarshalStrict([]byte(`
name: kube-api-access-bc2df
projected:
  defaultMode: 420
  sources:
  - serviceAccountToken: {path: token, expirationSeconds: 3607}
  - configMap: {name: kube-root-ca.crt, items: [{key: ca.crt, path: ca.crt}]}
  - downwardAPI:
      items:
      - path: namespace
        fieldRef: {apiVersion: v1, fieldPath: metadata.namespace}
`), &volume); err != nil {
		t.Fatal(err)
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: volume.Name, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	if err := validationPodMatchesTemplate(pod, template, "ReplicaSet", false); err != nil {
		t.Fatalf("standard bound credential projection rejected: %#v", err)
	}
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.Spec.Volumes[1].Projected.Sources[0].ServiceAccountToken.Audience = "other" },
		func(p *corev1.Pod) { p.Spec.Volumes[1].Projected.Sources[1].ConfigMap.Name = "other-ca" },
		func(p *corev1.Pod) {
			p.Spec.Volumes[1].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(int64(86400))
		},
		func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[1].ReadOnly = false },
		func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, *p.Spec.Volumes[1].DeepCopy())
			p.Spec.Volumes[2].Name = "kube-api-access-23456"
		},
	} {
		changed := pod.DeepCopy()
		mutate(changed)
		if validationPodMatchesTemplate(changed, template, "ReplicaSet", false) == nil {
			t.Fatal("unexpected automatic credential wiring accepted")
		}
	}
}

func TestValidationAdmittedPodDaemonSchedulerDefaults(t *testing.T) {
	pod, template := validationAdmittedPodFixture(t)
	template.HostNetwork, pod.Spec.HostNetwork = true, true
	template.PriorityClassName, pod.Spec.PriorityClassName = "system-node-critical", "system-node-critical"
	pod.Spec.Priority = ptr.To(int32(2000001000))
	pod.Spec.Containers[0].Ports[0].HostPort = 8080
	template.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "selected-device", Operator: corev1.NodeSelectorOpExists}}}}}}}
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"selected-node"}}}}}}}}
	pod.Spec.Tolerations = []corev1.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/network-unavailable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	if err := validationPodMatchesTemplate(pod, template, "DaemonSet", false); err != nil {
		t.Fatalf("documented DaemonSet target-node replacement rejected: %#v", err)
	}
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values[0] = "other-node"
	if validationPodMatchesTemplate(pod, template, "DaemonSet", false) == nil {
		t.Fatal("DaemonSet Pod target-node affinity drift accepted")
	}
}

func TestValidationPodRequestedNetworks(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"k8s.v1.cni.cncf.io/network-status": "observed output"}}}
	template := &corev1.PodTemplateSpec{}
	if err := validationPodNetworkRequestsMatch(pod, template); err != nil {
		t.Fatal("observed network-status was treated as desired input")
	}
	for _, key := range []string{"k8s.v1.cni.cncf.io/networks", "v1.multus-cni.io/default-network"} {
		pod.Annotations[key] = "unexpected-network"
		if validationPodNetworkRequestsMatch(pod, template) == nil {
			t.Fatal("injected requested network accepted")
		}
		template.Annotations = map[string]string{key: "selected-network"}
		pod.Annotations[key] = "selected-network"
		if err := validationPodNetworkRequestsMatch(pod, template); err != nil {
			t.Fatal(err)
		}
		delete(pod.Annotations, key)
		if validationPodNetworkRequestsMatch(pod, template) == nil {
			t.Fatal("missing expected requested network accepted")
		}
		template.Annotations = nil
	}
}

func TestValidationAdmittedPodUnknownAdmissionDoesNotPass(t *testing.T) {
	pod, template := validationAdmittedPodFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod, *corev1.PodSpec)
		kind   string
		state  validation.State
	}{
		{"unbound Pod", func(p *corev1.Pod, _ *corev1.PodSpec) { p.Spec.NodeName = "" }, "ReplicaSet", validation.Pending},
		{"custom priority admission", func(_ *corev1.Pod, s *corev1.PodSpec) { s.PriorityClassName = "unqualified" }, "ReplicaSet", validation.Unverified},
		{"unknown controller", func(*corev1.Pod, *corev1.PodSpec) {}, "Job", validation.Unverified},
		{"unknown SA automount policy", func(p *corev1.Pod, s *corev1.PodSpec) {
			p.Spec.AutomountServiceAccountToken = nil
			s.AutomountServiceAccountToken = nil
		}, "ReplicaSet", validation.Unverified},
		{"missing explicitly requested credentials", func(p *corev1.Pod, s *corev1.PodSpec) {
			p.Spec.AutomountServiceAccountToken = ptr.To(true)
			s.AutomountServiceAccountToken = ptr.To(true)
		}, "ReplicaSet", validation.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, s := pod.DeepCopy(), template.DeepCopy()
			test.mutate(p, s)
			err := validationPodMatchesTemplate(p, s, test.kind, false)
			failure, ok := err.(*validationCheckError)
			if !ok || failure.state != test.state {
				t.Fatalf("unknown/partial admission incorrectly qualified: %#v", err)
			}
		})
	}
	if validationPodMatchesTemplate(nil, template, "ReplicaSet", false) == nil || validationPodMatchesTemplate(pod, nil, "ReplicaSet", false) == nil {
		t.Fatal("missing input accepted")
	}
}
