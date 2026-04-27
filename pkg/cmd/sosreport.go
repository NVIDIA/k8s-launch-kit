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
		scriptPath, err := findSosreportScript()
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"sosreport script not found",
				err,
				"Run 'make download-sosreport' to download the script",
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
func findSosreportScript() (string, error) {
	candidates := []string{
		"scripts/kubectl-netop_sosreport",
		"/usr/local/share/l8k/scripts/kubectl-netop_sosreport",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "share", "l8k", "scripts", "kubectl-netop_sosreport"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("checked: %v", candidates)
}

func init() {
	rootCmd.AddCommand(sosreportCmd)

	sosreportCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (falls back to $KUBECONFIG)")
	sosreportCmd.Flags().StringVar(&sosreportOutputDir, "output-dir", "./sosreport", "Directory to save the sosreport")
}
