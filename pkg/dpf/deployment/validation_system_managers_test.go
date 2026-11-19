// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/go-cmp/cmp"
	"reflect"
	"slices"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func directManagerTestObserver(t *testing.T, pvc string, feature bool) *validationObserver {
	t.Helper()
	inputs := preflightGenerationInputs(t)
	inputs.Operator.Bootstrap.ArtifactPVC = pvc
	if feature {
		if inputs.Operator.Values == nil {
			inputs.Operator.Values = map[string]any{}
		}
		inputs.Operator.Values["controllerManager"] = map[string]any{"featureGates": map[string]any{"ConfigPortsOverHighSpeed": true}}
	}
	retained, err := generateFixtureBundle(inputs)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareManagementDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	return &validationObserver{desired: desired}
}

// Fixtures start from the independent stable public source manifests below,
// then apply the documented controller substitutions to fictional retained
// inputs. They do not call the production expected-manager compiler.
func directManagerSourceFixture(t *testing.T, o *validationObserver, role, pvc string, feature bool) *unstructured.Unstructured {
	t.Helper()
	source := map[string]string{"provisioning-controller": directManagerSourceProvisioning, "dpuservice-controller": directManagerSourceService, "kamaji-cluster-manager": directManagerSourceKamaji}[role]
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(source), &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Namespace = o.desired.operatorNamespace
	pod := &deployment.Spec.Template.Spec
	pod.ImagePullSecrets = nil
	pod.Tolerations = []corev1.Toleration{{Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpExists}, {Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpExists}}
	pod.Affinity.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node-role.kubernetes.io/master", Operator: corev1.NodeSelectorOpExists}}}, {MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.NodeSelectorOpExists}}}}}}
	release, err := releases.Resolve("26.8.0")
	if err != nil {
		t.Fatal(err)
	}
	manager := &pod.Containers[0]
	manager.Image = release.Operator.Image
	switch role {
	case "provisioning-controller":
		manager.Args = []string{"--leader-elect", "--v=3", "--dms-image=nvcr.io/nvidia/doca/hostdriver:v26.8.0", "--bfb-pvc=" + pvc, "--redfish-client-cert-dir=/etc/dpf/redfish-client-cert", "--dpu-install-interface=redfish", "--deployment-mode=zero-trust", fmt.Sprintf("--dms-pod-envs=KUBERNETES_SERVICE_HOST=%s,KUBERNETES_SERVICE_PORT=%d", o.desired.plan.Intent().Bootstrap.ManagementAPIIP, o.desired.plan.Intent().Bootstrap.ManagementAPIPort), "--max-dpu-parallel-installations=50", "--multi-dpu-operations-sync-wait-time=30s", "--max-unavailable-dpu-nodes=50", "--pre-install-agent-registration-timeout=30s", "--bfb-registry-load-balancer-address="}
		for _, entry := range []struct{ name, path string }{{"NODE_NAME", "spec.nodeName"}, {"NODE_IP", "status.hostIP"}, {"POD_NAME", "metadata.name"}, {"POD_NAMESPACE", "metadata.namespace"}} {
			manager.Env = append(manager.Env, corev1.EnvVar{Name: entry.name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: entry.path}}})
		}
		manager.Env = append(manager.Env, corev1.EnvVar{Name: "BFB_REGISTRY_IMAGE", Value: "nvcr.io/nvidia/doca/bfb-registry:v26.8.0"})
		pod.Containers[1].Image = "nvcr.io/nvidia/doca/bfb-registry:v26.8.0"
		for i := range pod.Volumes {
			if pod.Volumes[i].Name == "bfb-volume" {
				if pvc == "" {
					pod.Volumes[i].VolumeSource = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/nvidia/dpf/bfb", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}
				} else {
					pod.Volumes[i].PersistentVolumeClaim.ClaimName = pvc
				}
			}
		}
		if pvc == "" {
			pod.InitContainers = []corev1.Container{{Name: "prepare-local-storage", Image: "busybox:1.36", Command: []string{"sh", "-c", "mkdir -p /bfb && chown -R 65532:65532 /bfb"}, SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), Privileged: ptr.To(true)}, VolumeMounts: []corev1.VolumeMount{{Name: "bfb-volume", MountPath: "/bfb"}}}}
		}
	case "dpuservice-controller":
		manager.Args = []string{"--disable-dpu-ready-taints=false", "--disable-host-network-ready-noexecute-taints=true", fmt.Sprintf("--feature-gates=ConfigPortsOverHighSpeed=%t", feature)}
	case "kamaji-cluster-manager":
		manager.Args = append(manager.Args, "--keepalived-image=nvcr.io/nvidia/doca/dpf-keepalived:v26.8.0")
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: object}
}

