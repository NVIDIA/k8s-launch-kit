// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
)

// newTestActionConfig builds an action.Configuration backed by in-memory
// storage and a no-op KubeClient so install/upgrade actions can run end-to-end
// without touching a real cluster.
func newTestActionConfig(t *testing.T) *action.Configuration {
	t.Helper()
	return &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(_ string, _ ...interface{}) {},
	}
}

// newTestChart returns a minimal valid chart so install/upgrade can run.
// One trivial template is enough to keep the chart "non-empty" — the helm
// fake KubeClient discards rendered output, so chart content doesn't have
// to be meaningful.
func newTestChart(t *testing.T) *chart.Chart {
	t.Helper()
	return &chart.Chart{
		Metadata: &chart.Metadata{
			Name:       networkOperatorChartName,
			Version:    "0.0.0",
			APIVersion: chart.APIVersionV2,
		},
		Templates: []*chart.File{
			{Name: "templates/configmap.yaml", Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data: {}
`)},
		},
	}
}

// seedRelease persists a deployed release with the given user-supplied values
// so existing-release branches in installOrUpgradeWithLoader fire.
func seedRelease(t *testing.T, cfg *action.Configuration, values map[string]interface{}) {
	t.Helper()
	rel := &release.Release{
		Name:      helmclient.DefaultReleaseName,
		Namespace: "nvidia-network-operator",
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart:     newTestChart(t),
		Config:    values,
	}
	require.NoError(t, cfg.Releases.Create(rel))
}

func TestInstallOrUpgrade_FreshInstall(t *testing.T) {
	actionCfg := newTestActionConfig(t)
	generated := map[string]interface{}{"nfd": map[string]interface{}{"enabled": true}}
	loader := func() (*chart.Chart, error) { return newTestChart(t), nil }

	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		generated,
		"0.0.0",
		"nvidia-network-operator",
		false, // overwriteExisting
		30*time.Second,
		false,
	)
	require.NoError(t, err)

	rel, err := actionCfg.Releases.Last(helmclient.DefaultReleaseName)
	require.NoError(t, err)
	assert.Equal(t, release.StatusDeployed, rel.Info.Status)
	assert.Equal(t, generated, rel.Config)
}

func TestInstallOrUpgrade_SameValuesNoOp(t *testing.T) {
	actionCfg := newTestActionConfig(t)
	values := map[string]interface{}{
		"nfd":                  map[string]interface{}{"enabled": true},
		"sriovNetworkOperator": map[string]interface{}{"enabled": true},
	}
	seedRelease(t, actionCfg, values)

	loaderCalled := false
	loader := func() (*chart.Chart, error) {
		loaderCalled = true
		return newTestChart(t), nil
	}

	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		values, // identical to deployed
		"0.0.0",
		"nvidia-network-operator",
		false,
		30*time.Second,
		false,
	)
	require.NoError(t, err)
	assert.False(t, loaderCalled, "no-op path should skip chart load")

	// Only the seed release is present; no new revision was created.
	hist, err := actionCfg.Releases.History(helmclient.DefaultReleaseName)
	require.NoError(t, err)
	assert.Len(t, hist, 1)
}

func TestInstallOrUpgrade_ConflictWithoutOverwrite(t *testing.T) {
	actionCfg := newTestActionConfig(t)
	deployed := map[string]interface{}{
		"sriovNetworkOperator": map[string]interface{}{"enabled": false},
	}
	seedRelease(t, actionCfg, deployed)

	generated := map[string]interface{}{
		"sriovNetworkOperator": map[string]interface{}{"enabled": true},
	}

	loader := func() (*chart.Chart, error) {
		t.Fatal("chart loader must not be called when conflict short-circuits")
		return nil, nil
	}

	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		generated,
		"0.0.0",
		"nvidia-network-operator",
		false,
		30*time.Second,
		false,
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReleaseExistsWithDifferentValues),
		"expected ErrReleaseExistsWithDifferentValues, got %v", err)
}

func TestInstallOrUpgrade_UpgradeOnOverwrite(t *testing.T) {
	actionCfg := newTestActionConfig(t)
	deployed := map[string]interface{}{
		"sriovNetworkOperator": map[string]interface{}{"enabled": false},
	}
	seedRelease(t, actionCfg, deployed)

	generated := map[string]interface{}{
		"sriovNetworkOperator": map[string]interface{}{"enabled": true},
	}
	loader := func() (*chart.Chart, error) { return newTestChart(t), nil }

	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		generated,
		"0.0.0",
		"nvidia-network-operator",
		true, // overwriteExisting
		30*time.Second,
		false,
	)
	require.NoError(t, err)

	// History now has 2 revisions: the seeded v1 and the upgrade v2.
	hist, err := actionCfg.Releases.History(helmclient.DefaultReleaseName)
	require.NoError(t, err)
	assert.Len(t, hist, 2)

	latest, err := actionCfg.Releases.Last(helmclient.DefaultReleaseName)
	require.NoError(t, err)
	assert.Equal(t, 2, latest.Version)
	assert.Equal(t, generated, latest.Config)
}

// (UnmarshalValues + DeepEqualValues tests moved to
// pkg/networkoperatorplugin/preflight/values_diff_test.go — they are
// shared between deploy's Phase 0 conflict gate and the preflight checks.)

func TestInstallOrUpgrade_ChartVersionConflictWithoutOverwrite(t *testing.T) {
	// Seed a release whose chart Version differs from what the caller
	// asks to install. With overwriteExisting=false, the chart-version
	// gate must fire BEFORE the values gate.
	actionCfg := newTestActionConfig(t)
	seededValues := map[string]interface{}{"k": "v"}
	rel := &release.Release{
		Name:      helmclient.DefaultReleaseName,
		Namespace: "nvidia-network-operator",
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name:       networkOperatorChartName,
				Version:    "1.0.0", // deployed
				APIVersion: chart.APIVersionV2,
			},
		},
		Config: seededValues,
	}
	require.NoError(t, actionCfg.Releases.Create(rel))

	loader := func() (*chart.Chart, error) {
		t.Fatal("chart loader must not be called when chart-version conflict short-circuits")
		return nil, nil
	}
	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		seededValues, // same values — so only chart version differs
		"2.0.0",      // expected
		"nvidia-network-operator",
		false, // no overwrite
		30*time.Second,
		false,
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReleaseExistsWithDifferentChartVersion),
		"expected ErrReleaseExistsWithDifferentChartVersion, got %v", err)
}

func TestInstallOrUpgrade_ChartVersionUpgradeOnOverwrite(t *testing.T) {
	actionCfg := newTestActionConfig(t)
	seededValues := map[string]interface{}{"k": "v"}
	rel := &release.Release{
		Name:      helmclient.DefaultReleaseName,
		Namespace: "nvidia-network-operator",
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name:       networkOperatorChartName,
				Version:    "1.0.0",
				APIVersion: chart.APIVersionV2,
			},
		},
		Config: seededValues,
	}
	require.NoError(t, actionCfg.Releases.Create(rel))

	loader := func() (*chart.Chart, error) { return newTestChart(t), nil }
	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		seededValues,
		"2.0.0", // expected version differs
		"nvidia-network-operator",
		true, // overwrite
		30*time.Second,
		false,
	)
	require.NoError(t, err)

	latest, err := actionCfg.Releases.Last(helmclient.DefaultReleaseName)
	require.NoError(t, err)
	assert.Equal(t, 2, latest.Version)
}

func TestInstallOrUpgrade_DetectsStuckPendingRelease(t *testing.T) {
	// A previous run crashed mid-helm and left the release in
	// pending-upgrade. Any subsequent install/upgrade must surface
	// the typed sentinel instead of helm's "another operation in
	// progress" mid-flight error.
	actionCfg := newTestActionConfig(t)
	rel := &release.Release{
		Name:      helmclient.DefaultReleaseName,
		Namespace: "nvidia-network-operator",
		Version:   1,
		Info:      &release.Info{Status: release.StatusPendingUpgrade},
		Chart:     newTestChart(t),
		Config:    map[string]interface{}{"k": "v"},
	}
	require.NoError(t, actionCfg.Releases.Create(rel))

	loader := func() (*chart.Chart, error) {
		t.Fatal("chart loader must not be called when release is stuck")
		return nil, nil
	}
	err := installOrUpgradeWithLoader(
		context.Background(),
		actionCfg,
		loader,
		map[string]interface{}{"k": "v"},
		"0.0.0",
		"nvidia-network-operator",
		true, // even with overwrite, the stuck gate fires first
		30*time.Second,
		false,
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReleaseStuckInPendingState),
		"expected ErrReleaseStuckInPendingState, got %v", err)
}

func TestInstallOrUpgrade_RejectsEmptyCfg(t *testing.T) {
	err := InstallOrUpgrade(context.Background(), nil, nil, []byte("nfd: {}\n"), false, 30*time.Second, false)
	require.Error(t, err)
}
