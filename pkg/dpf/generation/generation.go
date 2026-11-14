// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package generation composes a reviewed, immutable DPF HBN generation plan.
// It performs no Kubernetes API, credential, BMC, or switch access.
package generation

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	kubeutilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	StageNamespaces     = "management-namespaces"
	StageOperator       = "dpf-operator-helm"
	StageOperatorConfig = "dpf-operator-config"
	StageTenant         = "dpu-tenant"
	StageEnrollment     = "selected-device-enrollment"
	StageWorkload       = "hbn-workload-prerequisites"
	StageDeployment     = "hbn-deployment-root"

	maxHBNChartBytes      = 16 << 20
	maxExpandedChartBytes = 64 << 20
	maxChartEntries       = 8192
	maxChartMetadataBytes = 64 << 10
)

const (
	PrereqManagementDependencies = "management-dependencies-ready"
	PrereqManagementRBAC         = "management-rbac-verified"
	PrereqManagementIdentity     = "management-identity-reverified"
	PrereqTenantEndpoint         = "tenant-vip-interface-vrid-verified"
	PrereqTenantStorage          = "tenant-storage-ready"
	PrereqBMCCredentials         = "selected-bmc-credentials-available"
	PrereqBMCIdentity            = "selected-bmc-identity-authenticated"
	PrereqTenantAPI              = "tenant-api-ready"
	PrereqDPUIdentity            = "selected-dpu-identities-reverified"
	PrereqProvisioningArtifacts  = "provisioning-artifact-bytes-verified"
	PrereqHBNChartSource         = "hbn-chart-source-matches-archive"
	PrereqFabricAndHost          = "selected-fabric-and-host-revalidated"
	PrereqNodeEffectLifecycle    = "dpu-node-effect-lifecycle-ready"
)

// ManagementIdentity binds a generated plan to the authenticated management
// cluster observed by discovery. It contains no kubeconfig or credential data.
type ManagementIdentity struct {
	APIEndpoint        string `yaml:"apiEndpoint" json:"apiEndpoint"`
	SystemNamespaceUID string `yaml:"systemNamespaceUID" json:"systemNamespaceUID"`
	TrustDigest        string `yaml:"trustDigest" json:"trustDigest"`
}

// Inputs are resolved desired state plus exact chart bytes. SourceConfig is
// retained verbatim as provenance; chart archives are retained separately and
// never copied into resolved configuration.
type Inputs struct {
	Operator        bootstrap.OperatorRenderInputs
	OperatorArchive []byte
	Workload        hbnrender.Inputs
	HBNChartArchive []byte
	SourceConfig    []byte
	Management      ManagementIdentity
}

