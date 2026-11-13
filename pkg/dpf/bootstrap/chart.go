// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/postrender"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
	kubeutilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	maxOperatorChartBytes     = 32 << 20
	operatorProjectionRelease = "26.8.0"
)

// OperatorRenderInputs are explicit inputs for offline rendering. The
// Kubernetes version is checked against the selected release; this function
// does not connect to a cluster or install the chart.
type OperatorRenderInputs struct {
	ReleaseVersion    string
	ReleaseName       string
	KubernetesVersion string
	Values            map[string]any
	Bootstrap         Inputs
}

// OperatorRenderResult contains the install projection, canonical explicit
// Helm values for the later install action, and the matching postrenderer.
type OperatorRenderResult struct {
	Manifest     []byte
	HelmValues   []byte
	PostRenderer postrender.PostRenderer
}

// RenderOperator verifies and renders the selected DPF operator chart without
// network or Kubernetes access. This projection supports the pinned 26.8.0
// cohort only; broader chart semantics require separate review.
func RenderOperator(archive []byte, in OperatorRenderInputs) (OperatorRenderResult, error) {
	release, err := releases.Resolve(in.ReleaseVersion)
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("resolve DPF release: %w", err)
	}
	if release.Version != operatorProjectionRelease {
		return OperatorRenderResult{}, fmt.Errorf("offline operator projection is supported only for DPF %s", operatorProjectionRelease)
	}
	if err := validateOperatorRenderInputs(in, release); err != nil {
		return OperatorRenderResult{}, err
	}
	if len(archive) == 0 || len(archive) > maxOperatorChartBytes {
		return OperatorRenderResult{}, fmt.Errorf("operator chart archive must be between 1 byte and %d bytes", maxOperatorChartBytes)
	}
	if err := verifyOperatorArchive(archive, release); err != nil {
		return OperatorRenderResult{}, err
	}
	chart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("load selected operator chart archive: %w", err)
	}
	if chart.Metadata == nil || chart.Metadata.Name != release.Operator.Chart || chart.Metadata.Version != release.Operator.Version || chart.Metadata.AppVersion != release.Operator.Version {
		return OperatorRenderResult{}, fmt.Errorf("operator chart metadata does not match DPF release %s", release.Version)
	}
	valuesYAML, values, err := canonicalHelmValues(in.Values)
	if err != nil {
		return OperatorRenderResult{}, err
	}
	// The exact pinned v26.8.0 chart contains no .Capabilities references.
	// Avoid making API discovery a render input that cannot affect this output.
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(chart, values, chartutil.ReleaseOptions{Name: in.ReleaseName, Namespace: in.Bootstrap.OperatorNamespace, IsInstall: true}, chartutil.DefaultCapabilities.Copy(), false)
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("validate operator chart values: %w", err)
	}
	rendered, err := (engine.Engine{}).Render(chart, renderValues)
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("render DPF operator chart: %w", err)
	}
	manifest, err := orderedOperatorManifests(rendered)
	if err != nil {
		return OperatorRenderResult{}, err
	}
	postRenderer, err := NewOperatorPostRenderer(in.Bootstrap)
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("create DPF operator config postrenderer: %w", err)
	}
	postRendered, err := postRenderer.Run(bytes.NewBuffer(manifest))
	if err != nil {
		return OperatorRenderResult{}, fmt.Errorf("bind DPF operator config in rendered chart: %w", err)
	}
	if err := validateRenderedOperatorImage(postRendered.Bytes(), release.Operator.Image); err != nil {
		return OperatorRenderResult{}, err
	}
	return OperatorRenderResult{Manifest: bytes.Clone(postRendered.Bytes()), HelmValues: valuesYAML, PostRenderer: postRenderer}, nil
}

func validateRenderedOperatorImage(manifest []byte, expected string) error {
	matched := false
	err := visitOperatorDocuments(manifest, "rendered DPF operator", func(object *unstructured.Unstructured) error {
		if object.GetKind() != "Deployment" || !isOperatorDeployment(object) {
			return nil
		}
		if matched {
			return fmt.Errorf("rendered chart contains multiple DPF operator Deployments")
		}
		matched = true
		if object.GetName() != operatorDeploymentName {
			return fmt.Errorf("rendered DPF operator Deployment has unexpected name %q", object.GetName())
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			return fmt.Errorf("rendered DPF operator Deployment has no containers")
		}
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("rendered DPF operator Deployment has a malformed container")
			}
			if container["name"] != operatorContainerName {
				continue
			}
			image, ok := container["image"].(string)
			if !ok || image == "" {
				return fmt.Errorf("rendered DPF operator manager image is missing")
			}
			if image != expected {
				return fmt.Errorf("rendered DPF operator manager image conflicts with the selected release catalog")
			}
			return nil
		}
		return fmt.Errorf("rendered DPF operator Deployment has no %q container", operatorContainerName)
	})
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("rendered chart has no DPF operator Deployment")
	}
	return nil
}