func directManagerConfigReader(t *testing.T, o *validationObserver) *validationReadFake {
	t.Helper()
	current, err := o.desired.verifier.markedDesired(o.desired.configObject)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID("current-operator-config")
	current.SetResourceVersion("1")
	current.SetGeneration(1)
	current.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{conditionMap("Ready", "True", "Ready", 1)}}
	return &validationReadFake{get: func(_ context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		if wanted.GetKind() != "DPFOperatorConfig" || wanted.GetName() != current.GetName() || wanted.GetNamespace() != current.GetNamespace() {
			t.Fatalf("manager contract requested an unrelated object: %s %s", wanted.GetKind(), wanted.GetName())
		}
		return current.DeepCopy(), nil
	}}
}

func TestValidationDirectManagerAcceptsStableSourceDefaultsAndRetainedChoices(t *testing.T) {
	for _, test := range []struct {
		name, pvc string
		feature   bool
	}{{"defaults", "", false}, {"retained PVC and feature gate", "retained-artifacts", true}} {
		t.Run(test.name, func(t *testing.T) {
			o := directManagerTestObserver(t, test.pvc, test.feature)
			reader := directManagerConfigReader(t, o)
			for _, role := range []string{"provisioning-controller", "dpuservice-controller", "kamaji-cluster-manager"} {
				current := directManagerSourceFixture(t, o, role, test.pvc, test.feature)
				before := current.DeepCopy()
				if err := o.verifyValidationDirectManager(context.Background(), reader, current, role); err != nil {
					expected, _ := o.validationDirectManagerExpected(role)
					want, _ := normalizedManagerSpec(expected)
					got, _ := normalizedManagerSpec(current)
					normalizeSystemPod(&want.Template.Spec)
					normalizeSystemPod(&got.Template.Spec)
					canonicalDirectManagerPod(&want.Template.Spec)
					canonicalDirectManagerPod(&got.Template.Spec)
					wantData, _ := json.Marshal(want)
					gotData, _ := json.Marshal(got)
					var a, b any
					_ = json.Unmarshal(wantData, &a)
					_ = json.Unmarshal(gotData, &b)
					t.Fatalf("stable %s rejected: %#v\n%s", role, err, cmp.Diff(a, b))
				}
				if !reflect.DeepEqual(before, current) {
					t.Fatal("functional comparison mutated a live input")
				}
			}
		})
	}
}