// Generate validates all input relationships, renders each fixed stage, checks
// the generated custom resources against schemas rendered from the exact DPF
// operator chart, and returns one closed immutable bundle.
func Generate(input Inputs) (*bundle.Bundle, error) {
	release, err := releases.Resolve(input.Operator.ReleaseVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve selected DPF release: %w", err)
	}
	definition, err := hbn.LoadDefinition()
	if err != nil {
		return nil, err
	}
	if release.Version != definition.Defaults.Release {
		return nil, fmt.Errorf("selected DPF release %s does not match HBN profile release %s", release.Version, definition.Defaults.Release)
	}
	if err := validateInputs(input); err != nil {
		return nil, err
	}
	if err := validateHBNChartArchive(input.HBNChartArchive, input.Workload.Generation); err != nil {
		return nil, fmt.Errorf("validate HBN chart archive: %w", err)
	}

	operator, err := bootstrap.RenderOperator(input.OperatorArchive, input.Operator)
	if err != nil {
		return nil, fmt.Errorf("render selected DPF operator chart: %w", err)
	}
	bootstrapFiles, err := bootstrap.Render(input.Operator.Bootstrap)
	if err != nil {
		return nil, fmt.Errorf("render DPF bootstrap resources: %w", err)
	}
	workloadFiles, err := hbnrender.Render(input.Workload)
	if err != nil {
		return nil, fmt.Errorf("render HBN workload resources: %w", err)
	}
	if err := validateRenderedHBNChart(input.HBNChartArchive, input.Workload, workloadFiles); err != nil {
		return nil, fmt.Errorf("render retained HBN chart: %w", err)
	}
	if err := validateGeneratedSchemas(operator.Manifest, bootstrapFiles, workloadFiles); err != nil {
		return nil, err
	}

	files, records, stages, err := buildFiles(input, release.Version, operator, bootstrapFiles, workloadFiles)
	if err != nil {
		return nil, err
	}
	serials := make([]string, 0, len(input.Workload.Devices))
	for _, device := range input.Workload.Devices {
		serials = append(serials, device.Serial)
	}
	sort.Strings(serials)
	index := bundle.Index{
		FormatVersion:   bundle.FormatVersion,
		Target:          bundle.Target,
		Phase:           bundle.Phase,
		Profile:         "hbn-bgp",
		Release:         release.Version,
		Renderer:        "native-dpf-generation/v1",
		SelectedDevices: serials,
		Files:           records,
		Stages:          stages,
		References:      hbnReferences(input, workloadFiles),
		ExternalPrerequisites: []string{
			PrereqManagementDependencies, PrereqManagementRBAC, PrereqManagementIdentity,
			PrereqTenantEndpoint, PrereqTenantStorage, PrereqBMCCredentials, PrereqBMCIdentity,
			PrereqTenantAPI, PrereqDPUIdentity, PrereqProvisioningArtifacts, PrereqHBNChartSource,
			PrereqFabricAndHost, PrereqNodeEffectLifecycle,
		},
	}
	result, err := bundle.New(index, files)
	if err != nil {
		return nil, fmt.Errorf("validate composed DPF bundle: %w", err)
	}
	return result, nil
}

// validateRenderedHBNChart renders the exact retained archive with the values
// generated into DPUServiceTemplate. This catches invalid Helm values and
// confirms the pinned chart applies the flat resource map to the HBN container.
func validateRenderedHBNChart(archive []byte, input hbnrender.Inputs, workloadFiles []bundle.File) error {
	chart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("load verified chart: %w", err)
	}
	if chart.Metadata == nil || chart.Metadata.Name != input.Generation.ChartName || chart.Metadata.Version != input.Generation.ChartVersion {
		return fmt.Errorf("loaded chart metadata does not match resolved chart coordinates")
	}
	var values map[string]any
	for _, file := range workloadFiles {
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(file.Data, &object.Object); err != nil {
			return fmt.Errorf("decode generated workload manifest %s: %w", file.Path, err)
		}
		if object.GetKind() != "DPUServiceTemplate" || object.GetName() != input.Generation.ChartName {
			continue
		}
		candidate, found, err := unstructured.NestedMap(object.Object, "spec", "helmChart", "values")
		if err != nil || !found {
			return fmt.Errorf("generated HBN DPUServiceTemplate has no chart values")
		}
		values = candidate
		break
	}
	if values == nil {
		return fmt.Errorf("generated HBN DPUServiceTemplate is missing")
	}
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(chart, values, chartutil.ReleaseOptions{Name: "hbn-validation", Namespace: input.Namespace, IsInstall: true}, chartutil.DefaultCapabilities.Copy(), false)
	if err != nil {
		return fmt.Errorf("validate generated chart values: %w", err)
	}
	manifests, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return fmt.Errorf("execute chart templates: %w", err)
	}
	var daemonSets int
	for filename, manifest := range manifests {
		objects, err := decodeObjects([]byte(manifest))
		if err != nil {
			return fmt.Errorf("decode rendered chart template %s: %w", filename, err)
		}
		for _, object := range objects {
			if object.GetKind() != "DaemonSet" {
				continue
			}
			daemonSets++
			containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
			if err != nil || !found {
				return fmt.Errorf("rendered HBN DaemonSet has no containers")
			}
			var hbnContainer map[string]any
			for _, raw := range containers {
				container, ok := raw.(map[string]any)
				if ok && container["name"] == "doca-hbn" {
					hbnContainer = container
					break
				}
			}
			if hbnContainer == nil {
				return fmt.Errorf("rendered HBN DaemonSet has no doca-hbn container")
			}
			resources, ok := values["resources"].(map[string]any)
			if !ok || len(resources) == 0 {
				return fmt.Errorf("generated HBN chart values require a nonempty flat resources map")
			}
			requests, found, err := unstructured.NestedMap(hbnContainer, "resources", "requests")
			if err != nil || !found || !equalAnyMap(requests, resources) {
				return fmt.Errorf("rendered HBN requests do not match the generated flat resources map")
			}
			limits, found, err := unstructured.NestedMap(hbnContainer, "resources", "limits")
			if err != nil || !found || !equalAnyMap(limits, resources) {
				return fmt.Errorf("rendered HBN limits do not match the generated flat resources map")
			}
		}
	}
	if daemonSets != 1 {
		return fmt.Errorf("expected one rendered HBN DaemonSet, got %d", daemonSets)
	}
	return nil
}

