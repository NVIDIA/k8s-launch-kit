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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

const (
	sosreportScriptName = "kubectl-netop_sosreport"
	sosreportScriptURL  = "https://raw.githubusercontent.com/Mellanox/network-operator/refs/heads/master/scripts/sosreport/kubectl-netop_sosreport"
)

var sosreportOutputDir string

var sosreportCmd = &cobra.Command{
	Use:   "sosreport",
	Short: "Collect diagnostic sosreport from a Kubernetes cluster",
	Long: `Collect a sosreport diagnostic dump from a live Kubernetes cluster
using the Network Operator sosreport script.

The sosreport contains NicClusterPolicy, pod logs, node info, CRDs,
and other diagnostic data useful for troubleshooting.`,
	Example: `  # Collect sosreport
  l8k sosreport --kubeconfig ~/.kube/config --output-dir ./sosreport

  # Uses $KUBECONFIG if set
  l8k sosreport --output-dir ./sosreport`,
	Run: func(cmd *cobra.Command, args []string) {
		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"kubeconfig required for sosreport collection",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			), outputFormat)
		}

		// Find the sosreport script
		scriptPath, installPath, err := findSosreportScript()
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"sosreport script not found",
				err,
				manualSosreportInstallSuggestion(installPath),
			), outputFormat)
		}

		// Ensure output directory exists
		if err := os.MkdirAll(sosreportOutputDir, 0755); err != nil {
			exitWithError(apperrors.NewGeneralError(
				fmt.Sprintf("failed to create output directory: %s", sosreportOutputDir), err,
			), outputFormat)
		}

		fmt.Printf("Collecting sosreport from cluster...\n")
		fmt.Printf("  Kubeconfig: %s\n", resolved)
		fmt.Printf("  Output:     %s\n", sosreportOutputDir)

		// Run the sosreport script
		sosCmd := exec.Command(scriptPath, "--kubeconfig", resolved, "--output-dir", sosreportOutputDir) //nolint:gosec
		sosCmd.Stdout = os.Stdout
		sosCmd.Stderr = os.Stderr
		if err := sosCmd.Run(); err != nil {
			exitWithError(apperrors.NewClusterError(
				"sosreport collection failed",
				err,
				"Check cluster connectivity and ensure the Network Operator is installed",
			), outputFormat)
		}

		fmt.Printf("\nSosreport collected: %s\n", sosreportOutputDir)
	},
}

// findSosreportScript looks for the sosreport script in known locations.
func findSosreportScript() (string, string, error) {
	invokedExecutablePath := executableInvocationPath()
	resolvedExecutablePath, _ := os.Executable()
	candidates, installPath := sosreportScriptLocations(invokedExecutablePath, resolvedExecutablePath)
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, installPath, nil
		}
	}
	return "", installPath, fmt.Errorf("checked: %v", candidates)
}

func executableInvocationPath() string {
	path, err := exec.LookPath(os.Args[0])
	if err != nil {
		return ""
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return absolutePath
}

func sosreportScriptLocations(invokedExecutablePath, resolvedExecutablePath string) ([]string, string) {
	candidates := []string{
		filepath.Join("scripts", sosreportScriptName),
		filepath.Join("/usr/local/share/l8k/scripts", sosreportScriptName),
	}
	installPath := candidates[len(candidates)-1]
	if invokedExecutablePath != "" {
		installPath = sosreportScriptPathForExecutable(invokedExecutablePath)
		candidates = appendUniquePath(candidates, installPath)
	}
	if resolvedExecutablePath != "" {
		resolvedInstallPath := sosreportScriptPathForExecutable(resolvedExecutablePath)
		if invokedExecutablePath == "" {
			installPath = resolvedInstallPath
		}
		candidates = appendUniquePath(candidates, resolvedInstallPath)
	}
	return candidates, installPath
}

func sosreportScriptPathForExecutable(executablePath string) string {
	return filepath.Join(filepath.Dir(executablePath), "..", "share", "l8k", "scripts", sosreportScriptName)
}

func appendUniquePath(paths []string, path string) []string {
	for _, existingPath := range paths {
		if existingPath == path {
			return paths
		}
	}
	return append(paths, path)
}

func manualSosreportInstallSuggestion(installPath string) string {
	return fmt.Sprintf(
		"Download %s and place it at %s next to the l8k installation with executable permissions",
		sosreportScriptURL,
		installPath,
	)
}

func init() {
	rootCmd.AddCommand(sosreportCmd)

	sosreportCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (falls back to $KUBECONFIG, then ~/.kube/config)")
	sosreportCmd.Flags().StringVar(&sosreportOutputDir, "output-dir", "./sosreport", "Directory to save the sosreport")

	setFlagGroup(sosreportCmd, "kubeconfig", GroupCommon)
	setFlagGroup(sosreportCmd, "output-dir", GroupGeneration)
}
