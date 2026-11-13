// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"bytes"
	"context"
	"embed"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"
)

//go:embed testdata/doca-hbn-3.5.0.tgz
var hbnChartFixture embed.FS

func generationInputs(t *testing.T) Inputs {
	t.Helper()
	hbnArchive, err := hbnChartFixture.ReadFile("testdata/doca-hbn-3.5.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	operatorArchive, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "operator", "dpf-operator-v26.8.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := hbn.ResolveGeneration(hbn.GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := profile.AllocateASNs([]string{"SERIAL-A"})
	if err != nil {
		t.Fatal(err)
	}
	selected := []bootstrap.Device{{Serial: "SERIAL-A", BMCIP: "192.0.2.20", BMCPort: 443}}
	apiIP, apiPort, vip, tenantInterface := "192.0.2.10", 6443, "192.0.2.11", "bond0"
	bootstrapInputs, issues, err := config.ResolveBootstrap(&config.BootstrapOptions{
		ManagementAPIIP: &apiIP, ManagementAPIPort: &apiPort,
		Tenant: config.TenantOptions{VIP: &vip, Interface: &tenantInterface},
	}, "dpf-operator-system", "https://api.example.test/prefix", selected)
	if err != nil || len(issues) != 0 {
		t.Fatalf("ResolveBootstrap() = %#v, %v", issues, err)
	}
	return Inputs{
		Operator: bootstrap.OperatorRenderInputs{
			ReleaseVersion: "26.8.0", ReleaseName: "dpf-operator", KubernetesVersion: "1.35.4",
			Values: map[string]any{}, Bootstrap: bootstrapInputs,
		},
		OperatorArchive: operatorArchive,
		Workload: hbnrender.Inputs{
			Namespace: "dpf-operator-system", Generation: generation, Profile: profile,
			Software: config.Software{
				OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)},
				PLDM:  map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: strings.Repeat("b", 64)}},
			},
			Devices:            []hbnrender.Device{{Serial: "SERIAL-A", PSID: "MT_0000001774", SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-a"}, ASN: assignments["SERIAL-A"]}},
			DPUClusterSelector: cloneLabels(bootstrapInputs.Tenant.Labels),
			FlavorSpec:         map[string]any{"dpuMode": "dpu"},
			HBNResources:       map[string]any{"memory": "6Gi", "nvidia.com/bf_sf": "4"},
		},
		HBNChartArchive: hbnArchive,
		SourceConfig:    []byte("dpf:\n  useCase: hbn-bgp\n"),
		Management: ManagementIdentity{
			APIEndpoint: "https://api.example.test/prefix", SystemNamespaceUID: "system-uid-1", TrustDigest: "sha256:" + strings.Repeat("c", 64),
		},
	}
}