func equalAnyMap(a, b map[string]any) bool {
	return reflect.DeepEqual(a, b)
}

// Publish delegates to the bundle's atomic, no-replace publication contract.
func Publish(ctx context.Context, generated *bundle.Bundle, destination string) (bundle.Publication, error) {
	return generated.Publish(ctx, destination)
}

func validateInputs(input Inputs) error {
	if len(input.SourceConfig) == 0 {
		return fmt.Errorf("original source configuration bytes are required")
	}
	if input.Management.SystemNamespaceUID == "" || strings.TrimSpace(input.Management.SystemNamespaceUID) != input.Management.SystemNamespaceUID {
		return fmt.Errorf("authenticated management system namespace UID is required")
	}
	if input.Management.TrustDigest == "" || strings.TrimSpace(input.Management.TrustDigest) != input.Management.TrustDigest {
		return fmt.Errorf("authenticated management trust identity is required")
	}
	endpoint, err := url.Parse(input.Management.APIEndpoint)
	if err != nil || !endpoint.IsAbs() || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("authenticated management API endpoint must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if input.Operator.Bootstrap.OperatorNamespace != input.Workload.Namespace {
		return fmt.Errorf("operator and HBN workload namespaces must match")
	}
	if !equalStringMap(input.Operator.Bootstrap.Tenant.Labels, input.Workload.DPUClusterSelector) {
		return fmt.Errorf("HBN DPUCluster selector must exactly match the resolved tenant identity labels")
	}
	bootstrapSerials := make([]string, 0, len(input.Operator.Bootstrap.Devices))
	for _, device := range input.Operator.Bootstrap.Devices {
		bootstrapSerials = append(bootstrapSerials, device.Serial)
	}
	workloadSerials := make([]string, 0, len(input.Workload.Devices))
	for _, device := range input.Workload.Devices {
		workloadSerials = append(workloadSerials, device.Serial)
	}
	sort.Strings(bootstrapSerials)
	sort.Strings(workloadSerials)
	if len(bootstrapSerials) == 0 || !slices.Equal(bootstrapSerials, workloadSerials) {
		return fmt.Errorf("bootstrap and HBN workload must select the exact same nonempty DPU serial set")
	}
	for i := 1; i < len(bootstrapSerials); i++ {
		if bootstrapSerials[i] == bootstrapSerials[i-1] {
			return fmt.Errorf("selected DPU serials must be unique")
		}
	}
	return nil
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || value != other {
			return false
		}
	}
	return true
}

