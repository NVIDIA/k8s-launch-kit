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
	"time"

	"github.com/spf13/cobra"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// Phase 2 connectivity-test flags. `--connectivity` defaults to ON —
// every `l8k validate` verifies the data plane (apply example DS,
// wait Ready, ping matrix, cleanup) unless the caller passes
// `--connectivity=false`. The other flags tune matrix behaviour
// (--ping-count, --connectivity-timeout, --keep) or extend the
// validate semantics (--wait blocks until in-progress manifests reach
// a terminal state).
var (
	validateConnectivity        bool
	validateKeep                bool
	validateConnectivityTimeout time.Duration
	validatePingCount           int
	validateWait                time.Duration
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

Three checks are run:

  1. Network Operator Helm release version: the chart's appVersion is
     compared against the version expected by the user's
     networkOperator.selectedRelease (looked up in the embedded catalog).
     Skipped when no user-config is found or no Helm release Secret matches.

  2. Manifest state: every YAML manifest under --deployment-files
     (excluding example workloads) is classified against the cluster via
     the per-Kind validator registry. Each manifest is reported as
     READY, IN-PROGRESS, ERROR, or MISSING.

  3. Connectivity (default ON, pass --connectivity=false to skip): the
     example DaemonSet is applied, the rollout is awaited until
     numberReady == desiredNumberScheduled > 0, and a ping matrix is
     run between the test pods' rail IPs (same-rail across every pod
     pair + one cross-rail canary per pair). The DS is deleted on exit
     unless --keep is set. Skipped when any manifest from step 2 is
     IN-PROGRESS / ERROR / MISSING (running connectivity against an
     unready cluster would just produce noise).

Exits non-zero on any missing manifest, version mismatch, or
connectivity-matrix failure.`,
	Example: `  # Full validate (manifest state + connectivity matrix)
  l8k validate

  # Manifest checks only (no DaemonSet apply, no ping matrix)
  l8k validate --connectivity=false

  # Block up to 10 minutes for in-progress manifests to finish reconciling
  l8k validate --wait 10m

  # Leave the test DaemonSet running for debugging
  l8k validate --keep

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
		var presetDeviations []groupDeviationReport
		if path := userConfigPath(); path != "" {
			if cfg, err := config.LoadFullConfig(path, log.Log); err == nil && cfg != nil {
				if cfg.NetworkOperator.Namespace != "" {
					operatorNamespace = cfg.NetworkOperator.Namespace
				}
				selectedRelease = cfg.NetworkOperator.SelectedRelease
				for _, g := range cfg.ClusterConfig {
					if len(g.PresetDeviation) == 0 {
						continue
					}
					presetDeviations = append(presetDeviations, groupDeviationReport{
						Group:       g.Identifier,
						MachineType: g.MachineType,
						GPUType:     g.GPUType,
						Deviations:  g.PresetDeviation,
					})
				}
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

		k8sClient, restConfig, err := kubeclient.New(resolved)
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

		// Optional `--wait`: poll until every in-progress manifest
		// reaches a terminal state (or the deadline elapses). The
		// loop re-runs the registry-backed validate every 10s. The
		// final results / verdict are emitted normally below.
		if validateWait > 0 {
			results = waitForReconcile(ctx, ctrlclient.Client(k8sClient), manifestDir, results, validateWait)
		}

		verdict := emitValidationReport(versionCheck, results, presetDeviations, outputFormat)

		// `--connectivity` runs the data-plane ping matrix only when
		// every CR is reconciled — otherwise we'd just produce noise.
		// In-progress (without errors) prints a warning and exits 0
		// so CI/operators can re-run later.
		switch {
		case verdict.HasError || verdict.HasMissing || !verdict.VersionOK:
			os.Exit(apperrors.ExitDeployment)
		case verdict.HasInProgress:
			if outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "\nNote: some manifests are still reconciling. Re-run later or use --wait to block.")
			}
			if validateConnectivity && outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "Connectivity matrix skipped — cluster has in-progress manifests.")
			}
			return
		}

		if validateConnectivity {
			uiOutput, _ := ui.NewOutputForFormat(outputFormat, yesFlag)
			ctxWithUI := ui.WithOutput(ctx, uiOutput)
			matrix, err := connectivity.RunMatrix(ctxWithUI, k8sClient, restConfig, uiOutput, connectivity.Options{
				ManifestDir: manifestDir,
				Timeout:     validateConnectivityTimeout,
				PingCount:   validatePingCount,
				Keep:        validateKeep,
			})
			if err != nil {
				exitWithError(apperrors.NewClusterError(
					"connectivity matrix failed",
					err,
					"See log output for the failing step; re-run with --keep to inspect the test DaemonSet",
				), outputFormat)
			}
			if outputFormat == "json" {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"connectivity": matrix,
				})
			}
			if matrix != nil && (matrix.Summary.Failed > 0 || matrix.Summary.ExecErrors > 0) {
				os.Exit(apperrors.ExitDeployment)
			}
		}
	},
}