func TestValidationDirectManagerRejectsSameImageFunctionalDrift(t *testing.T) {
	o := directManagerTestObserver(t, "", false)
	reader := directManagerConfigReader(t, o)
	for _, role := range []string{"provisioning-controller", "dpuservice-controller", "kamaji-cluster-manager"} {
		t.Run(role, func(t *testing.T) {
			baseline := directManagerSourceFixture(t, o, role, "", false)
			for _, test := range []struct {
				name   string
				change func(*corev1.PodSpec)
			}{
				{"same image sleep", func(p *corev1.PodSpec) { p.Containers[0].Command = []string{"/bin/sleep", "infinity"} }},
				{"unretained flags", func(p *corev1.PodSpec) {
					p.Containers[0].Args = append(p.Containers[0].Args, "--health-probe-bind-address=:1")
				}},
				{"injected environment", func(p *corev1.PodSpec) {
					p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: "untrusted.invalid"})
				}},
				{"environment from config", func(p *corev1.PodSpec) {
					p.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "unretained"}}}}
				}},
				{"credential volume changed", func(p *corev1.PodSpec) {
					for i := range p.Volumes {
						if p.Volumes[i].Name == "kube-api-access" {
							p.Volumes[i].Projected.Sources[0].ServiceAccountToken.Audience = "unretained"
						}
					}
				}},
				{"mount shadowing", func(p *corev1.PodSpec) {
					p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "kube-api-access", MountPath: "/usr/local/bin"})
				}},
				{"alternate service account", func(p *corev1.PodSpec) { p.ServiceAccountName = "unretained" }},
				{"exec lifecycle injection", func(p *corev1.PodSpec) {
					p.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "true"}}}}
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					var deployment appsv1.Deployment
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(baseline.Object, &deployment); err != nil {
						t.Fatal(err)
					}
					test.change(&deployment.Spec.Template.Spec)
					data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
					if err != nil {
						t.Fatal(err)
					}
					candidate := &unstructured.Unstructured{Object: data}
					err = o.verifyValidationDirectManager(context.Background(), reader, candidate, role)
					var failure *validationCheckError
					if !errors.As(err, &failure) || failure.state != validation.Failed {
						t.Fatalf("same-image functional mutation accepted or hidden: %v", err)
					}
				})
			}
		})
	}
}

func TestValidationDirectManagerPreservesAPIDefaultsAndRejectsAmbiguousLists(t *testing.T) {
	o := directManagerTestObserver(t, "", false)
	reader := directManagerConfigReader(t, o)
	current := directManagerSourceFixture(t, o, "provisioning-controller", "", false)
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &deployment); err != nil {
		t.Fatal(err)
	}
	normalizeManagerPodTemplate(&deployment.Spec.Template.Spec)
	normalizeSystemPod(&deployment.Spec.Template.Spec)
	slices.Reverse(deployment.Spec.Template.Spec.Containers[0].Env)
	slices.Reverse(deployment.Spec.Template.Spec.Containers[0].Args)
	slices.Reverse(deployment.Spec.Template.Spec.Volumes)
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.verifyValidationDirectManager(context.Background(), reader, &unstructured.Unstructured{Object: data}, "provisioning-controller"); err != nil {
		t.Fatalf("ordinary API defaults or named-list ordering rejected: %#v", err)
	}
	deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, deployment.Spec.Template.Spec.Containers[0].Env[0])
	data, _ = runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err := o.verifyValidationDirectManager(context.Background(), reader, &unstructured.Unstructured{Object: data}, "provisioning-controller"); err == nil {
		t.Fatal("duplicate environment name accepted with ambiguous precedence")
	}
}

func TestValidationDirectManagerRequiresCurrentRetainedConsumedConfig(t *testing.T) {
	o := directManagerTestObserver(t, "", false)
	current := directManagerSourceFixture(t, o, "dpuservice-controller", "", false)
	reader := directManagerConfigReader(t, o)
	base := reader.get
	reader.get = func(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		config, err := base(ctx, wanted)
		if err != nil {
			return nil, err
		}
		_ = unstructured.SetNestedField(config.Object, "host-trusted", "spec", "deploymentMode")
		return config, nil
	}
	if err := o.verifyValidationDirectManager(context.Background(), reader, current, "dpuservice-controller"); err == nil {
		t.Fatal("manager matching image/spec hid changed consumed operator configuration")
	}
	_ = unstructured.SetNestedField(o.desired.configObject.Object, "custom-bf-config", "spec", "provisioningController", "bfCFGTemplateConfigMap")
	err := o.verifyValidationDirectManager(context.Background(), reader, current, "provisioning-controller")
	var failure *validationCheckError
	if !errors.As(err, &failure) || failure.state != validation.Unverified || failure.reason != validation.ReasonUnsupported {
		t.Fatalf("custom unretained template was assumed supported: %v", err)
	}
}