func validateHBNChartArchive(archive []byte, generation hbn.Generation) error {
	if len(archive) == 0 || len(archive) > maxHBNChartBytes {
		return fmt.Errorf("chart archive must be between 1 byte and %d bytes", maxHBNChartBytes)
	}
	actual := sha256.Sum256(archive)
	if "sha256:"+hex.EncodeToString(actual[:]) != generation.ChartDigest {
		return fmt.Errorf("chart archive SHA-256 does not match resolved generation digest")
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open chart archive gzip: %w", err)
	}
	defer gzipReader.Close()
	counter := &countWriter{}
	bounded := io.TeeReader(io.LimitReader(gzipReader, maxExpandedChartBytes+1), counter)
	reader := tar.NewReader(bounded)
	seen := make(map[string]bool)
	var chartYAML []byte
	root := generation.ChartName + "/"
	for entries := 0; ; entries++ {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read chart archive: %w", err)
		}
		if entries >= maxChartEntries {
			return fmt.Errorf("chart archive exceeds %d entries", maxChartEntries)
		}
		archiveName := header.Name
		if header.Typeflag == tar.TypeDir {
			archiveName = strings.TrimSuffix(archiveName, "/")
		}
		clean := path.Clean(archiveName)
		chartRootDirectory := header.Typeflag == tar.TypeDir && clean == generation.ChartName
		if clean != archiveName || strings.HasPrefix(clean, "/") || clean == "." || (!chartRootDirectory && !strings.HasPrefix(clean, root)) {
			return fmt.Errorf("chart archive contains an unsafe or unexpected path")
		}
		if seen[clean] {
			return fmt.Errorf("chart archive contains a duplicate path")
		}
		seen[clean] = true
		if header.Size < 0 || header.Size > maxExpandedChartBytes {
			return fmt.Errorf("chart archive entry exceeds expanded size limit")
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return fmt.Errorf("chart directory entry has content")
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("chart archive contains an unsupported file type")
		}
		if clean == root+"Chart.yaml" {
			if header.Size > maxChartMetadataBytes {
				return fmt.Errorf("chart metadata exceeds size limit")
			}
			chartYAML, err = io.ReadAll(io.LimitReader(reader, maxChartMetadataBytes+1))
			if err != nil || int64(len(chartYAML)) != header.Size {
				return fmt.Errorf("read chart metadata")
			}
			continue
		}
		if _, err := io.CopyN(io.Discard, reader, header.Size); err != nil {
			return fmt.Errorf("read chart archive entry")
		}
	}
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return fmt.Errorf("finish chart archive: %w", err)
	}
	if counter.n > maxExpandedChartBytes {
		return fmt.Errorf("chart archive exceeds expanded size limit")
	}
	if len(chartYAML) == 0 {
		return fmt.Errorf("chart archive has no root Chart.yaml")
	}
	var metadata struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(chartYAML))
	if err := decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("decode chart metadata: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("chart metadata must contain one YAML document")
	}
	if metadata.Name != generation.ChartName || metadata.Version != generation.ChartVersion {
		return fmt.Errorf("chart metadata does not match resolved chart name and version")
	}
	return nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(data []byte) (int, error) {
	c.n += int64(len(data))
	return len(data), nil
}

