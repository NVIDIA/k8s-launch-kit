// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	kptr "k8s.io/utils/ptr"
)

func kamajiFixture(t *testing.T) (*fake.Clientset, *appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod, *corev1.Node, releases.KamajiCompatibility) {
	t.Helper()
	release, err := releases.Resolve("26.8.0")
	if err != nil {
		t.Fatal(err)
	}
	rule := *release.KamajiCompatibility
	ns := "kamaji-system"
	ls := map[string]string{"app.kubernetes.io/name": "kamaji", "app.kubernetes.io/instance": "kamaji"}
	manager := corev1.Container{Name: "manager", Image: rule.Image, Args: []string{"manager", "--health-probe-bind-address=:8081", "--leader-elect", "--metrics-bind-address=:8080", "--tmp-directory=/tmp/kamaji", "--datastore=default"}, Env: []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}, {Name: "SERVICE_ACCOUNT", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}}}, ReadinessProbe: &corev1.Probe{}, LivenessProbe: &corev1.Probe{}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "kamaji", Namespace: ns, UID: "deployment-uid", Generation: 1, Labels: ls}, Spec: appsv1.DeploymentSpec{Replicas: kptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: ls}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: ls}, Spec: corev1.PodSpec{ServiceAccountName: "kamaji", Containers: []corev1.Container{manager}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	owner := func(kind, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: kptr.To(true)}}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "kamaji-current", Namespace: ns, UID: "rs-uid", Generation: 1, OwnerReferences: owner("Deployment", d.Name, d.UID)}, Spec: appsv1.ReplicaSetSpec{Replicas: kptr.To(int32(1)), Selector: d.Spec.Selector.DeepCopy(), Template: *d.Spec.Template.DeepCopy()}, Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kamaji-current-pod", Namespace: ns, UID: "pod-uid", Labels: ls, OwnerReferences: owner("ReplicaSet", rs.Name, rs.UID)}, Spec: *d.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Image: rule.Image, ImageID: "docker-pullable://docker.io/clastix/kamaji@" + rule.PlatformManifest, ContainerID: "containerd://container-uid", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	pod.Spec.NodeName = "manager-node"
	for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: kptr.To(int64(300))})
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "manager-node", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}}}
	return fake.NewSimpleClientset(d, rs, pod, node), d, rs, pod, node, rule
}

func TestKamajiCompatibilityRuntimeArtifactForms(t *testing.T) {
	for _, form := range []string{"index", "manifest", "config", "explicit-entrypoint"} {
		t.Run(form, func(t *testing.T) {
			_, d, rs, pod, node, rule := kamajiFixture(t)
			switch form {
			case "index":
				pod.Status.ContainerStatuses[0].ImageID = "docker.io/clastix/kamaji@" + strings.Split(rule.Image, "@")[1]
			case "manifest":
				pod.Status.ContainerStatuses[0].ImageID = "containerd://" + rule.PlatformManifest
			case "config":
				pod.Status.ContainerStatuses[0].ImageID = "docker://" + rule.ConfigDigest
			case "explicit-entrypoint":
				d.Spec.Template.Spec.Containers[0].Command = []string{"/ko-app/kamaji"}
				rs.Spec.Template = *d.Spec.Template.DeepCopy()
				pod.Spec.Containers = d.Spec.Template.Spec.DeepCopy().Containers
			}
			client := fake.NewSimpleClientset(d, rs, pod, node)
			got := (Collector{Kubernetes: client}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if got.State != "passed" {
				t.Fatalf("%+v", got)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "list" && action.GetVerb() != "get" {
					t.Fatalf("unexpected effect: %v", action)
				}
			}
		})
	}
}

func TestKamajiCompatibilityRejectsUnsafeProducer(t *testing.T) {
	cases := map[string]func(*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod, *corev1.Node){
		"known vendor bug": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/nvidia/kamaji:v1.36.0-26.6.4"
		},
		"unknown newer tag": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Image = "docker.io/clastix/kamaji:99.1.0-edge"
		},
		"tag only": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Image = strings.Split(d.Spec.Template.Spec.Containers[0].Image, "@")[0]
		},
		"init false positive": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.InitContainers = d.Spec.Template.Spec.Containers
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "manager", Image: "other"}}
		},
		"sidecar false positive": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Name = "sidecar"
		},
		"stale deployment": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) { d.Generation++ },
		"deleting deployment": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			now := metav1.Now()
			d.DeletionTimestamp = &now
		},
		"incomplete rollout": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.UpdatedReplicas = 0
		},
		"bad selector": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"other": "controller"}}
		},
		"command override": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "other"}
		},
		"args override": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--unknown")
		},
		"env override": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "KUBECONFIG", Value: "/other"})
		},
		"binary mount": func(d *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "replace", MountPath: "/ko-app/kamaji"}}
		},
		"foreign replicaset": func(_ *appsv1.Deployment, rs *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			rs.OwnerReferences[0].UID = "old-deployment"
		},
		"stale replicaset": func(_ *appsv1.Deployment, rs *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			rs.Status.ObservedGeneration = 0
		},
		"replicaset template mismatch": func(_ *appsv1.Deployment, rs *appsv1.ReplicaSet, _ *corev1.Pod, _ *corev1.Node) {
			rs.Spec.Template.Spec.ServiceAccountName = "other"
		},
		"foreign pod": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			p.OwnerReferences[0].UID = "old-rs"
		},
		"deleting pod": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		},
		"unready pod": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			p.Status.Conditions[0].Status = corev1.ConditionFalse
		},
		"unready manager": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			p.Status.ContainerStatuses[0].Ready = false
		},
		"unknown runtime": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			p.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("a", 64)
		},
		"wrong runtime repo": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, p *corev1.Pod, _ *corev1.Node) {
			p.Status.ContainerStatuses[0].ImageID = strings.ReplaceAll(p.Status.ContainerStatuses[0].ImageID, "clastix", "other")
		},
		"arm64": func(_ *appsv1.Deployment, _ *appsv1.ReplicaSet, _ *corev1.Pod, n *corev1.Node) {
			n.Status.NodeInfo.Architecture = "arm64"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, d, rs, pod, node, _ := kamajiFixture(t)
			mutate(d, rs, pod, node)
			got := (Collector{Kubernetes: fake.NewSimpleClientset(d, rs, pod, node)}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if got.State == "passed" {
				t.Fatalf("unsafe producer accepted: %+v", got)
			}
		})
	}
}

