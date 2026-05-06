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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
)

// defaultUserConfigPath is the path l8k discover writes by default and
// validate looks for if --user-config is not specified.
const defaultUserConfigPath = "./cluster-config.yaml"

// defaultOperatorNamespace is the default Network Operator namespace used
// when no user-config is supplied to validate.
const defaultOperatorNamespace = "nvidia-network-operator"

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Verify a deployment matches the selected Network Operator release",
	Long: `Validate that a previously generated deployment is correctly applied to
the cluster.

Two checks are run:

  1. Network Operator Helm release version: the chart's appVersion is
     compared against the version expected by the user's
     networkOperator.selectedRelease (looked up in the embedded catalog).
     Skipped when no user-config is found or no Helm release Secret matches.

  2. Manifest presence: every YAML manifest under --deployment-files
     (excluding example workloads) is fetched from the cluster. Each
     manifest is reported as Found, Missing, or Error.

Exits non-zero on any missing manifest or version mismatch.`,
	Example: `  # Validate using defaults (./cluster-config.yaml + ./deployment, $KUBECONFIG)
  l8k validate

  # Specify paths and kubeconfig
  l8k validate --user-config ./cluster-config.yaml \
    --deployment-files ./deployment \
    --kubeconfig ~/.kube/config

  # Agent mode (JSON output)
  l8k validate --output json --yes 2>/dev/null`,
	Run: func(cmd *cobra.Command, args []string) {
		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"kubeconfig required for validate",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			), outputFormat)
		}

		manifestDir, err := resolveDeploymentDir(deploymentFiles)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"deployment files directory not found",
				err,
				"Run 'l8k generate' first or pass --deployment-files <path>",
			), outputFormat)
		}

		// Best-effort load of user-config — only the networkOperator section
		// is required by validate. Missing or unparseable config softens the
		// version check to "skipped" but does not fail the manifest check.
		operatorNamespace := defaultOperatorNamespace
		selectedRelease := ""
		if path := userConfigPath(); path != "" {
			if cfg, err := config.LoadFullConfig(path, log.Log); err == nil && cfg != nil {
				if cfg.NetworkOperator.Namespace != "" {
					operatorNamespace = cfg.NetworkOperator.Namespace
				}
				selectedRelease = cfg.NetworkOperator.SelectedRelease
			} else if err != nil {
				log.Log.V(1).Info("user-config not loaded; version check will be skipped",
					"path", path, "error", err.Error())
			}
		}

		log.Log.Info("Validating deployment",
			"kubeconfig", resolved,
			"manifestDir", manifestDir,
			"operatorNamespace", operatorNamespace,
			"selectedRelease", selectedRelease)

		k8sClient, _, err := kubeclient.New(resolved)
		if err != nil {
			exitWithError(apperrors.NewClusterError(
				"failed to create Kubernetes client",
				err,
				"Check that kubeconfig is valid and the cluster is reachable",
			), outputFormat)
		}

		ctx := context.Background()

		versionCheck, vcErr := networkoperatorplugin.CheckHelmReleaseVersion(ctx, k8sClient, operatorNamespace, selectedRelease)
		if vcErr != nil {
			exitWithError(apperrors.NewClusterError(
				"version check failed",
				vcErr,
				"Check that the kubeconfig has list-secrets permission in the operator namespace",
			), outputFormat)
		}

		results, valErr := networkoperatorplugin.ValidateManifests(ctx, k8sClient, manifestDir)
		if valErr != nil {
			exitWithError(apperrors.NewGeneralError(
				"manifest validation failed",
				valErr,
			), outputFormat)
		}

		ok := emitValidationReport(versionCheck, results, outputFormat)
		if !ok {
			os.Exit(apperrors.ExitDeployment)
		}
	},
}

// userConfigPath returns the user-config path to read, defaulting to
// ./cluster-config.yaml when --user-config was not specified. Returns an
// empty string if the resolved path doesn't exist (so the caller can
// proceed with a "skipped" version check).
func userConfigPath() string {
	path := userConfig
	if path == "" {
		path = defaultUserConfigPath
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// emitValidationReport prints results in text or JSON; returns true when
// every manifest is Found and the version check matched (or was skipped).
func emitValidationReport(vc *networkoperatorplugin.VersionCheck, results []networkoperatorplugin.ValidationResult, format string) bool {
	missing := 0
	for _, r := range results {
		if r.Missing || r.Detail != "" {
			missing++
		}
	}
	versionOK := vc == nil || vc.Skipped || vc.Match

	if format == "json" {
		out := map[string]any{
			"versionCheck": vc,
			"manifests":    results,
			"summary": map[string]any{
				"totalManifests":  len(results),
				"missingManifests": missing,
				"versionMatch":    versionOK,
			},
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return missing == 0 && versionOK
	}

	fmt.Println("Network Operator release")
	if vc == nil || vc.Skipped {
		reason := "skipped"
		if vc != nil && vc.Reason != "" {
			reason = vc.Reason
		}
		fmt.Printf("  status: SKIPPED (%s)\n", reason)
	} else {
		match := "MISMATCH"
		if vc.Match {
			match = "MATCH"
		}
		fmt.Printf("  selectedRelease: %s\n", vc.SelectedRelease)
		fmt.Printf("  expected version: %s\n", vc.ExpectedVersion)
		if vc.DeployedRelease != nil {
			fmt.Printf("  deployed: %s (chart=%s app=%s rev=%d status=%s)\n",
				vc.DeployedRelease.Name,
				vc.DeployedRelease.ChartVersion,
				vc.DeployedRelease.AppVersion,
				vc.DeployedRelease.Revision,
				vc.DeployedRelease.Status)
		}
		fmt.Printf("  result: %s\n", match)
	}

	fmt.Println()
	fmt.Println("Manifests")
	if len(results) == 0 {
		fmt.Println("  (no manifests to validate)")
	}
	for _, r := range results {
		status := "FOUND"
		if r.Missing {
			status = "MISSING"
		} else if r.Detail != "" && !r.Found {
			status = "ERROR"
		}
		ns := r.Namespace
		if ns == "" {
			ns = "(cluster-scoped)"
		}
		line := fmt.Sprintf("  [%s] %s/%s in %s", status, r.Kind, r.Name, ns)
		if r.Detail != "" {
			line = fmt.Sprintf("%s — %s", line, r.Detail)
		}
		fmt.Println(line)
	}

	fmt.Println()
	fmt.Printf("Summary: %d manifests, %d missing/error; version: %s\n",
		len(results), missing, versionStatusText(vc))
	return missing == 0 && versionOK
}

func versionStatusText(vc *networkoperatorplugin.VersionCheck) string {
	if vc == nil || vc.Skipped {
		return "skipped"
	}
	if vc.Match {
		return "match"
	}
	return "mismatch"
}

func init() {
	rootCmd.AddCommand(validateCmd)

	validateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG)")
	validateCmd.Flags().StringVar(&deploymentFiles, "deployment-files", DefaultDeploymentDir, "Directory containing the manifests to verify")
	validateCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file (auto-detected from ./cluster-config.yaml). Used to read networkOperator.selectedRelease and operator namespace.")

	setFlagGroup(validateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(validateCmd, "user-config", GroupCommon)
	setFlagGroup(validateCmd, "deployment-files", GroupGeneration)
}