func TestValidationDirectManagerProjectsExplicitRetainedControllerSettings(t *testing.T) {
	o := directManagerTestObserver(t, "", false)
	config := o.desired.configObject.Object
	for field, value := range map[string]any{"dmsTimeout": int64(7), "maxDPUParallelInstallations": int64(3), "multiDPUOperationsSyncWaitTime": "1m", "maxUnavailableDPUNodes": int64(1), "osInstallRetries": int64(3), "osInstallTimeout": "30m", "firmwareUpdateTimeout": "10m", "preInstallAgentRegistrationTimeout": "20s", "nodeEffectRemovalTimeout": "5m", "hostAgentDNSPolicy": "Default", "replicas": int64(1)} {
		if err := unstructured.SetNestedField(config, value, "spec", "provisioningController", field); err != nil {
			t.Fatal(err)
		}
	}
	_ = unstructured.SetNestedField(config, "https://registry.example.test", "spec", "provisioningController", "registry", "loadBalancerAddress")
	_ = unstructured.SetNestedStringSlice(config, []string{"retained-pull"}, "spec", "imagePullSecrets")
	resources := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("900m"), corev1.ResourceMemory: resource.MustParse("1Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}
	resourceMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&resources)
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedMap(config, resourceMap, "spec", "provisioningController", "controller", "resources")
	current := directManagerSourceFixture(t, o, "provisioning-controller", "", false)
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Spec.Replicas = ptr.To(int32(1))
	deployment.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "retained-pull"}}
	manager := &deployment.Spec.Template.Spec.Containers[0]
	manager.Resources = resources
	manager.Args = []string{"--leader-elect", "--v=3", "--dms-image=nvcr.io/nvidia/doca/hostdriver:v26.8.0", "--bfb-pvc=", "--redfish-client-cert-dir=/etc/dpf/redfish-client-cert", "--image-pull-secrets=retained-pull", "--dms-timeout=7", "--dpu-install-interface=redfish", "--deployment-mode=zero-trust", fmt.Sprintf("--dms-pod-envs=KUBERNETES_SERVICE_HOST=%s,KUBERNETES_SERVICE_PORT=%d", o.desired.plan.Intent().Bootstrap.ManagementAPIIP, o.desired.plan.Intent().Bootstrap.ManagementAPIPort), "--max-dpu-parallel-installations=3", "--multi-dpu-operations-sync-wait-time=1m0s", "--max-unavailable-dpu-nodes=1", "--os-install-timeout=30m0s", "--os-install-retries=3", "--firmware-update-timeout=10m0s", "--pre-install-agent-registration-timeout=20s", "--node-effect-removal-timeout=5m0s", "--hostagent-dns-policy=Default", "--bfb-registry-load-balancer-address=https://registry.example.test"}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.verifyValidationDirectManager(context.Background(), directManagerConfigReader(t, o), &unstructured.Unstructured{Object: data}, "provisioning-controller"); err != nil {
		t.Fatalf("explicit retained settings did not follow the documented controller mapping: %#v", err)
	}
}

// Stable public source Deployment fixtures at f1b50e85122a3df6047708e41a8203f8cdce03a1.