func TestKamajiCompatibilityCompleteCoverageAndFences(t *testing.T) {
	for _, resource := range []string{"deployments", "replicasets", "pods", "nodes"} {
		t.Run("denied "+resource, func(t *testing.T) {
			client, d, _, _, _, _ := kamajiFixture(t)
			client.PrependReactor("*", resource, func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", fmt.Errorf("denied"))
			})
			got := (Collector{Kubernetes: client}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if got.State == "passed" {
				t.Fatal(got)
			}
		})
	}
	for _, kind := range []string{"deployment replacement", "pod replacement", "process change", "pagination", "mixed old pod", "ambiguous deployment", "missing pod", "missing deployment", "heartbeat"} {
		t.Run(kind, func(t *testing.T) {
			client, d, rs, pod, node, _ := kamajiFixture(t)
			calls := 0
			client.PrependReactor("list", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
				calls++
				copy := d.DeepCopy()
				if calls > 1 {
					switch kind {
					case "deployment replacement":
						copy.UID = "replacement"
					case "process change":
						copy.Spec.Template.Annotations = map[string]string{"changed": "yes"}
					case "heartbeat":
						copy.ResourceVersion = "999"
					}
				}
				items := []appsv1.Deployment{*copy}
				if kind == "ambiguous deployment" {
					other := copy.DeepCopy()
					other.Name = "another"
					items = append(items, *other)
				}
				if kind == "missing deployment" {
					items = nil
				}
				list := &appsv1.DeploymentList{Items: items}
				if kind == "pagination" {
					list.Continue = "repeated"
				}
				return true, list, nil
			})
			if kind == "pod replacement" || kind == "missing pod" || kind == "mixed old pod" {
				podCalls := 0
				client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					podCalls++
					copy := pod.DeepCopy()
					if podCalls > 1 && kind == "pod replacement" {
						copy.UID = "replacement"
					}
					items := []corev1.Pod{*copy}
					if kind == "missing pod" {
						items = nil
					}
					if kind == "mixed old pod" {
						old := copy.DeepCopy()
						old.Name = "old"
						old.UID = "old"
						old.OwnerReferences[0].UID = "old-rs"
						items = append(items, *old)
					}
					return true, &corev1.PodList{Items: items}, nil
				})
			}
			_ = rs
			_ = node
			got := (Collector{Kubernetes: client}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if (got.State == "passed") != (kind == "heartbeat") {
				t.Fatalf("%s: %+v", kind, got)
			}
		})
	}
	client, d, _, _, _, _ := kamajiFixture(t)
	for _, release := range []string{"", "unknown", "26.4.0"} {
		if got := (Collector{Kubernetes: client}).CollectKamajiCompatibility(context.Background(), d.Namespace, release); got.State == "passed" {
			t.Fatal(got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := (Collector{Kubernetes: client}).CollectKamajiCompatibility(ctx, d.Namespace, "26.8.0"); got.State == "passed" {
		t.Fatal(got)
	}
}

func TestKamajiCompatibilityRecognizesOnlyStandardServiceAccountAdmission(t *testing.T) {
	for _, scenario := range []string{"standard", "unexpected token audience", "unexpected mount", "changed service account", "changed template volume"} {
		t.Run(scenario, func(t *testing.T) {
			_, d, rs, pod, node, _ := kamajiFixture(t)
			d.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			rs.Spec.Template = *d.Spec.Template.DeepCopy()
			pod.Spec.Volumes = append([]corev1.Volume{}, d.Spec.Template.Spec.Volumes...)
			tokenName := "kube-api-access-abc12"
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: tokenName, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: kptr.To(int32(420)), Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: kptr.To(int64(3607))}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
			}}}})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: tokenName, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
			switch scenario {
			case "unexpected token audience":
				pod.Spec.Volumes[1].Projected.Sources[0].ServiceAccountToken.Audience = "other"
			case "unexpected mount":
				pod.Spec.Containers[0].VolumeMounts[0].MountPath = "/ko-app"
			case "changed service account":
				pod.Spec.ServiceAccountName = "other"
			case "changed template volume":
				pod.Spec.Volumes[0].EmptyDir = nil
				pod.Spec.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/other"}
			}
			got := (Collector{Kubernetes: fake.NewSimpleClientset(d, rs, pod, node)}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if (got.State == "passed") != (scenario == "standard") {
				t.Fatalf("%s: %+v", scenario, got)
			}
		})
	}
}

