// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The helm checks require a real *rest.Config + helm action.Configuration
// pointing at a kube API to fetch releases. We can't easily fake that
// without a kube cluster, so these tests cover the early-skip paths
// (missing inputs) — the install/upgrade-and-diff integration is exercised
// in pkg/networkoperatorplugin/helm_test.go using helm's in-memory
// storage driver.

func TestCheckHelmChartVersion_SkipsWithoutExpected(t *testing.T) {
	r := CheckHelmChartVersion(context.Background(), Inputs{})
	require.True(t, r.Skipped)
	assert.Equal(t, CodeHelmChartVersion, r.Code)
	assert.Contains(t, r.Reason, "no expected chart version")
}

func TestCheckHelmChartVersion_SkipsWhenHelmManagementDisabled(t *testing.T) {
	r := CheckHelmChartVersion(context.Background(), Inputs{
		SkipHelmChecks:       true,
		ExpectedChartVersion: "26.7.0",
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "disabled by configuration")
}

func TestCheckHelmChartVersion_SkipsWithoutRestConfig(t *testing.T) {
	r := CheckHelmChartVersion(context.Background(), Inputs{
		ExpectedChartVersion: "26.4.0-beta.9",
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no kube REST config")
}

func TestCheckHelmValues_SkipsWithoutValuesYAML(t *testing.T) {
	r := CheckHelmValues(context.Background(), Inputs{})
	require.True(t, r.Skipped)
	assert.Equal(t, CodeHelmValues, r.Code)
	assert.Contains(t, r.Reason, "no values.yaml")
}

func TestCheckHelmValues_SkipsWhenHelmManagementDisabled(t *testing.T) {
	r := CheckHelmValues(context.Background(), Inputs{
		SkipHelmChecks:      true,
		GeneratedValuesYAML: []byte("nfd:\n  enabled: true\n"),
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "disabled by configuration")
}

func TestCheckHelmValues_SkipsWithoutRestConfig(t *testing.T) {
	r := CheckHelmValues(context.Background(), Inputs{
		GeneratedValuesYAML: []byte("nfd:\n  enabled: true\n"),
	})
	require.True(t, r.Skipped)
	assert.Contains(t, r.Reason, "no kube REST config")
}