func buildFiles(input Inputs, release string, operator bootstrap.OperatorRenderResult, bootstrapFiles, workloadFiles []bundle.File) ([]bundle.File, []bundle.FileRecord, []bundle.Stage, error) {
	var files []bundle.File
	var records []bundle.FileRecord
	add := func(file bundle.File, role bundle.Role) string {
		files = append(files, bundle.File{Path: file.Path, Data: bytes.Clone(file.Data)})
		records = append(records, bundle.FileRecord{Path: file.Path, Role: role})
		return file.Path
	}
	operatorChartPath := "charts/dpf-operator-" + release + ".tgz"
	operatorValuesPath := "helm/dpf-operator-values.yaml"
	operatorProjectionPath := "projections/dpf-operator-install.yaml"
	hbnChartPath := "charts/" + input.Workload.Generation.ChartName + "-" + input.Workload.Generation.ChartVersion + ".tgz"
	add(bundle.File{Path: operatorChartPath, Data: input.OperatorArchive}, bundle.Chart)
	add(bundle.File{Path: operatorValuesPath, Data: operator.HelmValues}, bundle.HelmValues)
	add(bundle.File{Path: operatorProjectionPath, Data: operator.Manifest}, bundle.Projection)
	add(bundle.File{Path: hbnChartPath, Data: input.HBNChartArchive}, bundle.Chart)

	var namespaceFiles, configFiles, tenantFiles, enrollmentFiles []string
	for _, file := range bootstrapFiles {
		kind, err := manifestKind(file.Data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inspect bootstrap manifest %s: %w", file.Path, err)
		}
		filename := "bootstrap/" + path.Base(file.Path)
		file.Path = filename
		add(file, bundle.Manifest)
		switch kind {
		case "Namespace":
			namespaceFiles = append(namespaceFiles, filename)
		case "DPFOperatorConfig":
			configFiles = append(configFiles, filename)
		case "DPUCluster":
			tenantFiles = append(tenantFiles, filename)
		case "DPUDiscovery":
			enrollmentFiles = append(enrollmentFiles, filename)
		default:
			return nil, nil, nil, fmt.Errorf("unsupported DPF bootstrap resource kind %q", kind)
		}
	}
	var workloadPrerequisiteFiles, deploymentFiles []string
	for _, file := range workloadFiles {
		kind, err := manifestKind(file.Data)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("inspect HBN manifest %s: %w", file.Path, err)
		}
		filename := "workload/" + path.Base(file.Path)
		file.Path = filename
		add(file, bundle.Manifest)
		if kind == "DPUDeployment" {
			deploymentFiles = append(deploymentFiles, filename)
		} else {
			workloadPrerequisiteFiles = append(workloadPrerequisiteFiles, filename)
		}
	}
	if len(namespaceFiles) == 0 || len(configFiles) != 1 || len(tenantFiles) != 1 || len(enrollmentFiles) == 0 || len(deploymentFiles) != 1 {
		return nil, nil, nil, fmt.Errorf("renderers did not produce the expected fixed resource stages")
	}
	// The HBN chart is retained with its apply-time template and source gate.
	workloadPrerequisiteFiles = append(workloadPrerequisiteFiles, hbnChartPath)
	resolvedPath := bundle.ResolvedConfigPath
	resolved, err := resolvedConfiguration(input, release, operatorValuesPath, operatorChartPath, hbnChartPath, operator.HelmValues)
	if err != nil {
		return nil, nil, nil, err
	}
	add(bundle.File{Path: resolvedPath, Data: resolved}, bundle.ResolvedConfig)
	add(bundle.File{Path: ".l8k/source-config.yaml", Data: input.SourceConfig}, bundle.Metadata)

	stages := []bundle.Stage{
		{ID: StageNamespaces, Destination: bundle.Management, Files: namespaceFiles},
		{ID: StageOperator, Destination: bundle.Management, Namespace: input.Operator.Bootstrap.OperatorNamespace, Files: []string{operatorChartPath, operatorValuesPath}, ProjectionFiles: []string{operatorProjectionPath}, DependsOn: []string{StageNamespaces}, Requires: []string{PrereqManagementDependencies, PrereqManagementRBAC}},
		{ID: StageOperatorConfig, Destination: bundle.Management, Namespace: input.Operator.Bootstrap.OperatorNamespace, Files: configFiles, DependsOn: []string{StageOperator}, Requires: []string{PrereqManagementIdentity}},
		{ID: StageTenant, Destination: bundle.Management, Namespace: input.Operator.Bootstrap.Tenant.Namespace, Files: tenantFiles, DependsOn: []string{StageOperatorConfig}, Requires: []string{PrereqTenantEndpoint, PrereqTenantStorage}},
		{ID: StageEnrollment, Destination: bundle.Management, Namespace: input.Operator.Bootstrap.OperatorNamespace, Files: enrollmentFiles, DependsOn: []string{StageTenant}, Requires: []string{PrereqBMCCredentials, PrereqBMCIdentity}},
		{ID: StageWorkload, Destination: bundle.Management, Namespace: input.Workload.Namespace, Files: workloadPrerequisiteFiles, DependsOn: []string{StageEnrollment}, Requires: []string{PrereqTenantAPI, PrereqDPUIdentity, PrereqProvisioningArtifacts, PrereqHBNChartSource}},
		{ID: StageDeployment, Destination: bundle.Management, Namespace: input.Workload.Namespace, Files: deploymentFiles, DependsOn: []string{StageWorkload}, Requires: []string{PrereqFabricAndHost, PrereqNodeEffectLifecycle, PrereqDPUIdentity}},
	}
	return files, records, stages, nil
}