const directManagerSourceProvisioning = `apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app.kubernetes.io/component: manager
    app.kubernetes.io/part-of: dpf-provisioning-controller-manager
    dpu.nvidia.com/component: dpf-provisioning-controller-manager
  name: dpf-provisioning-controller-manager
  namespace: dpf-provisioning
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/part-of: dpf-provisioning-controller-manager
      dpu.nvidia.com/component: dpf-provisioning-controller-manager
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: manager
      labels:
        app.kubernetes.io/part-of: dpf-provisioning-controller-manager
        dpu.nvidia.com/component: dpf-provisioning-controller-manager
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - podAffinityTerm:
              labelSelector:
                matchLabels:
                  app.kubernetes.io/part-of: dpf-provisioning-controller-manager
                  dpu.nvidia.com/component: dpf-provisioning-controller-manager
              topologyKey: kubernetes.io/hostname
            weight: 100
      automountServiceAccountToken: false
      containers:
      - args:
        - --leader-elect
        - --v=3
        - --dms-image=example.com/doca-platform-foundation/dpf-provisioning-controller/hostdriver:v0.1.0
        - --bfb-pvc=bfb-pvc
        - --redfish-client-cert-dir=/etc/dpf/redfish-client-cert
        command:
        - /provisioning
        env:
        - name: GRPC_ENFORCE_ALPN_ENABLED
          value: "false"
        - name: PLDM_UNPACK_SOCKET_PATH
          value: /var/run/dpf/pldm-unpack.sock
        - name: PLDM_UNPACK_ENDPOINT
          value: /v1/unpack
        image: example.com/dpf-system:v0.1.0
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        name: manager
        ports:
        - containerPort: 9443
          name: webhook-server
          protocol: TCP
        - containerPort: 8080
          name: metrics
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: 2
            memory: 2Gi
          requests:
            cpu: 1
            memory: 1Gi
        securityContext:
          capabilities:
            drop:
            - ALL
          privileged: true
          runAsNonRoot: true
          runAsUser: 65532
        volumeMounts:
        - mountPath: /tmp/k8s-webhook-server/serving-certs
          name: cert
          readOnly: true
        - mountPath: /bfb
          name: bfb-volume
        - mountPath: /var/run/secrets/kubernetes.io/serviceaccount
          name: kube-api-access
          readOnly: true
        - mountPath: /var/run/dpf
          name: pldm-unpack-socket
        - mountPath: /etc/dpf/redfish-client-cert
          name: redfish-client-cert
          readOnly: true
        - mountPath: /etc/dpf/redfish-client-cert-bf4
          name: redfish-client-cert-bf4
          readOnly: true
      - args:
        - --socket-path=/var/run/dpf/pldm-unpack.sock
        - --script-path=/opt/pldm-unpack/fwpkg_unpack.py
        command:
        - /usr/local/bin/pldmunpackserver
        image: example.com/bfb-registry:v0.1.0
        livenessProbe:
          exec:
            command:
            - test
            - -S
            - /var/run/dpf/pldm-unpack.sock
          initialDelaySeconds: 5
          periodSeconds: 30
        name: pldmunpack
        readinessProbe:
          exec:
            command:
            - test
            - -S
            - /var/run/dpf/pldm-unpack.sock
          initialDelaySeconds: 3
          periodSeconds: 10
        resources:
          limits:
            cpu: 500m
            memory: 256Mi
          requests:
            cpu: 10m
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          runAsNonRoot: true
          runAsUser: 65532
        volumeMounts:
        - mountPath: /bfb
          name: bfb-volume
        - mountPath: /var/run/dpf
          name: pldm-unpack-socket
      imagePullSecrets:
      - name: dpf-provisioning-regcred
      serviceAccountName: dpf-provisioning-controller-manager
      terminationGracePeriodSeconds: 10
      volumes:
      - name: cert
        secret:
          defaultMode: 420
          secretName: webhook-server-cert
      - name: bfb-volume
        persistentVolumeClaim:
          claimName: bfb-pvc
      - emptyDir: {}
        name: pldm-unpack-socket
      - name: redfish-client-cert
        secret:
          defaultMode: 420
          optional: true
          secretName: dpf-provisioning-redfish-client-secret
      - name: redfish-client-cert-bf4
        secret:
          defaultMode: 420
          optional: true
          secretName: dpf-provisioning-redfish-client-secret-bf4
      - name: kube-api-access
        projected:
          defaultMode: 420
          sources:
          - serviceAccountToken:
              path: token
          - configMap:
              items:
              - key: ca.crt
                path: ca.crt
              name: kube-root-ca.crt
          - downwardAPI:
              items:
              - fieldRef:
                  apiVersion: v1
                  fieldPath: metadata.namespace
                path: namespace`