// This fixture retains the actual API-defaulted Deployment/Pod shape from the
// reviewed chart, with dynamic site identity removed. Only its manager image is
// substituted for the source/artifact-qualified candidate; admission is real.
func TestKamajiCompatibilityObservedAdmission(t *testing.T) {
	data, err := os.ReadFile("testdata/kamaji-observed-pod-specs.json")
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Template corev1.PodSpec `json:"template"`
		Pod      corev1.PodSpec `json:"pod"`
	}
	if err = json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	_, d, rs, pod, node, rule := kamajiFixture(t)
	observed.Template.Containers[0].Image = rule.Image
	observed.Pod.Containers[0].Image = rule.Image
	d.Spec.Template.Spec = observed.Template
	rs.Spec.Template = *d.Spec.Template.DeepCopy()
	pod.Spec = observed.Pod
	got := (Collector{Kubernetes: fake.NewSimpleClientset(d, rs, pod, node)}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
	if got.State != "passed" {
		t.Fatalf("observed standard management admission rejected: %+v", got)
	}
}

func TestCouncilUnexpectedProcesses(t *testing.T) {
	for _, name := range []string{"dns", "security-context", "sidecar", "init-container", "ephemeral-container", "host-network", "host-pid", "host-ipc", "shared-process", "lifecycle", "runtime-class", "service-links", "priority", "preemption", "extra-toleration", "changed-eviction", "wrong-node", "wrong-sa-alias", "template-sidecar", "template-init", "template-ephemeral"} {
		t.Run(name, func(t *testing.T) {
			_, d, rs, p, n, _ := kamajiFixture(t)
			extra := corev1.Container{Name: "unexpected", Image: "other/image:latest"}
			switch name {
			case "dns":
				p.Spec.DNSPolicy = corev1.DNSNone
				p.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{"192.0.2.53"}}
			case "security-context":
				p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: kptr.To(int64(0))}
			case "sidecar":
				p.Spec.Containers = append(p.Spec.Containers, extra)
			case "init-container":
				p.Spec.InitContainers = []corev1.Container{extra}
			case "ephemeral-container":
				p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "unexpected", Image: "other/image:latest"}}}
			case "host-network":
				p.Spec.HostNetwork = true
			case "host-pid":
				p.Spec.HostPID = true
			case "host-ipc":
				p.Spec.HostIPC = true
			case "shared-process":
				p.Spec.ShareProcessNamespace = kptr.To(true)
			case "lifecycle":
				p.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "unexpected"}}}}
			case "runtime-class":
				p.Spec.RuntimeClassName = kptr.To("unexpected")
			case "service-links":
				p.Spec.EnableServiceLinks = kptr.To(false)
			case "priority":
				p.Spec.Priority = kptr.To(int32(1))
			case "preemption":
				p.Spec.PreemptionPolicy = kptr.To(corev1.PreemptNever)
			case "extra-toleration":
				p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
			case "changed-eviction":
				p.Spec.Tolerations[0].TolerationSeconds = kptr.To(int64(301))
			case "wrong-node":
				d.Spec.Template.Spec.NodeName = "other-node"
				rs.Spec.Template = *d.Spec.Template.DeepCopy()
			case "wrong-sa-alias":
				p.Spec.DeprecatedServiceAccount = "other-sa"
			case "template-sidecar":
				d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, extra)
			case "template-init":
				d.Spec.Template.Spec.InitContainers = []corev1.Container{extra}
			case "template-ephemeral":
				d.Spec.Template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "unexpected", Image: "other/image:latest"}}}
			}
			if strings.HasPrefix(name, "template-") {
				rs.Spec.Template = *d.Spec.Template.DeepCopy()
				p.Spec = *d.Spec.Template.Spec.DeepCopy()
				p.Spec.NodeName = n.Name
			}
			got := (Collector{Kubernetes: fake.NewSimpleClientset(d, rs, p, n)}).CollectKamajiCompatibility(context.Background(), d.Namespace, "26.8.0")
			if got.State == "passed" {
				t.Fatalf("unreviewed Pod/process accepted: %+v", got)
			}
		})
	}
}
