// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const maximumSystemChartBytes = 1 << 20

type systemArtifactRole struct {
	Component  string
	Management bool
}

type systemArtifactContainer struct {
	Kind string
	Name string
}

type systemArtifactExpectation struct {
	WorkloadKind string
	Images       map[systemArtifactContainer]string
	// Workload is rendered with a synthetic release name and namespace.
	// Its identity fields are unsuitable for binding live objects.
	Workload    *unstructured.Unstructured
	Attachments map[string]*unstructured.Unstructured
	// Values is the independently derived subset of the system service's
	// chart values that affects this projection.
	Values     map[string]any
	Defaults   map[string]any
	ConfigMaps map[string]*unstructured.Unstructured
	Services   map[string]*unstructured.Unstructured
	archive    []byte
	source     releases.SystemNetworking
	version    chartutil.KubeVersion
}

type systemArtifacts struct {
	Roles      map[systemArtifactRole]systemArtifactExpectation
	TenantCRDs map[string]*unstructured.Unstructured
}

// Each component has a tenant role. The per-cluster components also have a
// management role that runs its controller for the selected tenant.
var systemArtifactComponents = []struct {
	name       string
	tenantKind string
	perCluster bool
}{
	{name: "cni-installer", tenantKind: "DaemonSet"},
	{name: "coredns", perCluster: true},
	{name: "dpu-monitoring"},
	{name: "flannel", tenantKind: "DaemonSet"},
	{name: "kube-state-metrics", perCluster: true},
	{name: "multus", tenantKind: "DaemonSet"},
	{name: "node-problem-detector", tenantKind: "DaemonSet"},
	{name: "nvidia-k8s-ipam", tenantKind: "DaemonSet", perCluster: true},
	{name: "servicechainset-controller", perCluster: true},
	{name: "sfc-controller", tenantKind: "DaemonSet"},
	{name: "sriov-device-plugin", tenantKind: "DaemonSet"},
}

func systemArtifactRoles() []systemArtifactRole {
	var roles []systemArtifactRole
	for _, component := range systemArtifactComponents {
		roles = append(roles, systemArtifactRole{Component: component.name})
		if component.perCluster {
			roles = append(roles, systemArtifactRole{Component: component.name, Management: true})
		}
	}
	return roles
}

// projectSystemArtifacts derives the v26.8 flat HBN system expectations from a
// verified release archive and the retained intent. The caller owns the returned
// projection and can reuse it for all observations in the invocation.
func projectSystemArtifacts(archive []byte, intent generation.ResolvedIntent, config *unstructured.Unstructured) (systemArtifacts, error) {
	release, err := releases.Resolve(intent.Release)
	if err != nil || intent.Release == "" || intent.UseCase != releases.UseCaseHBNBGP || release.SystemNetworking == nil {
		return systemArtifacts{}, fmt.Errorf("system networking projection is unavailable for the retained release or profile")
	}
	if err := verifySystemArchive(archive, *release.SystemNetworking); err != nil {
		return systemArtifacts{}, err
	}
	if _, err := netip.ParsePrefix(intent.Bootstrap.PodCIDR); err != nil ||
		intent.Bootstrap.Tenant.VIP == "" || intent.Bootstrap.TenantKubernetesVersion != release.TenantKubernetesVersion ||
		intent.Networking.UplinkMTU < 1280 || intent.Networking.UplinkMTU > 9216 {
		return systemArtifacts{}, fmt.Errorf("retained system networking inputs are incomplete or inconsistent")
	}
	if config == nil {
		return systemArtifacts{}, fmt.Errorf("retained defaulted operator configuration is unavailable")
	}
	controlPlaneMTU, found, mtuErr := unstructured.NestedInt64(config.UnstructuredContent(), "spec", "networking", "controlPlaneMTU")
	if mtuErr != nil || !found || controlPlaneMTU < 1280 || controlPlaneMTU > 9216 {
		return systemArtifacts{}, fmt.Errorf("retained defaulted control-plane MTU is unavailable or invalid")
	}
	managementVersion, err := chartutil.ParseKubeVersion(intent.KubernetesVersion)
	if err != nil {
		return systemArtifacts{}, fmt.Errorf("retained management Kubernetes version is invalid")
	}
	tenantVersion, err := chartutil.ParseKubeVersion(intent.Bootstrap.TenantKubernetesVersion)
	if err != nil {
		return systemArtifacts{}, fmt.Errorf("retained tenant Kubernetes version is invalid")
	}
	projected := systemArtifacts{
		Roles:      make(map[systemArtifactRole]systemArtifactExpectation),
		TenantCRDs: make(map[string]*unstructured.Unstructured),
	}
	for _, component := range systemArtifactComponents {
		role := systemArtifactRole{Component: component.name}
		if err := projectSystemArtifactRole(projected, archive, intent, release, role, component.tenantKind, component.perCluster, *tenantVersion, controlPlaneMTU); err != nil {
			return systemArtifacts{}, err
		}
		if component.perCluster {
			role.Management = true
			if err := projectSystemArtifactRole(projected, archive, intent, release, role, "Deployment", true, *managementVersion, controlPlaneMTU); err != nil {
				return systemArtifacts{}, err
			}
		}
	}
	if len(projected.TenantCRDs) != 8 {
		return systemArtifacts{}, fmt.Errorf("system networking projection has incomplete tenant API coverage")
	}
	return projected, nil
}