func TestGenerateRetainsSourcesAndComposesOrderedStages(t *testing.T) {
	input := generationInputs(t)
	before := cloneInputs(input)
	generated, err := Generate(input)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("Generate() mutated input values or archive bytes")
	}
	index := generated.Index()
	wantStages := []string{StageNamespaces, StageOperator, StageOperatorConfig, StageTenant, StageEnrollment, StageWorkload, StageDeployment}
	if len(index.Stages) != len(wantStages) {
		t.Fatalf("got %d stages, want %d", len(index.Stages), len(wantStages))
	}
	for i, stage := range index.Stages {
		if stage.ID != wantStages[i] {
			t.Errorf("stage %d = %q, want %q", i, stage.ID, wantStages[i])
		}
	}
	if len(index.ExternalPrerequisites) < 10 {
		t.Fatalf("generation omitted deployment-only gates: %#v", index.ExternalPrerequisites)
	}
	files := generated.Files()
	byPath := make(map[string][]byte, len(files))
	for _, file := range files {
		byPath[file.Path] = file.Data
	}
	if !bytes.Equal(byPath[".l8k/source-config.yaml"], input.SourceConfig) {
		t.Fatal("source configuration was not retained verbatim")
	}
	if !bytes.Equal(byPath["charts/doca-hbn-3.5.0.tgz"], input.HBNChartArchive) {
		t.Fatal("exact HBN chart archive bytes were not retained")
	}
	resolved := string(byPath[bundle.ResolvedConfigPath])
	if strings.Contains(resolved, "192.0.2.20") || strings.Contains(resolved, "password") || strings.Contains(resolved, "credentials") {
		t.Fatal("resolved configuration contains enrollment endpoint or credential material")
	}
	if !strings.Contains(resolved, "https://api.example.test/prefix") {
		t.Fatal("resolved configuration dropped the authenticated API proxy base path")
	}
	var config struct {
		Config struct {
			Generation hbn.Generation `yaml:"generation"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(byPath[bundle.ResolvedConfigPath], &config); err != nil {
		t.Fatal(err)
	}
	if config.Config.Generation.ChartDigest == "" {
		t.Fatal("resolved generation omitted chart digest")
	}

	// The exact chart renders with a flat resource map; the integration validator
	// checks both requests and limits on the actual rendered HBN container.
	workloadFiles, err := hbnrender.Render(input.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRenderedHBNChart(input.HBNChartArchive, input.Workload, workloadFiles); err != nil {
		t.Fatalf("render pinned HBN chart: %v", err)
	}
}

func TestGenerateRejectsInconsistentBindingsAndCharts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inputs)
		want   string
	}{
		{name: "serial set mismatch", mutate: func(in *Inputs) { in.Workload.Devices[0].Serial = "SERIAL-B" }, want: "exact same"},
		{name: "tenant selector mismatch", mutate: func(in *Inputs) { in.Workload.DPUClusterSelector["inventory.example/tenant"] = "other" }, want: "selector"},
		{name: "API query rejected", mutate: func(in *Inputs) { in.Management.APIEndpoint += "?token=hidden" }, want: "without credentials"},
		{name: "chart byte digest mismatch", mutate: func(in *Inputs) { in.HBNChartArchive[0] ^= 0xff }, want: "SHA-256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := generationInputs(t)
			test.mutate(&input)
			generated, err := Generate(input)
			if err == nil || !strings.Contains(err.Error(), test.want) || generated != nil {
				t.Fatalf("Generate() = %v, %v; want error containing %q and no bundle", generated, err, test.want)
			}
		})
	}
}

func TestGenerateValidatesCustomResourcesAgainstSelectedChartSchema(t *testing.T) {
	input := generationInputs(t)
	input.Workload.FlavorSpec = map[string]any{"dpuMode": int64(7)}
	generated, err := Generate(input)
	if err == nil || !strings.Contains(err.Error(), "selected DPF OpenAPI schema") || generated != nil {
		t.Fatalf("Generate() = %v, %v; want OpenAPI schema error", generated, err)
	}

	input = generationInputs(t)
	input.Workload.FlavorSpec["definitelyUnknownSetting"] = true
	generated, err = Generate(input)
	if err == nil || !strings.Contains(err.Error(), "would prune") || generated != nil {
		t.Fatalf("Generate() = %v, %v; want rejection of unknown fields that the selected CRD would prune", generated, err)
	}
}

func TestFlavorSchemaRejectsFieldsTheAPIWouldPrune(t *testing.T) {
	input := generationInputs(t)
	operator, err := bootstrap.RenderOperator(input.OperatorArchive, input.Operator)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := schemasFromOperatorManifest(operator.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	workload, err := hbnrender.Render(input.Workload)
	if err != nil {
		t.Fatal(err)
	}
	var flavor bundle.File
	for _, file := range workload {
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(file.Data, &object.Object); err != nil {
			t.Fatal(err)
		}
		if object.GetKind() == "DPUFlavor" {
			flavor = file
			break
		}
	}
	if flavor.Path == "" {
		t.Fatal("rendered workload has no DPUFlavor")
	}
	if err := validateCustomResourceFile(flavor, schemas); err != nil {
		t.Fatalf("valid DPUFlavor rejected: %v", err)
	}
	object := &unstructured.Unstructured{}
	if err := sigsyaml.Unmarshal(flavor.Data, &object.Object); err != nil {
		t.Fatal(err)
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		t.Fatalf("DPUFlavor spec missing: found=%t err=%v", found, err)
	}
	spec["definitelyUnknownSetting"] = true
	if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
		t.Fatal(err)
	}
	flavor.Data, err = yaml.Marshal(object.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCustomResourceFile(flavor, schemas); err == nil || !strings.Contains(err.Error(), "would prune") {
		t.Fatalf("unknown DPUFlavor setting error = %v, want prune-path rejection", err)
	}
}

func TestPublishIsAtomicAndIdempotent(t *testing.T) {
	input := generationInputs(t)
	generated, err := Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.MkdirTemp(".", ".generation-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	destination := filepath.Join(parent, "generation")
	first, err := Publish(context.Background(), generated, destination)
	if err != nil || first.Unchanged {
		t.Fatalf("first Publish() = %#v, %v", first, err)
	}
	loaded, err := bundle.Load(destination)
	if err != nil || loaded == nil || loaded.Index().BundleDigest != generated.Index().BundleDigest {
		t.Fatalf("bundle.Load() = %v, %v", loaded, err)
	}
	second, err := Publish(context.Background(), generated, destination)
	if err != nil || !second.Unchanged {
		t.Fatalf("idempotent Publish() = %#v, %v", second, err)
	}
	changedInput := generationInputs(t)
	changedInput.Workload.Generation.WorkloadGatewayIndex++
	changed, err := Generate(changedInput)
	if err == nil {
		// Increasing to an in-range value is valid and should create a distinct plan.
		_, err = Publish(context.Background(), changed, destination)
	}
	if err == nil || !strings.Contains(err.Error(), "destination differs") {
		t.Fatalf("publishing a different bundle error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, bundle.IndexPath)); err != nil {
		t.Fatalf("failed replacement damaged prior complete bundle: %v", err)
	}
}

func cloneInputs(in Inputs) Inputs {
	out := in
	out.OperatorArchive = bytes.Clone(in.OperatorArchive)
	out.HBNChartArchive = bytes.Clone(in.HBNChartArchive)
	out.SourceConfig = bytes.Clone(in.SourceConfig)
	out.Operator.Values = map[string]any{}
	for key, value := range in.Operator.Values {
		out.Operator.Values[key] = value
	}
	out.Operator.Bootstrap.Devices = append([]bootstrap.Device(nil), in.Operator.Bootstrap.Devices...)
	out.Operator.Bootstrap.Tenant.Labels = cloneLabels(in.Operator.Bootstrap.Tenant.Labels)
	out.Operator.Bootstrap.Tenant.NodeSelector = cloneLabels(in.Operator.Bootstrap.Tenant.NodeSelector)
	out.Workload.Devices = append([]hbnrender.Device(nil), in.Workload.Devices...)
	for i := range out.Workload.Devices {
		out.Workload.Devices[i].SelectorLabels = cloneLabels(in.Workload.Devices[i].SelectorLabels)
	}
	out.Workload.DPUClusterSelector = cloneLabels(in.Workload.DPUClusterSelector)
	out.Workload.FlavorSpec = cloneTestMap(in.Workload.FlavorSpec)
	out.Workload.HBNResources = cloneTestMap(in.Workload.HBNResources)
	out.Workload.Software.PLDM = map[string]config.Artifact{}
	for key, value := range in.Workload.Software.PLDM {
		out.Workload.Software.PLDM[key] = value
	}
	return out
}

func cloneTestMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