func resolvedConfiguration(input Inputs, release, valuesPath, operatorChartPath, hbnChartPath string, helmValues []byte) ([]byte, error) {
	operatorRelease, err := releases.Resolve(release)
	if err != nil {
		return nil, err
	}
	selectedPSIDs := make([]string, 0, len(input.Workload.Devices))
	for _, device := range input.Workload.Devices {
		selectedPSIDs = append(selectedPSIDs, device.PSID)
	}
	software, issues, err := config.ValidateProvisioningSoftware(&config.Provisioning{Software: input.Workload.Software}, selectedPSIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve selected provisioning software: %w", err)
	}
	if len(issues) != 0 {
		return nil, fmt.Errorf("selected provisioning software is incomplete: %s", issues[0].Field)
	}
	selected := make([]resolvedDevice, 0, len(input.Workload.Devices))
	for _, device := range input.Workload.Devices {
		selected = append(selected, resolvedDevice{Serial: device.Serial, PSID: device.PSID, ASN: device.ASN, SelectorLabels: cloneLabels(device.SelectorLabels)})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Serial < selected[j].Serial })
	intent := resolvedIntent{
		UseCase: "hbn-bgp", Release: release, Namespace: input.Workload.Namespace,
		Management:        input.Management,
		KubernetesVersion: input.Operator.KubernetesVersion,
		Networking:        input.Workload.Profile,
		Generation:        input.Workload.Generation,
		Software:          software,
		Bootstrap: resolvedBootstrap{
			OperatorConfigName:    input.Operator.Bootstrap.OperatorConfigName,
			BMCFactoryResetPolicy: input.Operator.Bootstrap.BMCFactoryResetPolicy,
			ManagementAPIIP:       input.Operator.Bootstrap.ManagementAPIIP, ManagementAPIPort: input.Operator.Bootstrap.ManagementAPIPort,
			ArtifactPVC: input.Operator.Bootstrap.ArtifactPVC, PodCIDR: input.Operator.Bootstrap.PodCIDR,
			Tenant: input.Operator.Bootstrap.Tenant,
		},
		OperatorChart:  resolvedArtifact{Path: operatorChartPath, SHA256: operatorRelease.Operator.ChartDigest},
		OperatorValues: resolvedArtifact{Path: valuesPath, SHA256: digest(helmValues)},
		HBNChart:       resolvedArtifact{Path: hbnChartPath, SHA256: input.Workload.Generation.ChartDigest},
		Devices:        selected,
	}
	data, err := yaml.Marshal(struct {
		FormatVersion string         `yaml:"formatVersion"`
		Target        string         `yaml:"target"`
		Config        resolvedIntent `yaml:"config"`
	}{FormatVersion: bundle.FormatVersion, Target: bundle.Target, Config: intent})
	if err != nil {
		return nil, fmt.Errorf("encode resolved DPF intent: %w", err)
	}
	return data, nil
}

type resolvedIntent struct {
	UseCase           string             `yaml:"useCase"`
	Release           string             `yaml:"release"`
	Namespace         string             `yaml:"namespace"`
	Management        ManagementIdentity `yaml:"management"`
	KubernetesVersion string             `yaml:"kubernetesVersion"`
	Networking        hbn.Profile        `yaml:"networking"`
	Generation        hbn.Generation     `yaml:"generation"`
	Software          config.Software    `yaml:"software"`
	Bootstrap         resolvedBootstrap  `yaml:"bootstrap"`
	OperatorChart     resolvedArtifact   `yaml:"operatorChart"`
	OperatorValues    resolvedArtifact   `yaml:"operatorValues"`
	HBNChart          resolvedArtifact   `yaml:"hbnChart"`
	Devices           []resolvedDevice   `yaml:"devices"`
}

type resolvedBootstrap struct {
	OperatorConfigName    string                          `yaml:"operatorConfigName"`
	BMCFactoryResetPolicy bootstrap.BMCFactoryResetPolicy `yaml:"bmcFactoryResetPolicy"`
	ManagementAPIIP       string                          `yaml:"managementAPIIP"`
	ManagementAPIPort     int                             `yaml:"managementAPIPort"`
	ArtifactPVC           string                          `yaml:"artifactPVC,omitempty"`
	PodCIDR               string                          `yaml:"podCIDR"`
	Tenant                bootstrap.Tenant                `yaml:"tenant"`
}

