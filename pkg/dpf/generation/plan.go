// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"bytes"
	"fmt"
	"io"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Plan is a validated, caller-isolated view of one retained HBN generation.
// Its fields are private; accessors return copies so later lifecycle steps
// cannot mutate the source bundle or another consumer's view.
type Plan struct {
	digest          string
	intent          ResolvedIntent
	operator        bootstrap.OperatorRenderInputs
	operatorArchive []byte
	hbnChart        []byte
	stages          []bundle.StageView
}

// LoadPlan strictly decodes and verifies the retained plan before a caller
// performs any API access. It reconstructs explicit inputs from retained
// metadata and manifests, reruns the existing generator, and requires an exact
// bundle match; it does not re-resolve source defaults or access credentials.
func LoadPlan(retained *bundle.Bundle) (*Plan, error) {
	if retained == nil {
		return nil, fmt.Errorf("retained DPF bundle is required")
	}
	index := retained.Index()
	files := retained.Files()
	resolvedFile, ok := findFile(files, bundle.ResolvedConfigPath)
	if !ok || recordRole(index, bundle.ResolvedConfigPath) != bundle.ResolvedConfig {
		return nil, fmt.Errorf("retained DPF resolved configuration is missing")
	}
	intent, err := decodeResolvedIntent(resolvedFile.Data)
	if err != nil {
		return nil, fmt.Errorf("decode retained DPF intent: %w", err)
	}
	if err := validateIntentIndex(intent, index); err != nil {
		return nil, err
	}
	fileMap := make(map[string]bundle.File, len(files))
	for _, file := range files {
		fileMap[file.Path] = file
	}
	operatorArchive, err := requiredArtifact(fileMap, index, intent.OperatorChart, bundle.Chart)
	if err != nil {
		return nil, fmt.Errorf("retained operator chart: %w", err)
	}
	operatorValues, err := requiredArtifact(fileMap, index, intent.OperatorValues, bundle.HelmValues)
	if err != nil {
		return nil, fmt.Errorf("retained operator values: %w", err)
	}
	hbnArchive, err := requiredArtifact(fileMap, index, intent.HBNChart, bundle.Chart)
	if err != nil {
		return nil, fmt.Errorf("retained HBN chart: %w", err)
	}
	values, err := helmclient.UnmarshalValues(operatorValues.Data)
	if err != nil {
		return nil, fmt.Errorf("decode retained operator values: %w", err)
	}
	flavor, resources, err := retainedWorkloadInputs(retained)
	if err != nil {
		return nil, err
	}
	source, ok := findMetadata(index, files, ".l8k/source-config.yaml")
	if !ok {
		return nil, fmt.Errorf("retained source configuration is missing")
	}
	input := inputsFromIntent(intent, values, operatorArchive.Data, hbnArchive.Data, source.Data, flavor, resources)
	expected, err := Generate(input)
	if err != nil {
		return nil, fmt.Errorf("validate retained DPF generation: %w", err)
	}
	if err := compareBundles(expected, retained); err != nil {
		return nil, err
	}
	stages := make([]bundle.StageView, 0, len(index.Stages))
	for _, stage := range index.Stages {
		view, exists := retained.Stage(stage.ID)
		if !exists {
			return nil, fmt.Errorf("retained DPF stage %q is unavailable", stage.ID)
		}
		stages = append(stages, view)
	}
	return &Plan{
		digest:          index.BundleDigest,
		intent:          cloneResolvedIntent(intent),
		operator:        input.Operator,
		operatorArchive: bytes.Clone(operatorArchive.Data),
		hbnChart:        bytes.Clone(hbnArchive.Data),
		stages:          stages,
	}, nil
}

// Intent returns a deep copy of the retained effective inputs.
func (p *Plan) Intent() ResolvedIntent {
	if p == nil {
		return ResolvedIntent{}
	}
	return cloneResolvedIntent(p.intent)
}

// Operator returns copied explicit Helm inputs and the exact retained chart.
func (p *Plan) Operator() (bootstrap.OperatorRenderInputs, []byte) {
	if p == nil {
		return bootstrap.OperatorRenderInputs{}, nil
	}
	input := p.operator
	input.Values = cloneAnyMap(input.Values)
	input.Bootstrap = cloneBootstrapInputs(input.Bootstrap)
	return input, bytes.Clone(p.operatorArchive)
}

// HBNChart returns a copy of the exact retained workload chart archive.
func (p *Plan) HBNChart() []byte {
	if p == nil {
		return nil
	}
	return bytes.Clone(p.hbnChart)
}

