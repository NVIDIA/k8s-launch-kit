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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

const (
	sosreportScriptName  = "kubectl-netop_sosreport"
	sosreportScriptURL   = "https://raw.githubusercontent.com/Mellanox/network-operator/refs/heads/master/scripts/sosreport/kubectl-netop_sosreport"
	sosreportHTTPTimeout = 30 * time.Second
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

		// Reuse an existing sosreport script or download it next to the l8k installation.
		scriptPath, installPath, err := resolveSosreportScript(cmd.Context())
		if err != nil {
			exitWithError(apperrors.NewValidationError(
				"sosreport script unavailable",
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

func resolveSosreportScript(ctx context.Context) (string, string, error) {
	executablePath, _ := os.Executable()
	candidates, installPath := sosreportScriptLocations(executablePath)
	scriptPath, err := findOrDownloadSosreportScript(
		ctx,
		&http.Client{Timeout: sosreportHTTPTimeout},
		sosreportScriptURL,
		candidates,
		installPath,
	)
	return scriptPath, installPath, err
}

// sosreportScriptLocations returns the lookup order and the installation-adjacent
// path used to cache a lazily downloaded script.
func sosreportScriptLocations(executablePath string) ([]string, string) {
	candidates := []string{
		filepath.Join("scripts", sosreportScriptName),
		filepath.Join("/usr/local/share/l8k/scripts", sosreportScriptName),
	}
	installPath := candidates[len(candidates)-1]
	if executablePath != "" {
		installPath = filepath.Join(filepath.Dir(executablePath), "..", "share", "l8k", "scripts", sosreportScriptName)
		candidates = append(candidates, installPath)
	}
	return candidates, installPath
}

func findOrDownloadSosreportScript(
	ctx context.Context,
	client *http.Client,
	sourceURL string,
	candidates []string,
	installPath string,
) (string, error) {
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	if err := downloadSosreportScript(ctx, client, sourceURL, installPath); err != nil {
		return "", fmt.Errorf("failed to download sosreport script from %s to %s: %w", sourceURL, installPath, err)
	}
	return installPath, nil
}

func downloadSosreportScript(ctx context.Context, client *http.Client, sourceURL, installPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request script: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request script: HTTP %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(installPath), ".sosreport-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	written, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if written == 0 {
		return fmt.Errorf("downloaded script is empty")
	}
	if err := tmpFile.Chmod(0o755); err != nil {
		return fmt.Errorf("make temporary file executable: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, installPath); err != nil {
		return fmt.Errorf("install script: %w", err)
	}
	return nil
}

func manualSosreportInstallSuggestion(installPath string) string {
	return fmt.Sprintf(
		"Download %s and place it at %s (the share directory next to the l8k installation) with executable permissions",
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