type resolvedArtifact struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

type resolvedDevice struct {
	Serial         string            `yaml:"serial"`
	PSID           string            `yaml:"psid"`
	ASN            uint32            `yaml:"asn"`
	SelectorLabels map[string]string `yaml:"selectorLabels"`
}

func cloneLabels(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func manifestKind(data []byte) (string, error) {
	var object struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &object); err != nil {
		return "", err
	}
	if object.Kind == "" {
		return "", fmt.Errorf("kind is required")
	}
	return object.Kind, nil
}

type resourceKey struct{ group, kind string }

type dpfSchema struct {
	versions map[string]versionSchema
}

type versionSchema struct {
	validator  validation.SchemaCreateValidator
	structural *structuralschema.Structural
}

func validateGeneratedSchemas(operatorManifest []byte, bootstrapFiles, workloadFiles []bundle.File) error {
	schemas, err := schemasFromOperatorManifest(operatorManifest)
	if err != nil {
		return err
	}
	for _, file := range append(slices.Clone(bootstrapFiles), workloadFiles...) {
		if err := validateCustomResourceFile(file, schemas); err != nil {
			return err
		}
	}
	return nil
}

func schemasFromOperatorManifest(manifest []byte) (map[resourceKey]dpfSchema, error) {
	objects, err := decodeObjects(manifest)
	if err != nil {
		return nil, fmt.Errorf("decode selected DPF operator projection: %w", err)
	}
	result := make(map[resourceKey]dpfSchema)
	for _, object := range objects {
		if object.GetAPIVersion() != "apiextensions.k8s.io/v1" || object.GetKind() != "CustomResourceDefinition" {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		crdData, err := json.Marshal(object.Object)
		if err != nil {
			return nil, fmt.Errorf("encode selected operator CRD %s: %w", object.GetName(), err)
		}
		if err := json.Unmarshal(crdData, &crd); err != nil {
			return nil, fmt.Errorf("decode selected operator CRD %s: %w", object.GetName(), err)
		}
		key := resourceKey{group: crd.Spec.Group, kind: crd.Spec.Names.Kind}
		if key.group == "" || key.kind == "" {
			return nil, fmt.Errorf("selected operator chart contains an incomplete CRD identity")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("selected operator chart contains duplicate CRD identity %s/%s", key.group, key.kind)
		}
		entry := dpfSchema{versions: make(map[string]versionSchema)}
		for _, version := range crd.Spec.Versions {
			if !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			internal := &apiextensions.JSONSchemaProps{}
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internal, nil); err != nil {
				return nil, fmt.Errorf("convert selected operator schema for %s/%s %s: %w", key.group, key.kind, version.Name, err)
			}
			validator, _, err := validation.NewSchemaValidator(internal)
			if err != nil {
				return nil, fmt.Errorf("build selected operator schema validator for %s/%s %s: %w", key.group, key.kind, version.Name, err)
			}
			structural, err := structuralschema.NewStructural(internal)
			if err != nil {
				return nil, fmt.Errorf("build selected operator structural schema for %s/%s %s: %w", key.group, key.kind, version.Name, err)
			}
			entry.versions[version.Name] = versionSchema{validator: validator, structural: structural}
		}
		if len(entry.versions) == 0 {
			return nil, fmt.Errorf("selected operator CRD %s/%s has no served OpenAPI schema", key.group, key.kind)
		}
		result[key] = entry
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("selected operator chart projection contains no DPF CRD OpenAPI schemas")
	}
	return result, nil
}

