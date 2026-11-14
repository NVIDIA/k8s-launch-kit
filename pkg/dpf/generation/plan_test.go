// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestLoadPlanRegeneratesExactRetainedBundleAndReturnsCopies(t *testing.T) {
	input := generationInputs(t)
	input.Operator.Values = map[string]any{"controllerManager": map[string]any{"replicas": 2}}
	input.SourceConfig = []byte("dpf:\n  useCase: hbn-bgp\n  artifactSource: ./relative/source.yaml\n")
	retained, err := Generate(input)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	plan, err := LoadPlan(retained)
	if err != nil {
		t.Fatalf("LoadPlan() error = %v", err)
	}
	if plan.Digest() != retained.Index().BundleDigest {
		t.Fatalf("plan digest = %q, want %q", plan.Digest(), retained.Index().BundleDigest)
	}
	intent := plan.Intent()
	if intent.Bootstrap.OperatorReleaseName != input.Operator.ReleaseName || intent.Bootstrap.OperatorNamespace != input.Operator.Bootstrap.OperatorNamespace {
		t.Fatalf("retained operator identity = %s/%s", intent.Bootstrap.OperatorNamespace, intent.Bootstrap.OperatorReleaseName)
	}
	if intent.KubernetesVersion != input.Operator.KubernetesVersion || intent.Bootstrap.TenantKubernetesVersion != "v1.35.6" {
		t.Fatalf("management/tenant Kubernetes versions = %q/%q, want %q/v1.35.6", intent.KubernetesVersion, intent.Bootstrap.TenantKubernetesVersion, input.Operator.KubernetesVersion)
	}
	if len(intent.Devices) != 1 || intent.Devices[0].Serial != "SERIAL-A" || intent.Devices[0].PSID != "MT_0000001774" || intent.Devices[0].BMCIP != "192.0.2.20" || intent.Devices[0].BMCPort != 443 {
		t.Fatalf("retained selected device = %#v", intent.Devices)
	}
	operator, archive := plan.Operator()
	if operator.ReleaseName != input.Operator.ReleaseName || operator.Bootstrap.OperatorNamespace != input.Operator.Bootstrap.OperatorNamespace || fmt.Sprint(operator.Values["controllerManager"].(map[string]any)["replicas"]) != "2" {
		t.Fatalf("retained operator inputs = %#v", operator)
	}
	if !bytes.Equal(archive, input.OperatorArchive) || !bytes.Equal(plan.HBNChart(), input.HBNChartArchive) {
		t.Fatal("plan chart accessors did not return exact retained archives")
	}
	if len(plan.Stages()) != 7 {
		t.Fatalf("retained stage count = %d, want seven", len(plan.Stages()))
	}

	intent.Devices[0].SelectorLabels["inventory.example/serial-hash"] = "mutated"
	intent.Software.PLDM["MT_0000001774"] = structArtifact()
	operator.Values["controllerManager"].(map[string]any)["replicas"] = 99
	operator.Bootstrap.Tenant.Labels["launch-kit.nvidia.com/dpf-tenant"] = "mutated"
	operator.Bootstrap.Devices[0].BMCIP = "192.0.2.99"
	archive[0] ^= 0xff
	hbnArchive := plan.HBNChart()
	hbnArchive[0] ^= 0xff
	stages := plan.Stages()
	stage := stages[0].Stage()
	stage.Files[0] = "mutated"
	for _, document := range stages[5].Documents() {
		object := document.ObjectCopy()
		if object.GetKind() == "DPUFlavor" {
			object.SetName("mutated")
		}
	}
	again := plan.Intent()
	operatorAgain, archiveAgain := plan.Operator()
	if again.Devices[0].SelectorLabels["inventory.example/serial-hash"] != "serial-a" || again.Software.PLDM["MT_0000001774"].URL != input.Workload.Software.PLDM["MT_0000001774"].URL {
		t.Fatal("Intent() returned aliases into the retained plan")
	}
	if fmt.Sprint(operatorAgain.Values["controllerManager"].(map[string]any)["replicas"]) != "2" || operatorAgain.Bootstrap.Tenant.Labels["launch-kit.nvidia.com/dpf-tenant"] != input.Operator.Bootstrap.Tenant.Labels["launch-kit.nvidia.com/dpf-tenant"] || operatorAgain.Bootstrap.Devices[0].BMCIP != "192.0.2.20" || !bytes.Equal(archiveAgain, input.OperatorArchive) || !bytes.Equal(plan.HBNChart(), input.HBNChartArchive) {
		t.Fatal("Operator() or HBNChart() returned aliases into the retained plan")
	}
	if !reflect.DeepEqual(stages[0].Stage().Files, plan.Stages()[0].Stage().Files) {
		t.Fatal("Stages() returned mutable stage metadata")
	}
	for _, document := range plan.Stages()[5].Documents() {
		object := document.ObjectCopy()
		if object.GetKind() == "DPUFlavor" && object.GetName() == "mutated" {
			t.Fatal("Stages() returned an alias into the retained resource object")
		}
	}
}

