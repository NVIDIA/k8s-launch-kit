// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestValidatePresetBundleRetainsDiscoveredHostMismatch(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..")))
	for _, explicitEnable := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-container-enable=%t", explicitEnable), func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "discovered.yaml")
			require.NoError(t, os.WriteFile(input, []byte(`networkOperator: {selectedRelease: "26.10"}
profile: {fabric: ethernet, deployment: host_device}
validation: {connectivity: false}
clusterConfig:
  - identifier: discovered-source
    workerNodes: [worker-a]
discoveredDocaHost:
  - {node: worker-a, status: installed, version: 3.5.0-1}
`), 0o600))
			output := filepath.Join(t.TempDir(), "deployment")
			opts := options.Options{UserConfig: input, ForPreset: "GB300-DGX-Station-NVIDIA-GB300",
				NodeSelector: "role=preset-render", SaveDeploymentFiles: output,
				EnabledPlugins: []string{networkoperatorplugin.PluginName}, Quiet: true}
			if explicitEnable {
				opts.EnableDocaDriver = &explicitEnable
			}
			require.NoError(t, app.New(opts).Run(), "use the production generation workflow to write bundle metadata")
			resolved, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(output))
			require.NoError(t, err)
			assert.Empty(t, resolved.ClusterConfig[0].WorkerNodes)
			require.NotNil(t, resolved.DiscoveredDOCAHostScope)
			assert.Equal(t, []string{"worker-a"}, resolved.DiscoveredDOCAHostScope.WorkerNodes)
			assert.Equal(t, explicitEnable, resolved.DOCADriver.Enable)
			called := false
			runner := validateRunner{newKubeClient: func(string) (client.Client, *rest.Config, error) {
				called = true
				return nil, nil, fmt.Errorf("unexpected cluster access")
			}}
			report := filepath.Join(t.TempDir(), "report.html")
			err = runner.Run(context.Background(), ValidateRequest{DeploymentFiles: output,
				Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), ReportPath: report, OutputFormat: "json"})
			require.ErrorContains(t, err, "does not match Network Operator 26.10 release SBOM DOCA 3.6.0")
			assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
			assert.False(t, called, "saved discovery compatibility must gate validation before Kubernetes access")
			assert.FileExists(t, report)
			resolved.DiscoveredDOCAHostScope.WorkerNodes = nil
			_, err = config.WriteEffectiveConfig(output, resolved)
			require.NoError(t, err)
			err = runner.Run(context.Background(), ValidateRequest{DeploymentFiles: output,
				Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), ReportPath: report, OutputFormat: "json"})
			require.ErrorContains(t, err, "membershipKnown=true requires nonempty workerNodes")
			assert.False(t, called, "malformed preserved scope must not turn missing evidence into a compatibility pass")
		})
	}
}
