// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"fmt"

	sharedbundle "github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// operatorDesired is the immutable retained chart comparison projection. It
// contains no install callback, client, preflight permissions or action state.
// RenderOperator's post-rendering is local manifest compilation only; the
// executable installation callback is added exclusively by deployment.
type operatorDesired struct {
	releaseName        string
	namespace          string
	configName         string
	archive            []byte
	chart              *chart.Chart
	chartFingerprint   string
	expectedValuesJSON []byte
	valuesYAML         []byte
	expectedManifest   []byte
	renderedManifest   []byte
}

func prepareOperatorDesired(archiveInput []byte, inputs bootstrap.OperatorRenderInputs) (*operatorDesired, error) {
	archive := bytes.Clone(archiveInput)
	rendered, err := bootstrap.RenderOperator(archive, inputs)
	if err != nil {
		return nil, fmt.Errorf("validate retained DPF operator chart and inputs: %w", err)
	}
	return compileOperatorDesired(archive, inputs, rendered.Manifest, rendered.HelmValues)
}

// compileOperatorDesired prepares comparison and installation inputs from a
// projection already validated by RenderOperator or a digest-bound LoadPlan.
// It owns all retained bytes; executable callbacks remain operation-specific.
func compileOperatorDesired(archiveInput []byte, inputs bootstrap.OperatorRenderInputs, manifestInput, valuesInput []byte) (*operatorDesired, error) {
	archive := bytes.Clone(archiveInput)
	operatorChart, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load retained DPF operator chart: %w", err)
	}
	if operatorChart.Metadata == nil || operatorChart.Metadata.Name == "" || operatorChart.Metadata.Version == "" {
		return nil, fmt.Errorf("retained DPF operator chart has incomplete metadata")
	}
	if len(operatorChart.Dependencies()) != 0 {
		return nil, fmt.Errorf("retained DPF operator chart has subcharts that Helm does not preserve in release metadata")
	}
	if err := validateInstallableChartShape(operatorChart); err != nil {
		return nil, err
	}
	fingerprint, err := chartFingerprint(operatorChart)
	if err != nil {
		return nil, fmt.Errorf("fingerprint retained DPF operator chart: %w", err)
	}
	values, err := sharedbundle.ParseValues(valuesInput)
	if err != nil {
		return nil, fmt.Errorf("decode rendered DPF Helm values: %w", err)
	}
	valuesJSON, err := canonicalHelmValues(values)
	if err != nil {
		return nil, fmt.Errorf("canonicalize rendered DPF Helm values: %w", err)
	}
	manifest, err := canonicalManifest(manifestInput)
	if err != nil {
		return nil, fmt.Errorf("canonicalize rendered DPF operator manifest: %w", err)
	}
	return &operatorDesired{
		releaseName:        inputs.ReleaseName,
		namespace:          inputs.Bootstrap.OperatorNamespace,
		configName:         inputs.Bootstrap.OperatorConfigName,
		archive:            archive,
		chart:              operatorChart,
		chartFingerprint:   fingerprint,
		expectedValuesJSON: valuesJSON,
		valuesYAML:         bytes.Clone(valuesInput),
		expectedManifest:   manifest,
		renderedManifest:   bytes.Clone(manifestInput),
	}, nil
}