func TestLoadPlanRejectsMalformedOrInconsistentRetainedBundles(t *testing.T) {
	base, err := Generate(generationInputs(t))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*bundle.Index, *[]bundle.File)
	}{
		{name: "unknown resolved field", mutate: func(_ *bundle.Index, files *[]bundle.File) {
			mutateFile(t, files, bundle.ResolvedConfigPath, func(data []byte) []byte {
				var root map[string]any
				if err := yaml.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				root["config"].(map[string]any)["unexpected"] = true
				return marshalYAML(t, root)
			})
		}},
		{name: "selected serial mismatch", mutate: func(index *bundle.Index, _ *[]bundle.File) {
			index.SelectedDevices = []string{"SERIAL-B"}
		}},
		{name: "selected BMC endpoint mismatch", mutate: func(_ *bundle.Index, files *[]bundle.File) {
			mutateFile(t, files, bundle.ResolvedConfigPath, func(data []byte) []byte {
				var root map[string]any
				if err := yaml.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				device := root["config"].(map[string]any)["devices"].([]any)[0].(map[string]any)
				device["bmcIP"] = "192.0.2.21"
				return marshalYAML(t, root)
			})
		}},
		{name: "tenant Kubernetes version missing", mutate: func(_ *bundle.Index, files *[]bundle.File) {
			mutateFile(t, files, bundle.ResolvedConfigPath, func(data []byte) []byte {
				var root map[string]any
				if err := yaml.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				delete(root["config"].(map[string]any)["bootstrap"].(map[string]any), "tenantKubernetesVersion")
				return marshalYAML(t, root)
			})
		}},
		{name: "tenant Kubernetes version tampered", mutate: func(_ *bundle.Index, files *[]bundle.File) {
			mutateFile(t, files, bundle.ResolvedConfigPath, func(data []byte) []byte {
				var root map[string]any
				if err := yaml.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				root["config"].(map[string]any)["bootstrap"].(map[string]any)["tenantKubernetesVersion"] = "v1.36.0"
				return marshalYAML(t, root)
			})
		}},
		{name: "stage prerequisite drift", mutate: func(index *bundle.Index, _ *[]bundle.File) {
			for i := range index.Stages {
				if index.Stages[i].ID == StageEnrollment {
					index.Stages[i].Requires = nil
				}
			}
		}},
		{name: "workload software drift", mutate: func(_ *bundle.Index, files *[]bundle.File) {
			mutateManifest(t, files, "BlueFieldSoftware", func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "https://other.example/os.iso", "spec", "osIso")
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := rehashBundle(t, base, test.mutate)
			if _, err := LoadPlan(mutated); err == nil {
				t.Fatal("LoadPlan() accepted an inconsistent retained bundle")
			}
		})
	}
}