func verifySystemArchive(archive []byte, source releases.SystemNetworking) error {
	if len(archive) == 0 || len(archive) > maximumSystemChartBytes {
		return fmt.Errorf("system networking chart archive has an invalid size")
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(archive)) != source.ChartDigest {
		return fmt.Errorf("system networking chart digest differs from the release lock")
	}
	return nil
}

func projectSystemArtifactRole(projected systemArtifacts, archive []byte, intent generation.ResolvedIntent, release releases.Release,
	role systemArtifactRole, workloadKind string, perCluster bool, kubeVersion chartutil.KubeVersion, controlPlaneMTU int64,
) error {
	values := systemArtifactValues(intent, release, role, perCluster)
	if role.Component == "flannel" {
		_ = unstructured.SetNestedField(values, strconv.FormatInt(controlPlaneMTU, 10), "flannel", "flannel", "mtu")
	}
	expectation := systemArtifactExpectation{WorkloadKind: workloadKind, Values: values, Attachments: map[string]*unstructured.Unstructured{}, ConfigMaps: map[string]*unstructured.Unstructured{}, Services: map[string]*unstructured.Unstructured{}, archive: bytes.Clone(archive), source: *release.SystemNetworking, version: kubeVersion}
	expectedDefaults, err := systemRoleDefaults(archive, role)
	if err != nil {
		return err
	}
	expectation.Defaults = expectedDefaults
	objects, err := renderSystemArtifactRole(archive, *release.SystemNetworking, role, values, kubeVersion)
	if err != nil {
		return err
	}
	for _, object := range objects {
		switch object.GetKind() {
		case "ConfigMap":
			expectation.ConfigMaps[object.GetName()] = object
		case "Service":
			// Only the selected functional DNS Service is an obligation;
			// incidental metrics Services are outside this finite contract.
			if role.Component == "coredns" && role.Management {
				expectation.Services[object.GetName()] = object
			}
		case "CustomResourceDefinition":
			if role.Management {
				return fmt.Errorf("management system networking role rendered a tenant API definition")
			}
			if err := addSystemArtifactCRD(projected.TenantCRDs, object); err != nil {
				return err
			}
		case "NetworkAttachmentDefinition":
			if object.GetAPIVersion() != "k8s.cni.cncf.io/v1" || role.Management || expectation.Attachments[object.GetName()] != nil {
				return fmt.Errorf("system role rendered an unsupported or duplicate network attachment")
			}
			expectation.Attachments[object.GetName()] = object
		case "DaemonSet", "Deployment", "StatefulSet", "Job", "CronJob", "Pod", "ReplicaSet", "ReplicationController":
			if object.GetAPIVersion() != "apps/v1" || object.GetKind() != workloadKind || expectation.Workload != nil {
				return fmt.Errorf("system networking role %s rendered an unexpected workload", role.Component)
			}
			expectation.Workload = object
			expectation.Images, err = systemArtifactImages(object)
			if err != nil {
				return err
			}
		}
	}
	if workloadKind != "" && expectation.Workload == nil {
		return fmt.Errorf("system networking role %s did not render its expected workload", role.Component)
	}
	names := systemArtifactAttachmentNames(role)
	if len(expectation.Attachments) != len(names) {
		return fmt.Errorf("system role rendered incomplete network attachment coverage")
	}
	for _, name := range names {
		if expectation.Attachments[name] == nil {
			return fmt.Errorf("system role omitted its selected network attachment")
		}
	}
	projected.Roles[role] = expectation
	return nil
}

func systemArtifactValues(intent generation.ResolvedIntent, release releases.Release, role systemArtifactRole, perCluster bool) map[string]any {
	component := map[string]any{"enabled": true}
	if perCluster {
		if role.Management {
			component["deployHostManifests"] = true
		} else {
			component["deployDPUManifests"] = true
		}
	}
	switch role.Component {
	case "dpu-monitoring":
		component["deployDPUManifests"] = true
	case "cni-installer":
		component["cniInstaller"] = map[string]any{"image": systemArtifactImageValues(release.SystemNetworking.CNIInstallerImage)}
	case "sfc-controller", "servicechainset-controller":
		component["controllerManager"] = map[string]any{"manager": map[string]any{"image": systemArtifactImageValues(release.Operator.Image)}}
	case "flannel":
		component["podCidr"] = intent.Bootstrap.PodCIDR
		component["flannel"] = map[string]any{"skipCNIConfigInstallation": true}
	case "multus":
		component["mtu"] = strconv.Itoa(intent.Networking.UplinkMTU)
	case "coredns":
		if role.Management && len(intent.Bootstrap.Tenant.NodeSelector) != 0 {
			selector := make(map[string]any, len(intent.Bootstrap.Tenant.NodeSelector))
			for key, value := range intent.Bootstrap.Tenant.NodeSelector {
				selector[key] = value
			}
			component["nodeSelector"] = selector
		}
	}
	values := map[string]any{role.Component: component}
	systemControllerValues(intent, role, values)
	return values
}