func validateCustomResourceFile(file bundle.File, schemas map[resourceKey]dpfSchema) error {
	objects, err := decodeObjects(file.Data)
	if err != nil {
		return fmt.Errorf("decode generated manifest %s: %w", file.Path, err)
	}
	for _, object := range objects {
		group, version, err := splitAPIVersion(object.GetAPIVersion())
		if err != nil {
			return fmt.Errorf("%s %s: %w", object.GetKind(), object.GetName(), err)
		}
		if group == "" {
			continue
		}
		key := resourceKey{group: group, kind: object.GetKind()}
		crd, ok := schemas[key]
		if !ok {
			return fmt.Errorf("generated custom resource %s %s/%s is not served by the selected DPF operator chart", object.GetKind(), group, version)
		}
		schema, ok := crd.versions[version]
		if !ok {
			return fmt.Errorf("generated custom resource %s %s/%s has no selected chart schema", object.GetKind(), group, version)
		}
		if errors := validation.ValidateCustomResource(field.NewPath("resource"), object.Object, schema.validator); len(errors) > 0 {
			return fmt.Errorf("generated custom resource %s %q fails selected DPF OpenAPI schema: %s", object.GetKind(), object.GetName(), errors[0])
		}
		pruned := object.DeepCopy().Object
		unknown := pruning.PruneWithOptions(pruned, schema.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
		if len(unknown) > 0 {
			return fmt.Errorf("generated custom resource %s %q contains fields that the selected DPF schema would prune: %s", object.GetKind(), object.GetName(), unknown[0])
		}
	}
	return nil
}

func decodeObjects(data []byte) ([]*unstructured.Unstructured, error) {
	reader := kubeutilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var objects []*unstructured.Unstructured
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if err == io.EOF {
			return objects, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read document %d: %w", ordinal, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(raw, &object.Object); err != nil {
			return nil, fmt.Errorf("decode document %d: %w", ordinal, err)
		}
		if object.GetKind() == "" || object.GetAPIVersion() == "" || object.GetName() == "" {
			return nil, fmt.Errorf("document %d is missing apiVersion, kind, or metadata.name", ordinal)
		}
		objects = append(objects, object)
	}
}

func splitAPIVersion(apiVersion string) (group, version string, err error) {
	parts := strings.Split(apiVersion, "/")
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid apiVersion %q", apiVersion)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("invalid apiVersion %q", apiVersion)
	}
}

func hbnReferences(input Inputs, workloadFiles []bundle.File) []bundle.Reference {
	workloadNamespace := input.Workload.Namespace
	ref := func(group, kind, namespace, name string) bundle.ResourceRef {
		return bundle.ResourceRef{Destination: bundle.Management, Group: group, Kind: kind, Namespace: namespace, Name: name}
	}
	deployment := ref("svc.dpu.nvidia.com", "DPUDeployment", workloadNamespace, "hbn-bgp")
	cluster := ref("provisioning.dpu.nvidia.com", "DPUCluster", input.Operator.Bootstrap.Tenant.Namespace, input.Operator.Bootstrap.Tenant.Name)
	refs := []bundle.Reference{{From: deployment, To: cluster}}
	for _, name := range []string{"p0", "p1", "pf0hpf"} {
		refs = append(refs, bundle.Reference{From: deployment, To: ref("svc.dpu.nvidia.com", "DPUServiceInterface", workloadNamespace, name)})
	}
	for _, file := range workloadFiles {
		var identity struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if yaml.Unmarshal(file.Data, &identity) != nil {
			continue
		}
		switch identity.Kind {
		case "BlueFieldSoftware":
			refs = append(refs, bundle.Reference{From: deployment, To: ref("provisioning.dpu.nvidia.com", identity.Kind, workloadNamespace, identity.Metadata.Name)})
		case "DPUFlavor":
			refs = append(refs, bundle.Reference{From: deployment, To: ref("provisioning.dpu.nvidia.com", identity.Kind, workloadNamespace, identity.Metadata.Name)})
		case "DPUServiceTemplate", "DPUServiceConfiguration":
			refs = append(refs, bundle.Reference{From: deployment, To: ref("svc.dpu.nvidia.com", identity.Kind, workloadNamespace, identity.Metadata.Name)})
		case "DPUServiceIPAM":
			refs = append(refs, bundle.Reference{From: ref("svc.dpu.nvidia.com", identity.Kind, workloadNamespace, identity.Metadata.Name), To: cluster})
		}
	}
	return refs
}
