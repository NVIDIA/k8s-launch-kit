// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

type deployRunner struct{}

// NewDeployRunner returns the production standalone Host deploy service.
func NewDeployRunner() DeployRunner {
	return deployRunner{}
}

func (deployRunner) Run(ctx context.Context, request DeployRequest) error {
	resolved, err := ResolveKubeconfig(request.Kubeconfig)
	if err != nil {
		return apperrors.NewValidationError(
			"kubeconfig required for deploy",
			err,
			"Set $KUBECONFIG or pass --kubeconfig <path>",
		)
	}
	manifestDir, err := ResolveDeploymentDir(request.DeploymentFiles)
	if err != nil {
		return apperrors.NewValidationError(
			"deployment files directory not found",
			err,
			"Run 'l8k generate' first or pass --deployment-files <path>",
		)
	}

	log.Log.Info("Deploying manifests", "kubeconfig", resolved, "manifestDir", manifestDir, "dryRun", request.DryRun)
	k8sClient, restConfig, err := kubeclient.New(resolved)
	if err != nil {
		return apperrors.NewClusterError(
			"failed to create Kubernetes client",
			err,
			"Check that kubeconfig is valid and the cluster is reachable",
		)
	}

	uiOutput, _ := ui.NewOutputForFormat(request.OutputFormat, request.AutoApprove)
	if request.Quiet && request.OutputFormat != "json" {
		uiOutput = ui.NewSilent()
	}
	ctx = ui.WithOutput(ctx, uiOutput)
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}

	if request.DryRun {
		uiOutput.Section("Dry Run: Cluster Deployment (preview)")
		uiOutput.Info("Manifests in %s will be sent to the cluster with server-side dry-run.", manifestDir)
	} else {
		uiOutput.Section("Cluster Deployment")
		uiOutput.Info("Applying manifests from %s", manifestDir)
	}

	deployOpts := networkoperatorplugin.DeployOptions{
		LaunchKitVersion:  request.LaunchKitVersion,
		DryRun:            request.DryRun,
		OverwriteExisting: request.OverwriteExisting,
		SkipHelmChart:     request.SkipNetworkOperatorHelm.Set && request.SkipNetworkOperatorHelm.Value,
		RestConfig:        restConfig,
	}
	cfg, cfgPath, cfgErr := LoadUserConfig(UserConfigInput{
		Explicit:        request.UserConfig,
		DeploymentFiles: request.DeploymentFiles,
		ConfigDir:       request.ConfigDir,
	}, options.Options{
		ConfigDir:                request.ConfigDir,
		UserConfig:               request.UserConfig,
		NetworkOperatorNamespace: request.OperatorNamespace,

		SkipNetworkOperatorHelm:    request.SkipNetworkOperatorHelm.Value,
		SkipNetworkOperatorHelmSet: request.SkipNetworkOperatorHelm.Set,
	})
	if cfgErr != nil {
		return apperrors.NewValidationError(
			"failed to load user config",
			cfgErr,
			"Verify the YAML is parseable and networkOperator.selectedRelease is set to a supported MAJOR.MINOR (e.g. 26.4), or re-run `l8k discover`",
		)
	}
	if cfg != nil {
		deployOpts.NetworkOperator = cfg.NetworkOperator
		if cfg.NetworkOperator != nil {
			deployOpts.SkipHelmChart = cfg.NetworkOperator.SkipHelmChart
		}
		if cfg.DOCADriver != nil {
			deployOpts.DOCAVersion = cfg.DOCADriver.Version
		}
		log.Log.V(1).Info("Loaded user config for deploy",
			"path", cfgPath,
			"selectedRelease", selectedReleaseFromConfig(cfg))
	}
	if err := networkoperatorplugin.ApplyManifestsFromDir(ctx, k8sClient, manifestDir, deployOpts); err != nil {
		var structured *apperrors.StructuredError
		if errors.As(err, &structured) {
			return structured
		}
		return apperrors.NewDeploymentError(
			"deployment failed",
			err,
			"Check cluster connectivity, RBAC, and manifest validity.",
		)
	}

	if request.DryRun {
		uiOutput.Success("Dry run completed — no changes were applied")
	} else {
		uiOutput.Success("Deployment completed")
	}
	return nil
}

func selectedReleaseFromConfig(cfg *config.LaunchKitConfig) string {
	if cfg == nil || cfg.NetworkOperator == nil {
		return ""
	}
	return cfg.NetworkOperator.SelectedRelease
}