// Stages returns ordered immutable bundle views for the validated plan.
func (p *Plan) Stages() []bundle.StageView {
	if p == nil {
		return nil
	}
	return append([]bundle.StageView(nil), p.stages...)
}

// Digest is the retained bundle digest validated by LoadPlan.
func (p *Plan) Digest() string {
	if p == nil {
		return ""
	}
	return p.digest
}

func decodeResolvedIntent(data []byte) (ResolvedIntent, error) {
	var envelope struct {
		FormatVersion string         `yaml:"formatVersion"`
		Target        string         `yaml:"target"`
		Config        ResolvedIntent `yaml:"config"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&envelope); err != nil {
		return ResolvedIntent{}, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ResolvedIntent{}, fmt.Errorf("multiple YAML documents are unsupported")
		}
		return ResolvedIntent{}, err
	}
	if envelope.FormatVersion != bundle.FormatVersion || envelope.Target != bundle.Target {
		return ResolvedIntent{}, fmt.Errorf("unsupported resolved DPF format or target")
	}
	return envelope.Config, nil
}

func validateIntentIndex(intent ResolvedIntent, index bundle.Index) error {
	if intent.UseCase != "hbn-bgp" || intent.Release == "" || intent.Namespace == "" {
		return fmt.Errorf("retained HBN intent is incomplete")
	}
	if index.Profile != intent.UseCase || index.Release != intent.Release {
		return fmt.Errorf("bundle profile or release differs from retained intent")
	}
	if intent.Bootstrap.OperatorNamespace == "" || intent.Bootstrap.OperatorReleaseName == "" {
		return fmt.Errorf("retained operator namespace or release name is missing; regenerate the bundle")
	}
	if intent.Bootstrap.OperatorNamespace != intent.Namespace {
		return fmt.Errorf("operator and workload namespaces differ in retained intent")
	}
	serials := make([]string, 0, len(intent.Devices))
	for _, device := range intent.Devices {
		if device.BMCIP == "" || device.BMCPort < 1 || device.BMCPort > 65535 {
			return fmt.Errorf("retained selected device enrollment endpoint is missing; regenerate the bundle")
		}
		serials = append(serials, device.Serial)
	}
	if !reflect.DeepEqual(serials, index.SelectedDevices) {
		return fmt.Errorf("selected device identities differ from retained intent")
	}
	return nil
}

func inputsFromIntent(intent ResolvedIntent, values map[string]any, operatorArchive, hbnArchive, source []byte, flavor, resources map[string]any) Inputs {
	devices := make([]bootstrap.Device, 0, len(intent.Devices))
	workloadDevices := make([]hbnrender.Device, 0, len(intent.Devices))
	for _, device := range intent.Devices {
		devices = append(devices, bootstrap.Device{Serial: device.Serial, BMCIP: device.BMCIP, BMCPort: device.BMCPort})
		workloadDevices = append(workloadDevices, hbnrender.Device{
			Serial: device.Serial, PSID: device.PSID, ASN: device.ASN,
			SelectorLabels: cloneLabels(device.SelectorLabels),
		})
	}
	bootstrapInputs := bootstrap.Inputs{
		OperatorNamespace:     intent.Bootstrap.OperatorNamespace,
		OperatorConfigName:    intent.Bootstrap.OperatorConfigName,
		BMCFactoryResetPolicy: intent.Bootstrap.BMCFactoryResetPolicy,
		ManagementAPIIP:       intent.Bootstrap.ManagementAPIIP, ManagementAPIPort: intent.Bootstrap.ManagementAPIPort,
		ArtifactPVC: intent.Bootstrap.ArtifactPVC, PodCIDR: intent.Bootstrap.PodCIDR,
		Tenant: cloneTenant(intent.Bootstrap.Tenant), Devices: devices,
	}
	return Inputs{
		Operator: bootstrap.OperatorRenderInputs{
			ReleaseVersion: intent.Release, ReleaseName: intent.Bootstrap.OperatorReleaseName,
			KubernetesVersion: intent.KubernetesVersion, Values: cloneAnyMap(values), Bootstrap: bootstrapInputs,
		},
		OperatorArchive: bytes.Clone(operatorArchive),
		Workload: hbnrender.Inputs{
			Namespace: intent.Namespace, Generation: intent.Generation, Profile: intent.Networking,
			Software: cloneSoftware(intent.Software), Devices: workloadDevices,
			DPUClusterSelector: cloneLabels(intent.Bootstrap.Tenant.Labels),
			FlavorSpec:         cloneAnyMap(flavor), HBNResources: cloneAnyMap(resources),
		},
		HBNChartArchive: bytes.Clone(hbnArchive), SourceConfig: bytes.Clone(source), Management: intent.Management,
	}
}

func retainedWorkloadInputs(retained *bundle.Bundle) (map[string]any, map[string]any, error) {
	var flavor, resources map[string]any
	flavors, templates := 0, 0
	for _, stage := range retained.Index().Stages {
		view, ok := retained.Stage(stage.ID)
		if !ok {
			return nil, nil, fmt.Errorf("retained DPF stage %q is unavailable", stage.ID)
		}
		for _, document := range view.Documents() {
			object := document.ObjectCopy()
			switch object.GetKind() {
			case "DPUFlavor":
				flavors++
				flavor, _, _ = unstructured.NestedMap(object.Object, "spec")
			case "DPUServiceTemplate":
				templates++
				resources, _, _ = unstructured.NestedMap(object.Object, "spec", "helmChart", "values", "resources")
			}
		}
	}
	if flavors != 1 || len(flavor) == 0 || templates != 1 || len(resources) == 0 {
		return nil, nil, fmt.Errorf("retained workload is missing its unique DPUFlavor or HBN resource inputs")
	}
	return flavor, resources, nil
}

func compareBundles(expected, retained *bundle.Bundle) error {
	if !reflect.DeepEqual(expected.Index(), retained.Index()) {
		return fmt.Errorf("retained DPF bundle stages, references, prerequisites, or index differ from regenerated intent")
	}
	want, got := expected.Files(), retained.Files()
	if len(want) != len(got) {
		return fmt.Errorf("retained DPF bundle file set differs from regenerated intent")
	}
	for i := range want {
		if want[i].Path != got[i].Path || !bytes.Equal(want[i].Data, got[i].Data) {
			return fmt.Errorf("retained DPF artifact %q differs from regenerated intent", got[i].Path)
		}
	}
	return nil
}

func requiredArtifact(files map[string]bundle.File, index bundle.Index, artifact ResolvedArtifact, role bundle.Role) (bundle.File, error) {
	file, ok := files[artifact.Path]
	if artifact.Path == "" || !ok || recordRole(index, artifact.Path) != role {
		return bundle.File{}, fmt.Errorf("path or role does not match retained intent")
	}
	if artifact.SHA256 == "" || digest(file.Data) != artifact.SHA256 {
		return bundle.File{}, fmt.Errorf("content digest does not match retained intent")
	}
	return file, nil
}

func findMetadata(index bundle.Index, files []bundle.File, name string) (bundle.File, bool) {
	for _, record := range index.Files {
		if record.Path == name && record.Role == bundle.Metadata {
			return findFile(files, name)
		}
	}
	return bundle.File{}, false
}

func findFile(files []bundle.File, name string) (bundle.File, bool) {
	for _, file := range files {
		if file.Path == name {
			return file, true
		}
	}
	return bundle.File{}, false
}

func recordRole(index bundle.Index, name string) bundle.Role {
	for _, record := range index.Files {
		if record.Path == name {
			return record.Role
		}
	}
	return ""
}

func cloneResolvedIntent(input ResolvedIntent) ResolvedIntent {
	output := input
	output.Software = cloneSoftware(input.Software)
	output.Bootstrap.Tenant = cloneTenant(input.Bootstrap.Tenant)
	output.Devices = make([]ResolvedDevice, len(input.Devices))
	for i, device := range input.Devices {
		output.Devices[i] = device
		output.Devices[i].SelectorLabels = cloneLabels(device.SelectorLabels)
	}
	return output
}

func cloneSoftware(input config.Software) config.Software {
	output := input
	output.PLDM = make(map[string]config.Artifact, len(input.PLDM))
	for key, artifact := range input.PLDM {
		output.PLDM[key] = artifact
	}
	return output
}

func cloneTenant(input bootstrap.Tenant) bootstrap.Tenant {
	output := input
	output.Labels = cloneLabels(input.Labels)
	output.NodeSelector = cloneLabels(input.NodeSelector)
	return output
}

func cloneBootstrapInputs(input bootstrap.Inputs) bootstrap.Inputs {
	output := input
	output.Tenant = cloneTenant(input.Tenant)
	output.Devices = append([]bootstrap.Device(nil), input.Devices...)
	return output
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneAny(value)
	}
	return output
}

func cloneAny(input any) any {
	switch value := input.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case []any:
		output := make([]any, len(value))
		for i := range value {
			output[i] = cloneAny(value[i])
		}
		return output
	case map[string]string:
		return cloneLabels(value)
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}