// waitForReconcile re-runs ValidateManifests at 10s cadence until no
// manifest is in-progress, an error appears, or the deadline elapses.
// Returns the most recent results regardless of which terminal
// condition fired. Emits a one-line update only when the in-progress
// count changes so we don't flood logs on long waits.
func waitForReconcile(ctx context.Context, c ctrlclient.Client, manifestDir string, initial []networkoperatorplugin.ValidationResult, budget time.Duration) []networkoperatorplugin.ValidationResult {
	deadline := time.Now().Add(budget)
	results := initial
	lastInProgress := -1
	for {
		inProgress := 0
		for _, r := range results {
			if r.State == crstate.StateInProgress {
				inProgress++
			}
		}
		if inProgress == 0 {
			return results
		}
		if inProgress != lastInProgress {
			fmt.Fprintf(os.Stderr, "Waiting for %d manifest(s) to reconcile (budget: %s remaining)…\n",
				inProgress, time.Until(deadline).Round(time.Second))
			lastInProgress = inProgress
		}

		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "--wait deadline reached with %d manifest(s) still in progress; continuing with current snapshot.\n", inProgress)
			return results
		}
		select {
		case <-ctx.Done():
			return results
		case <-time.After(10 * time.Second):
		}

		fresh, err := networkoperatorplugin.ValidateManifests(ctx, c, manifestDir)
		if err != nil {
			// Transient — keep the previous snapshot and try again
			// on the next tick.
			log.Log.V(1).Info("ValidateManifests during --wait failed; retrying", "error", err.Error())
			continue
		}
		results = fresh
	}
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

// groupDeviationReport carries the per-group preset deviations that
// emitValidationReport surfaces alongside the version + manifest checks.
// Source: ClusterConfig.PresetDeviation in the user-supplied
// cluster-config.yaml. The deviations are recorded by `l8k discover` when
// a matched preset's PFs don't exactly match discovered hardware; validate
// re-displays them so an operator running against drifted hardware sees
// the gap every time the deployment is checked.
type groupDeviationReport struct {
	Group       string                        `json:"group"`
	MachineType string                        `json:"machineType,omitempty"`
	GPUType     string                        `json:"gpuType,omitempty"`
	Deviations  []config.PresetDeviationEntry `json:"deviations"`
}

// validationVerdict captures the aggregate outcome of a validate run.
// Phase 2's CLI uses it to decide exit code AND whether to proceed with
// the optional connectivity tests:
//
//	all manifests success                → connectivity may run; exit 0
//	any in-progress (no errors/missing)  → warning, exit 0, skip connectivity
//	any error or missing                 → exit ExitDeployment, skip connectivity
//	version mismatch                     → exit ExitDeployment regardless
type validationVerdict struct {
	OK              bool // overall pass (no errors, no missing, version OK)
	HasError        bool
	HasMissing      bool
	HasInProgress   bool
	VersionOK       bool
	SuccessCount    int
	InProgressCount int
	ErrorCount      int
	MissingCount    int
	Total           int
}

func aggregateVerdict(vc *networkoperatorplugin.VersionCheck, results []networkoperatorplugin.ValidationResult) validationVerdict {
	v := validationVerdict{
		Total:     len(results),
		VersionOK: vc == nil || vc.Skipped || vc.Match,
	}
	for _, r := range results {
		switch r.State {
		case crstate.StateSuccess:
			v.SuccessCount++
		case crstate.StateInProgress:
			v.InProgressCount++
			v.HasInProgress = true
		case crstate.StateNotDeployed:
			v.MissingCount++
			v.HasMissing = true
		case crstate.StateError:
			v.ErrorCount++
			v.HasError = true
		default:
			// Older results without State set — fall back to
			// Found/Missing for the legacy code path.
			if r.Missing {
				v.MissingCount++
				v.HasMissing = true
			} else if !r.Found {
				v.ErrorCount++
				v.HasError = true
			} else {
				v.SuccessCount++
			}
		}
	}
	v.OK = !v.HasError && !v.HasMissing && v.VersionOK
	return v
}