func validateOperatorRenderInputs(in OperatorRenderInputs, release releases.Release) error {
	if in.ReleaseName == "" {
		return fmt.Errorf("helm release name is required")
	}
	if err := chartutil.ValidateReleaseName(in.ReleaseName); err != nil {
		return fmt.Errorf("invalid Helm release name: %w", err)
	}
	if in.Bootstrap.OperatorNamespace == "" || in.Bootstrap.OperatorConfigName == "" {
		return fmt.Errorf("bootstrap operator namespace and config name are required")
	}
	if in.KubernetesVersion == "" {
		return fmt.Errorf("management Kubernetes version is required")
	}
	versionText := strings.TrimPrefix(in.KubernetesVersion, "v")
	versionParts := strings.Split(versionText, ".")
	if len(versionParts) != 3 {
		return fmt.Errorf("management Kubernetes version must be a valid major.minor.patch version")
	}
	for _, part := range versionParts {
		if _, err := parseVersionComponent(part); err != nil {
			return fmt.Errorf("management Kubernetes version must be a valid major.minor.patch version")
		}
	}
	actual, err := k8sversion.ParseGeneric(versionText)
	if err != nil {
		return fmt.Errorf("management Kubernetes version must be a valid major.minor.patch version")
	}
	if release.SupportedKubernetes != nil {
		minimum, err := parseMajorMinor(release.SupportedKubernetes.Minimum)
		if err != nil {
			return fmt.Errorf("invalid selected release Kubernetes minimum: %w", err)
		}
		maximum, err := parseMajorMinor(release.SupportedKubernetes.Maximum)
		if err != nil {
			return fmt.Errorf("invalid selected release Kubernetes maximum: %w", err)
		}
		if compareMajorMinor(int64(actual.Major()), int64(actual.Minor()), minimum[0], minimum[1]) < 0 || compareMajorMinor(int64(actual.Major()), int64(actual.Minor()), maximum[0], maximum[1]) > 0 {
			return fmt.Errorf("management Kubernetes version %s is outside DPF %s supported range %s-%s", in.KubernetesVersion, release.Version, release.SupportedKubernetes.Minimum, release.SupportedKubernetes.Maximum)
		}
	}
	return nil
}

func verifyOperatorArchive(archive []byte, release releases.Release) error {
	parts := strings.SplitN(release.Operator.ChartDigest, ":", 2)
	if len(parts) != 2 || parts[0] != "sha256" {
		return fmt.Errorf("selected release operator chart digest is not SHA-256")
	}
	expected, err := hex.DecodeString(parts[1])
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("selected release operator chart digest is malformed")
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], expected) {
		return fmt.Errorf("operator chart archive SHA-256 does not match selected DPF release %s", release.Version)
	}
	return nil
}

func canonicalHelmValues(values map[string]any) ([]byte, map[string]any, error) {
	if values == nil {
		values = map[string]any{}
	}
	data, err := yaml.Marshal(values)
	if err != nil {
		return nil, nil, fmt.Errorf("encode explicit operator Helm values: %w", err)
	}
	copy := map[string]any{}
	if err := sigsyaml.Unmarshal(data, &copy); err != nil {
		return nil, nil, fmt.Errorf("decode explicit operator Helm values: %w", err)
	}
	return data, copy, nil
}

func orderedOperatorManifests(rendered map[string]string) ([]byte, error) {
	paths := make([]string, 0, len(rendered))
	for file := range rendered {
		if strings.EqualFold(path.Base(file), "NOTES.txt") {
			continue
		}
		paths = append(paths, file)
	}
	slices.Sort(paths)
	var manifests bytes.Buffer
	for _, file := range paths {
		content := rendered[file]
		if len(bytes.TrimSpace([]byte(content))) == 0 {
			continue
		}
		err := visitOperatorDocuments([]byte(content), file, func(object *unstructured.Unstructured) error {
			if hook := object.GetAnnotations()["helm.sh/hook"]; hook != "" {
				return fmt.Errorf("operator chart template %q contains unsupported Helm hook %q", file, hook)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if manifests.Len() != 0 {
			manifests.WriteString("---\n")
		}
		manifests.WriteString(strings.TrimSpace(content))
		manifests.WriteByte('\n')
	}
	if manifests.Len() == 0 {
		return nil, fmt.Errorf("operator chart rendered no install manifests")
	}
	return manifests.Bytes(), nil
}

func visitOperatorDocuments(content []byte, file string, visit func(*unstructured.Unstructured) error) error {
	reader := kubeutilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(content)))
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read operator chart template %q document %d: %w", file, ordinal, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		object := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(raw, &object.Object); err != nil {
			return fmt.Errorf("decode operator chart template %q document %d: %w", file, ordinal, err)
		}
		if err := visit(object); err != nil {
			return err
		}
	}
}

func parseMajorMinor(value string) ([2]int64, error) {
	var result [2]int64
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return result, fmt.Errorf("invalid major.minor version %q", value)
	}
	major, err := parseVersionComponent(parts[0])
	if err != nil {
		return result, err
	}
	minor, err := parseVersionComponent(parts[1])
	if err != nil {
		return result, err
	}
	return [2]int64{major, minor}, nil
}

func parseVersionComponent(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty Kubernetes version component")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid Kubernetes version component %q", value)
		}
	}
	result, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Kubernetes version component %q: %w", value, err)
	}
	return result, nil
}

func compareMajorMinor(major, minor, otherMajor, otherMinor int64) int {
	if major < otherMajor || major == otherMajor && minor < otherMinor {
		return -1
	}
	if major > otherMajor || major == otherMajor && minor > otherMinor {
		return 1
	}
	return 0
}