func systemArtifactImageValues(image string) map[string]any {
	repository, tag, _ := strings.Cut(image, ":")
	return map[string]any{"repository": repository, "tag": tag}
}

func renderSystemArtifactRole(archive []byte, source releases.SystemNetworking, role systemArtifactRole, values map[string]any,
	version chartutil.KubeVersion,
) ([]*unstructured.Unstructured, error) {
	return renderSystemArtifactRoleAt(archive, source, role, values, version, "system-artifact-validation", "system-artifact-validation")
}

func renderSystemArtifactRoleAt(archive []byte, source releases.SystemNetworking, role systemArtifactRole, values map[string]any, version chartutil.KubeVersion, name, namespace string) ([]*unstructured.Unstructured, error) {
	// ProcessDependencies prunes the chart tree. Load a fresh tree for each
	// role, from the same archive verified by projectSystemArtifacts.
	chart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load verified system networking chart")
	}
	metadata := chart.Metadata
	if metadata == nil || metadata.Name != source.Chart || metadata.Version != source.ChartVersion ||
		metadata.AppVersion != source.ChartVersion || metadata.Annotations["dpfVersion"] != source.ChartVersion {
		return nil, fmt.Errorf("system networking chart metadata differs from the release lock")
	}
	if err := chartutil.ProcessDependencies(chart, chartutil.Values(values)); err != nil {
		return nil, fmt.Errorf("select system networking chart dependency")
	}
	dependencies := chart.Dependencies()
	if len(dependencies) != 1 || dependencies[0].Name() != role.Component {
		return nil, fmt.Errorf("system networking chart did not select exactly one expected component")
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion = version
	options := chartutil.ReleaseOptions{Name: name, Namespace: namespace, IsInstall: true}
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(chart, values, options, capabilities, false)
	if err != nil {
		return nil, fmt.Errorf("validate retained system networking chart values")
	}
	rendered, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return nil, fmt.Errorf("render verified system networking chart")
	}
	var objects []*unstructured.Unstructured
	for _, name := range slices.Sorted(maps.Keys(rendered)) {
		documents, err := decodeOperatorManifest([]byte(rendered[name]))
		if err != nil {
			return nil, fmt.Errorf("decode rendered system networking manifest")
		}
		objects = append(objects, documents...)
	}
	// Helm renders CRDs in templates above and separately ships files in crds/.
	for _, crd := range chart.CRDObjects() {
		documents, err := decodeOperatorManifest(crd.File.Data)
		if err != nil {
			return nil, fmt.Errorf("decode packaged system networking API definition")
		}
		objects = append(objects, documents...)
	}
	return objects, nil
}

func addSystemArtifactCRD(expected map[string]*unstructured.Unstructured, object *unstructured.Unstructured) error {
	if object.GetAPIVersion() != "apiextensions.k8s.io/v1" {
		return fmt.Errorf("system networking chart rendered an unsupported API definition")
	}
	definition, err := operatorCRDFromObject(object)
	if err != nil || !definition.namespaced {
		return fmt.Errorf("system networking chart rendered an invalid tenant API definition")
	}
	if existing := expected[definition.name]; existing != nil {
		if !crdSpecMatches(definition.normalizedSpecJSON, existing) {
			return fmt.Errorf("system networking chart rendered conflicting tenant API definitions")
		}
		return nil
	}
	expected[definition.name] = object
	return nil
}

func systemArtifactImages(workload *unstructured.Unstructured) (map[systemArtifactContainer]string, error) {
	if workload == nil {
		return nil, fmt.Errorf("system networking workload is unavailable")
	}
	images := make(map[systemArtifactContainer]string)
	for _, kind := range []string{"containers", "initContainers"} {
		containers, _, err := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", kind)
		if err != nil || (kind == "containers" && len(containers) == 0) {
			return nil, fmt.Errorf("system networking workload container list is unavailable or malformed")
		}
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			name, _ := container["name"].(string)
			image, _ := container["image"].(string)
			key := systemArtifactContainer{Kind: kind, Name: name}
			if name == "" || image == "" || images[key] != "" {
				return nil, fmt.Errorf("system networking workload container name or image is invalid")
			}
			images[key] = image
		}
	}
	return images, nil
}

// Attachment identities are stable chart contracts for this selected profile;
// current App inventory can locate them but cannot reduce expected coverage.
func systemArtifactAttachmentNames(role systemArtifactRole) []string {
	if role.Management {
		return nil
	}
	switch role.Component {
	case "multus":
		return []string{"mybrhbn"}
	case "sfc-controller":
		return []string{"iprequest"}
	default:
		return nil
	}
}

func systemRoleDefaults(archive []byte, role systemArtifactRole) (map[string]any, error) {
	chart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defaults, err := chartutil.CoalesceValues(chart, nil)
	if err != nil {
		return nil, err
	}
	component, ok := defaults[role.Component].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("system component defaults unavailable")
	}
	return map[string]any{role.Component: component}, nil
}