// emitValidationReport prints results in text or JSON and returns the
// aggregate verdict so the caller can decide on exit code and on
// whether to proceed with optional connectivity testing.
//
// Preset deviations are surfaced for visibility but do not affect the
// verdict — the deployment can run correctly while diverging from the
// certified preset.
func emitValidationReport(vc *networkoperatorplugin.VersionCheck, results []networkoperatorplugin.ValidationResult, presetDeviations []groupDeviationReport, format string) validationVerdict {
	verdict := aggregateVerdict(vc, results)

	if format == "json" {
		out := map[string]any{
			"versionCheck":     vc,
			"manifests":        results,
			"presetDeviations": presetDeviations,
			"summary": map[string]any{
				"totalManifests":   verdict.Total,
				"successManifests": verdict.SuccessCount,
				"inProgress":       verdict.InProgressCount,
				"errorManifests":   verdict.ErrorCount,
				"missingManifests": verdict.MissingCount,
				"versionMatch":     verdict.VersionOK,
				"deviationGroups":  len(presetDeviations),
				"success":          verdict.OK,
			},
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return verdict
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
		status := validationStatusLabel(r)
		ns := r.Namespace
		if ns == "" {
			ns = "(cluster-scoped)"
		}
		line := fmt.Sprintf("  [%-11s] %s/%s in %s", status, r.Kind, r.Name, ns)
		if r.Reason != "" {
			line = fmt.Sprintf("%s — %s", line, r.Reason)
		}
		fmt.Println(line)
	}

	if len(presetDeviations) > 0 {
		fmt.Println()
		fmt.Println("Preset deviations (cluster differs from matched preset)")
		for _, gd := range presetDeviations {
			label := gd.Group
			if gd.MachineType != "" || gd.GPUType != "" {
				label = fmt.Sprintf("%s (%s/%s)", gd.Group, gd.MachineType, gd.GPUType)
			}
			fmt.Printf("  %s — %d deviation(s):\n", label, len(gd.Deviations))
			for _, d := range gd.Deviations {
				expected := d.Expected
				if expected == "" {
					expected = "-"
				}
				got := d.Got
				if got == "" {
					got = "-"
				}
				fmt.Printf("    [%s] expected=%s got=%s — %s\n", d.Field, expected, got, d.Detail)
			}
		}
	}

	fmt.Println()
	fmt.Printf("Summary: %d/%d ready, %d in-progress, %d error, %d missing; version: %s; preset deviations: %d group(s)\n",
		verdict.SuccessCount, verdict.Total,
		verdict.InProgressCount, verdict.ErrorCount, verdict.MissingCount,
		versionStatusText(vc), len(presetDeviations))
	return verdict
}

// validationStatusLabel maps the per-result state to the human-readable
// label rendered in the text report.
func validationStatusLabel(r networkoperatorplugin.ValidationResult) string {
	switch r.State {
	case crstate.StateSuccess:
		return "READY"
	case crstate.StateInProgress:
		return "IN-PROGRESS"
	case crstate.StateError:
		return "ERROR"
	case crstate.StateNotDeployed:
		return "MISSING"
	}
	// Fallback for results that bypassed the registry (shouldn't
	// happen in practice).
	if r.Missing {
		return "MISSING"
	}
	if r.Detail != "" && !r.Found {
		return "ERROR"
	}
	return "READY"
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

	validateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG, then ~/.kube/config)")
	validateCmd.Flags().StringVar(&deploymentFiles, "deployment-files", DefaultDeploymentDir, "Directory containing the manifests to verify")
	validateCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file (auto-detected from ./cluster-config.yaml). Used to read networkOperator.selectedRelease and operator namespace.")

	// Phase 2 flags. `--connectivity` defaults to true — every
	// `l8k validate` exercises the data plane unless explicitly
	// disabled. Pass `--connectivity=false` to limit validate to
	// the static manifest-presence + Helm release-version checks.
	validateCmd.Flags().BoolVar(&validateConnectivity, "connectivity", true, "Run a ping matrix between pods of the example DaemonSet to verify the data plane. Default true. Pass --connectivity=false to skip when only the static manifest checks are wanted.")
	validateCmd.Flags().BoolVar(&validateKeep, "keep", false, "Leave the example DaemonSet running after --connectivity completes (useful for debugging).")
	validateCmd.Flags().DurationVar(&validateConnectivityTimeout, "connectivity-timeout", 5*time.Minute, "Wall-clock budget for the connectivity matrix (DaemonSet rollout + ping execs).")
	validateCmd.Flags().IntVar(&validatePingCount, "ping-count", 3, "Number of ICMP echoes per src→dst pair when running --connectivity (ping -c N).")
	validateCmd.Flags().DurationVar(&validateWait, "wait", 0, "Block validate up to this duration waiting for in-progress manifests to reach a terminal state. 0 (default) returns immediately on the first snapshot.")

	setFlagGroup(validateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(validateCmd, "user-config", GroupCommon)
	setFlagGroup(validateCmd, "deployment-files", GroupGeneration)
}
