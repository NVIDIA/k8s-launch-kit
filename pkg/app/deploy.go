// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type bundleDeployer interface {
	DeployBundle(context.Context, client.Client, *bundle.Bundle) error
}

// executeDeploy handles the deployment phase for all found profiles.
func (l *Launcher) executeDeploy() error {
	l.result.Phase = "deploy"
	if l.options.DryRun {
		l.result.DryRun = true
		l.ui.Section("Dry Run: Cluster Deployment (preview)")
		l.ui.Info("The following manifests would be applied to the cluster:")
		for _, profile := range l.foundProfiles {
			manifestDir := filepath.Join(l.options.SaveDeploymentFiles, profile.Plugin)
			l.ui.Info("  Profile: %s (from %s)", profile.Name, manifestDir)
		}
		l.ui.Success("Dry run completed — no changes were applied")
	} else {
		l.ui.Section("Cluster Deployment")
		for _, profile := range l.foundProfiles {
			if err := l.deployConfigurationProfile(&profile); err != nil {
				l.ui.Error("Deployment failed: %v", err)
				var structured *apperrors.StructuredError
				if errors.As(err, &structured) {
					return structured
				}
				return apperrors.NewDeploymentError("deployment failed", err,
					"Check cluster connectivity and resource permissions")
			}
		}
		l.result.Deployed = true
	}
	return nil
}

// deployConfigurationProfile handles cluster deployment for a single profile.
func (l *Launcher) deployConfigurationProfile(profile *profiles.Profile) error {
	if !l.options.Deploy {
		l.logger.Info("Skipped (deploy not requested)")
		return nil
	}

	l.ui.Info("Deploying profile: %s", profile.Name)
	l.logger.Info("Deploying profile to cluster", "profile", profile.Name, "kubeconfig", l.options.Kubeconfig)

	if l.options.SaveDeploymentFiles == "" {
		l.ui.Error("Deployment requires generated files (use --save-deployment-files)")
		return fmt.Errorf("--deploy requires generated files directory; provide --save-deployment-files")
	}

	plugin, ok := l.plugins[profile.Plugin]
	if !ok {
		l.ui.Error("Plugin not found: %s", profile.Plugin)
		return fmt.Errorf("plugin %s not found", profile.Plugin)
	}

	ctx := ui.WithOutput(l.context, l.ui)
	if l.options.DeployTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.options.DeployTimeout)
		defer cancel()
	}
	var err error
	if capable, ok := plugin.(bundleDeployer); ok {
		artifacts := l.generatedBundles[generatedProfileKey{Plugin: profile.Plugin, Name: profile.Name}]
		if artifacts == nil {
			return apperrors.NewValidationError("generated artifact snapshot is missing", nil, "Regenerate the selected profile before deployment")
		}
		err = capable.DeployBundle(ctx, l.kubeClient, artifacts)
	} else {
		err = plugin.DeployProfile(ctx, profile, l.kubeClient, filepath.Join(l.options.SaveDeploymentFiles, profile.Plugin))
	}
	if err != nil {
		l.ui.Error("Deployment failed: %v", err)
		return fmt.Errorf("failed to deploy profile: %w", err)
	}

	l.ui.Success("Profile deployed: %s", profile.Name)
	l.logger.Info("Deployment profile applied successfully", "profile", profile.Name)
	return nil
}