func TestLoadPlanDeepCopiesTenantAllocationDeclarations(t *testing.T) {
	input := generationInputs(t)
	reserved, excluded, unique := true, false, true
	input.Operator.Bootstrap.Tenant.Allocation = &bootstrap.SiteAllocation{
		VIPReserved: &reserved, VIPExcludedFromDHCP: &excluded, RouterIDUniqueOnSelectedNodes: &unique,
	}
	retained, err := Generate(input)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	plan, err := LoadPlan(retained)
	if err != nil {
		t.Fatalf("LoadPlan() error = %v", err)
	}
	intent := plan.Intent()
	operator, _ := plan.Operator()
	if intent.Bootstrap.Tenant.Allocation == nil || operator.Bootstrap.Tenant.Allocation == nil {
		t.Fatal("retained plan dropped tenant allocation declarations")
	}
	if !*intent.Bootstrap.Tenant.Allocation.VIPReserved || *intent.Bootstrap.Tenant.Allocation.VIPExcludedFromDHCP || !*intent.Bootstrap.Tenant.Allocation.RouterIDUniqueOnSelectedNodes {
		t.Fatalf("retained intent declarations = %#v", intent.Bootstrap.Tenant.Allocation)
	}
	if !*operator.Bootstrap.Tenant.Allocation.VIPReserved || *operator.Bootstrap.Tenant.Allocation.VIPExcludedFromDHCP || !*operator.Bootstrap.Tenant.Allocation.RouterIDUniqueOnSelectedNodes {
		t.Fatalf("operator intent declarations = %#v", operator.Bootstrap.Tenant.Allocation)
	}

	reserved, excluded, unique = false, true, false
	operatorAgain, _ := plan.Operator()
	if !*plan.Intent().Bootstrap.Tenant.Allocation.VIPReserved || *operatorAgain.Bootstrap.Tenant.Allocation.VIPExcludedFromDHCP {
		t.Fatal("caller mutation of generation input changed the retained bundle")
	}
	*intent.Bootstrap.Tenant.Allocation.VIPReserved = false
	*operator.Bootstrap.Tenant.Allocation.RouterIDUniqueOnSelectedNodes = false
	operatorAgain, _ = plan.Operator()
	if !*plan.Intent().Bootstrap.Tenant.Allocation.VIPReserved || !*operatorAgain.Bootstrap.Tenant.Allocation.RouterIDUniqueOnSelectedNodes {
		t.Fatal("Intent() or Operator() returned allocation pointers into the retained plan")
	}
}

func TestBundleRejectsMultipleResolvedDocumentsBeforePlanLoad(t *testing.T) {
	base, err := Generate(generationInputs(t))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	index, files := base.Index(), base.Files()
	index.BundleDigest = ""
	for i := range index.Files {
		index.Files[i].SHA256 = ""
	}
	mutateFile(t, &files, bundle.ResolvedConfigPath, func(data []byte) []byte {
		return append(data, []byte("---\nformatVersion: v1alpha1\ntarget: dpf\nconfig: {}\n")...)
	})
	if _, err := bundle.New(index, files); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("bundle.New() with multiple resolved documents error = %v", err)
	}
}

func TestLoadPlanRejectsOldIntentMissingExplicitOperatorIdentity(t *testing.T) {
	base, err := Generate(generationInputs(t))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	mutated := rehashBundle(t, base, func(_ *bundle.Index, files *[]bundle.File) {
		mutateFile(t, files, bundle.ResolvedConfigPath, func(data []byte) []byte {
			var root map[string]any
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			config := root["config"].(map[string]any)
			bootstrap := config["bootstrap"].(map[string]any)
			delete(bootstrap, "operatorReleaseName")
			return marshalYAML(t, root)
		})
	})
	if _, err := LoadPlan(mutated); err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Fatalf("LoadPlan() with incomplete operator identity error = %v, want regenerate guidance", err)
	}
}

func rehashBundle(t *testing.T, source *bundle.Bundle, mutate func(*bundle.Index, *[]bundle.File)) *bundle.Bundle {
	t.Helper()
	index, files := source.Index(), source.Files()
	index.BundleDigest = ""
	for i := range index.Files {
		index.Files[i].SHA256 = ""
	}
	mutate(&index, &files)
	result, err := bundle.New(index, files)
	if err != nil {
		t.Fatalf("bundle.New(mutated bundle) error = %v", err)
	}
	return result
}

func mutateFile(t *testing.T, files *[]bundle.File, name string, mutate func([]byte) []byte) {
	t.Helper()
	for i := range *files {
		if (*files)[i].Path == name {
			(*files)[i].Data = mutate((*files)[i].Data)
			return
		}
	}
	t.Fatalf("file %q not found", name)
}

func mutateManifest(t *testing.T, files *[]bundle.File, kind string, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	for i := range *files {
		if recordRoleForPath((*files)[i].Path) != bundle.Manifest {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal((*files)[i].Data, &object.Object); err != nil {
			t.Fatal(err)
		}
		if object.GetKind() == kind {
			mutate(object)
			updated, err := sigsyaml.Marshal(object.Object)
			if err != nil {
				t.Fatal(err)
			}
			(*files)[i].Data = updated
			return
		}
	}
	t.Fatalf("manifest kind %q not found", kind)
}

func recordRoleForPath(path string) bundle.Role {
	if strings.HasPrefix(path, "workload/") || strings.HasPrefix(path, "bootstrap/") {
		return bundle.Manifest
	}
	return ""
}

func marshalYAML(t *testing.T, value any) []byte {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func structArtifact() config.Artifact {
	return config.Artifact{URL: "https://mutated.example/fw.pldm", SHA256: strings.Repeat("a", 64)}
}