const directManagerSourceService = `apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app.kubernetes.io/component: manager
    app.kubernetes.io/part-of: dpuservice-controller-manager
    dpu.nvidia.com/component: dpuservice-controller-manager
  name: dpuservice-controller-manager
  namespace: dpuservice-controller-system
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/part-of: dpuservice-controller-manager
      dpu.nvidia.com/component: dpuservice-controller-manager
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: manager
      labels:
        app.kubernetes.io/part-of: dpuservice-controller-manager
        dpu.nvidia.com/component: dpuservice-controller-manager
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - podAffinityTerm:
              labelSelector:
                matchLabels:
                  app.kubernetes.io/part-of: dpuservice-controller-manager
                  dpu.nvidia.com/component: dpuservice-controller-manager
              topologyKey: kubernetes.io/hostname
            weight: 100
      automountServiceAccountToken: false
      containers:
      - command:
        - /dpuservice
        - --leader-elect
        image: example.com/dpf-system:v0.1.0
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        name: manager
        ports:
        - containerPort: 9443
          name: webhook-server
          protocol: TCP
        - containerPort: 8080
          name: metrics
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          privileged: false
        volumeMounts:
        - mountPath: /.cache/helm
          name: helm-cache
        - mountPath: /tmp/k8s-webhook-server/serving-certs
          name: cert
          readOnly: true
        - mountPath: /var/run/secrets/kubernetes.io/serviceaccount
          name: kube-api-access
          readOnly: true
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
      serviceAccountName: dpuservice-controller-manager
      terminationGracePeriodSeconds: 10
      volumes:
      - emptyDir: {}
        name: helm-cache
      - name: cert
        secret:
          defaultMode: 420
          secretName: dpuservice-webhook-server-cert
      - name: kube-api-access
        projected:
          defaultMode: 420
          sources:
          - serviceAccountToken:
              path: token
          - configMap:
              items:
              - key: ca.crt
                path: ca.crt
              name: kube-root-ca.crt
          - downwardAPI:
              items:
              - fieldRef:
                  apiVersion: v1
                  fieldPath: metadata.namespace
                path: namespace`

const directManagerSourceKamaji = `apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app.kubernetes.io/component: manager
    app.kubernetes.io/part-of: kamaji-cluster-manager
    dpu.nvidia.com/component: kamaji-cluster-manager
  name: kamaji-cm-controller-manager
  namespace: dpf-operator-system
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/part-of: kamaji-cluster-manager
      dpu.nvidia.com/component: kamaji-cluster-manager
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: manager
      labels:
        app.kubernetes.io/part-of: kamaji-cluster-manager
        dpu.nvidia.com/component: kamaji-cluster-manager
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - podAffinityTerm:
              labelSelector:
                matchLabels:
                  app.kubernetes.io/part-of: kamaji-cluster-manager
                  dpu.nvidia.com/component: kamaji-cluster-manager
              topologyKey: kubernetes.io/hostname
            weight: 100
      automountServiceAccountToken: false
      containers:
      - args:
        - --leader-elect
        command:
        - /kamaji-cluster-manager
        image: example.com/dpf-system:v0.1.0
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        name: manager
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 10m
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
        volumeMounts:
        - mountPath: /var/run/secrets/kubernetes.io/serviceaccount
          name: kube-api-access
          readOnly: true
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
      serviceAccountName: kamaji-cm-controller-manager
      terminationGracePeriodSeconds: 10
      volumes:
      - name: kube-api-access
        projected:
          defaultMode: 420
          sources:
          - serviceAccountToken:
              path: token
          - configMap:
              items:
              - key: ca.crt
                path: ca.crt
              name: kube-root-ca.crt
          - downwardAPI:
              items:
              - fieldRef:
                  apiVersion: v1
                  fieldPath: metadata.namespace
                path: namespace`
