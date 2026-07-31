// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	chartloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

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

func TestCredentialsFromImagePullSecrets_NGCRegistryCredential(t *testing.T) {
	const (
		namespace  = "nvidia-network-operator"
		secretName = "ngc-image-secret"
		username   = "$oauthtoken"
		password   = "test-api-key"
	)

	dockerConfig, err := json.Marshal(dockerConfigJSON{
		Auths: map[string]dockerAuthConfig{
			"nvcr.io": {
				Username: username,
				Password: password,
			},
		},
	})
	require.NoError(t, err)

	clientset := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig},
	})

	credentials, err := credentialsFromImagePullSecrets(
		context.Background(),
		clientset.CoreV1().Secrets(namespace),
		[]string{secretName},
		"https://helm.ngc.nvidia.com/nvstaging/mellanox",
		namespace,
	)
	require.NoError(t, err)
	require.Len(t, credentials, 1)
	assert.Equal(t, username, credentials[0].Username)
	assert.Equal(t, password, credentials[0].Password)
	assert.Equal(t, secretName, credentials[0].SourceSecret)
}

func TestCredentialsFromImagePullSecrets_ExactHelmHostLegacySecret(t *testing.T) {
	const (
		namespace  = "operator-system"
		secretName = "chart-creds"
	)

	auth := base64.StdEncoding.EncodeToString([]byte("chart-user:chart-password"))
	dockerConfig, err := json.Marshal(map[string]dockerAuthConfig{
		"https://charts.example.com/v1/": {Auth: auth},
	})
	require.NoError(t, err)

	clientset := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeDockercfg,
		Data:       map[string][]byte{corev1.DockerConfigKey: dockerConfig},
	})

	credentials, err := credentialsFromImagePullSecrets(
		context.Background(),
		clientset.CoreV1().Secrets(namespace),
		[]string{secretName},
		"https://charts.example.com/networking",
		namespace,
	)
	require.NoError(t, err)
	require.Len(t, credentials, 1)
	assert.Equal(t, "chart-user", credentials[0].Username)
	assert.Equal(t, "chart-password", credentials[0].Password)
}

func TestCredentialsFromImagePullSecrets_DoesNotForwardUnrelatedRegistry(t *testing.T) {
	const (
		namespace  = "nvidia-network-operator"
		secretName = "unrelated-registry"
	)

	dockerConfig, err := json.Marshal(dockerConfigJSON{
		Auths: map[string]dockerAuthConfig{
			"registry.internal.example.com": {
				Username: "private-user",
				Password: "private-password",
			},
		},
	})
	require.NoError(t, err)

	clientset := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig},
	})

	credentials, err := credentialsFromImagePullSecrets(
		context.Background(),
		clientset.CoreV1().Secrets(namespace),
		[]string{secretName},
		"https://helm.ngc.nvidia.com/nvidia",
		namespace,
	)
	require.NoError(t, err)
	assert.Empty(t, credentials)
}

func TestCredentialsFromImagePullSecrets_MissingSecret(t *testing.T) {
	const namespace = "nvidia-network-operator"
	clientset := k8sfake.NewSimpleClientset()

	credentials, err := credentialsFromImagePullSecrets(
		context.Background(),
		clientset.CoreV1().Secrets(namespace),
		[]string{"missing-secret"},
		"https://helm.ngc.nvidia.com/nvidia",
		namespace,
	)
	require.Error(t, err)
	assert.Empty(t, credentials)
	assert.Contains(t, err.Error(), "missing-secret")
	assert.Contains(t, err.Error(), namespace)
}

func TestPullChart_UsesImagePullSecretCredentials(t *testing.T) {
	const (
		username = "$oauthtoken"
		password = "test-api-key"
		version  = "0.0.0"
	)

	chartPath, err := chartutil.Save(newTestChart(t), t.TempDir())
	require.NoError(t, err)
	chartArchive, err := os.ReadFile(chartPath)
	require.NoError(t, err)

	var totalRequests atomic.Int32
	var authenticatedRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalRequests.Add(1)
		requestUsername, requestPassword, ok := r.BasicAuth()
		if !ok || requestUsername != username || requestPassword != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticatedRequests.Add(1)

		switch r.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  network-operator:
  - apiVersion: v2
    name: network-operator
    version: %s
    urls:
    - %s/network-operator-%s.tgz
`, version, server.URL, version)
		case "/network-operator-" + version + ".tgz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(chartArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pulledPath, cleanup, err := pullChart(
		context.Background(),
		server.URL,
		networkOperatorChartName,
		version,
		[]helmRepositoryCredential{
			{
				Username:     "expired-user",
				Password:     "expired-password",
				SourceSecret: "expired-secret",
			},
			{
				Username:     username,
				Password:     password,
				SourceSecret: "ngc-image-secret",
			},
		},
	)
	require.NoError(t, err)
	defer cleanup()

	pulledChart, err := chartloader.Load(pulledPath)
	require.NoError(t, err)
	assert.Equal(t, networkOperatorChartName, pulledChart.Name())
	assert.EqualValues(t, 2, authenticatedRequests.Load(), "index and chart archive must both use credentials")
	assert.EqualValues(t, 3, totalRequests.Load(), "downloader must try configured credentials in order")
}

func TestPullChart_DoesNotForwardCredentialsToCrossHostChart(t *testing.T) {
	const (
		username = "chart-user"
		password = "chart-password"
		version  = "0.0.0"
	)

	chartPath, err := chartutil.Save(newTestChart(t), t.TempDir())
	require.NoError(t, err)
	chartArchive, err := os.ReadFile(chartPath)
	require.NoError(t, err)

	var archiveReceivedAuth atomic.Bool
	archiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, received := r.BasicAuth()
		archiveReceivedAuth.Store(received)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(chartArchive)
	}))
	defer archiveServer.Close()

	indexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestUsername, requestPassword, ok := r.BasicAuth()
		if !ok || requestUsername != username || requestPassword != password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  network-operator:
  - apiVersion: v2
    name: network-operator
    version: %s
    urls:
    - %s/network-operator-%s.tgz
`, version, archiveServer.URL, version)
	}))
	defer indexServer.Close()

	_, cleanup, err := pullChart(
		context.Background(),
		indexServer.URL,
		networkOperatorChartName,
		version,
		[]helmRepositoryCredential{{
			Username:     username,
			Password:     password,
			SourceSecret: "chart-creds",
		}},
	)
	require.NoError(t, err)
	defer cleanup()
	assert.False(t, archiveReceivedAuth.Load(), "repository credentials must not cross chart hosts")
}
