// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestDefaultedObjectUsesSelectedOperatorManifestAndDoesNotMutateInput(t *testing.T) {
	archive, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "operator", "dpf-operator-v26.8.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := bootstrap.OperatorRenderInputs{
		ReleaseVersion: "26.8.0", ReleaseName: "dpf-operator", KubernetesVersion: "1.35.4",
		Values: map[string]any{},
		Bootstrap: bootstrap.Inputs{
			OperatorNamespace: "dpf-system", OperatorConfigName: "dpf-config", BMCFactoryResetPolicy: bootstrap.BMCFactoryResetPolicyNever,
			ManagementAPIIP: "192.0.2.10", ManagementAPIPort: 6443, PodCIDR: "10.244.0.0/14",
			Tenant:  bootstrap.Tenant{Name: "tenant", Namespace: "tenant", Labels: map[string]string{"l8k.example/tenant": "hbn"}, VIP: "192.0.2.20", Interface: "eth0", RouterID: 126, NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}, MaxNodes: 1, ServiceCIDR: "10.96.0.0/16"},
			Devices: []bootstrap.Device{{Serial: "SERIAL-A", BMCIP: "192.0.2.100", BMCPort: 443}},
		},
	}
	rendered, err := bootstrap.RenderOperator(archive, inputs)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := NewObjectSchemas(rendered.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []resourceKey{{group: "operator.dpu.nvidia.com", kind: "DPFOperatorConfig"}, {group: "provisioning.dpu.nvidia.com", kind: "DPUCluster"}, {group: "provisioning.dpu.nvidia.com", kind: "DPUDiscovery"}, {group: "svc.dpu.nvidia.com", kind: "DPUServiceTemplate"}} {
		if _, found := schemas.byGVK[key]; !found {
			t.Errorf("selected operator schemas lack %s/%s", key.group, key.kind)
		}
	}
	bootstrapFiles, err := bootstrap.Render(inputs.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defaultsSeen := map[string]bool{}
	for _, file := range bootstrapFiles {
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(file.Data, &object.Object); err != nil {
			t.Fatal(err)
		}
		if object.GroupVersionKind().Group == "" {
			continue
		}
		defaulted, err := schemas.DefaultedCopy(object)
		if err != nil {
			t.Fatalf("selected schema rejected generated %s: %v", object.GetKind(), err)
		}
		switch object.GetKind() {
		case "DPFOperatorConfig":
			mtu, found, err := unstructured.NestedInt64(defaulted.Object, "spec", "networking", "controlPlaneMTU")
			if err != nil || !found || mtu != 1500 {
				t.Fatalf("DPFOperatorConfig default controlPlaneMTU=%d found=%t err=%v", mtu, found, err)
			}
			defaultsSeen[object.GetKind()] = true
		case "DPUDiscovery":
			interval, found, err := unstructured.NestedString(defaulted.Object, "spec", "scanInterval")
			if err != nil || !found || interval != "1h" {
				t.Fatalf("DPUDiscovery default scanInterval=%q found=%t err=%v", interval, found, err)
			}
			defaultsSeen[object.GetKind()] = true
		}
	}
	if !defaultsSeen["DPFOperatorConfig"] || !defaultsSeen["DPUDiscovery"] {
		t.Fatalf("bootstrap defaults were not exercised: %v", defaultsSeen)
	}
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	generationSettings, err := hbn.ResolveGeneration(hbn.GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := profile.AllocateASNs([]string{"SERIAL-A"})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := hbnrender.Render(hbnrender.Inputs{
		Namespace: "dpf-system", Profile: profile, Generation: generationSettings,
		Software:           config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: stringRepeat('a', 64)}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: stringRepeat('b', 64)}}},
		Devices:            []hbnrender.Device{{Serial: "SERIAL-A", PSID: "MT_0000001774", SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-a"}, ASN: assignments["SERIAL-A"]}},
		DPUClusterSelector: map[string]string{"inventory.example/tenant": "tenant-a"}, FlavorSpec: map[string]any{"dpuMode": "dpu"}, HBNResources: map[string]any{"memory": "6Gi", "nvidia.com/bf_sf": int64(4)},
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]bool{}
	for _, file := range workload {
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(file.Data, &object.Object); err != nil {
			t.Fatal(err)
		}
		if _, err := schemas.DefaultedCopy(object); err != nil {
			t.Fatalf("selected CRD schema rejected generated %s: %v", object.GetKind(), err)
		}
		groups[object.GroupVersionKind().Group] = true
	}
	if !groups["provisioning.dpu.nvidia.com"] || !groups["svc.dpu.nvidia.com"] {
		t.Fatalf("generated workload did not exercise pinned provisioning and svc schemas: %v", groups)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster",
		"metadata": map[string]any{"name": "tenant", "namespace": "tenant"},
		"spec":     map[string]any{"type": "kamaji", "clusterEndpoint": map[string]any{"keepalived": map[string]any{"vip": "192.0.2.20", "interface": "eth0", "virtualRouterID": int64(126), "nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""}}}},
	}}
	before := object.DeepCopy()
	got, err := DefaultedObject(rendered.Manifest, object)
	if err != nil {
		t.Fatalf("DefaultedObject() error = %v", err)
	}
	if maxNodes, found, err := unstructured.NestedInt64(got.Object, "spec", "maxNodes"); err != nil || !found || maxNodes != 1000 {
		t.Fatalf("defaulted maxNodes = %d, found %t, error %v; want 1000", maxNodes, found, err)
	}
	if !reflect.DeepEqual(object, before) {
		t.Fatal("DefaultedObject() mutated its input")
	}
}

func stringRepeat(character byte, count int) string {
	result := make([]byte, count)
	for i := range result {
		result[i] = character
	}
	return string(result)
}

func TestDefaultedObjectRejectsUnknownFieldsAndUnsupportedSchema(t *testing.T) {
	archive, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "operator", "dpf-operator-v26.8.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := bootstrap.RenderOperator(archive, bootstrap.OperatorRenderInputs{
		ReleaseVersion: "26.8.0", ReleaseName: "dpf-operator", KubernetesVersion: "1.35.4", Values: map[string]any{},
		Bootstrap: bootstrap.Inputs{OperatorNamespace: "dpf-system", OperatorConfigName: "dpf-config", BMCFactoryResetPolicy: bootstrap.BMCFactoryResetPolicyNever, ManagementAPIIP: "192.0.2.10", ManagementAPIPort: 6443, PodCIDR: "10.244.0.0/14", Tenant: bootstrap.Tenant{Name: "tenant", Namespace: "tenant", Labels: map[string]string{"l8k.example/tenant": "hbn"}, VIP: "192.0.2.20", Interface: "eth0", RouterID: 126, NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}, MaxNodes: 1, ServiceCIDR: "10.96.0.0/16"}, Devices: []bootstrap.Device{{Serial: "SERIAL-A", BMCIP: "192.0.2.100", BMCPort: 443}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster", "metadata": map[string]any{"name": "tenant", "namespace": "tenant"}, "spec": map[string]any{"type": "kamaji", "unrecognized": true}}}
	if _, err := DefaultedObject(rendered.Manifest, base); err == nil {
		t.Fatal("DefaultedObject() accepted a field that the API server would prune")
	}
	unsupported := base.DeepCopy()
	unsupported.SetAPIVersion("unknown.dpu.nvidia.com/v1alpha1")
	if _, err := DefaultedObject(rendered.Manifest, unsupported); err == nil {
		t.Fatal("DefaultedObject() accepted a GVK absent from the selected chart")
	}
}
