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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	sigsYaml "sigs.k8s.io/yaml"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// Phase 2 connectivity-test flags. `--connectivity` defaults to ON —
// every `l8k validate` verifies the data plane (apply example DS,
// wait Ready, source-bound connectivity matrix, cleanup) unless the caller passes
// `--connectivity=false`. The other flags tune matrix behaviour
// (--connectivity-timeout, --keep) or extend the validate semantics
// (--wait blocks until in-progress manifests reach a terminal state).
//
// Phase 3 adds --report-path: emit an HTML validation report alongside
// the text/JSON output. Empty default means "auto-place at
// <deployment-files>/k8s-launch-kit-validation-report.html"; the
// literal "-" disables report writing.
var (
	validateConnectivity        bool
	validateKeep                bool
	validateConnectivityTimeout time.Duration
	validateWait                time.Duration
	validateReportPath          string
	validateMode                string
	validateChecks              []string
	validateRDMAIterations      int
	validateRDMAIBWriteSize     int
	validateRDMAIBWriteMinGbps  float64
)

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
     numberReady == desiredNumberScheduled > 0, and the configured
     source-bound checks (icmp, rping, and/or ib_write_bw) run between
     the test pods' rail IPs. validation.mode controls coverage and
     cross-rail gating: quick, full, or strict. The DS is deleted on exit
     unless --keep is set. Skipped
     when any manifest from step 2 is IN-PROGRESS / ERROR / MISSING
     (running connectivity against an unready cluster would just
     produce noise).

Exits non-zero on any missing manifest, version mismatch, or
connectivity-matrix failure.`,
	Example: `  # Full validate (manifest state + connectivity matrix)
  l8k validate

  # Manifest checks only (no DaemonSet apply, no connectivity matrix)
  l8k validate --connectivity=false

  # Only run rping, with more iterations, in full coverage mode
  l8k validate --validation-mode full --validation-checks rping --rdma-rping-iterations 100

  # Block up to 10 minutes for in-progress manifests to finish reconciling
  l8k validate --wait 10m

  # Leave the test DaemonSet running for debugging
  l8k validate --keep

  # Agent mode (JSON output)
  l8k validate --output json 2>/dev/null`,
	Run: func(cmd *cobra.Command, args []string) {
		// State accumulated during the run. Captured by reference
		// in exitWithReport so that EVERY error path — including
		// the ones that go through exitWithError(...).os.Exit —
		// still emits the HTML report with whatever was observed.
		// Go's defer doesn't run on os.Exit, so we can't rely on
		// a deferred report write.
		var (
			versionCheck      *networkoperatorplugin.VersionCheck
			componentCheck    *networkoperatorplugin.ComponentVersionCheck
			helmValuesCheck   *networkoperatorplugin.HelmValuesCheck
			strayCheck        *preflight.Result
			results           []networkoperatorplugin.ValidationResult
			matrix            *connectivity.MatrixResult
			warnings          []string
			presetDeviations  []groupDeviationReport
			presetResults     []presetmatch.Result
			reportClient      ctrlclient.Client
			reportRestConfig  *rest.Config
			reportManifestDir string
			operatorNamespace = defaultOperatorNamespace
		)

		// exitWithReport flushes the HTML report (best-effort) and
		// then calls exitWithError. Use this in place of bare
		// exitWithError everywhere a validate-level error needs to
		// terminate the run, so the operator gets a partial report
		// instead of nothing on failure.
		exitWithReport := func(err *apperrors.StructuredError) {
			if err != nil {
				warnings = append(warnings, err.Error())
			}
			// Compute the overall verdict from whatever inputs we
			// have at this exit point — verdict may not have been
			// computed yet on an early-error path, so build a
			// pessimistic FAIL with the error as the reason.
			overall := connectivity.OverallVerdict{Pass: false}
			if err != nil {
				overall.Reasons = append(overall.Reasons, err.Error())
			}
			emitVerdictBanner(overall, outputFormat)
			writeHTMLReportIfWanted(context.Background(), reportClient, reportRestConfig,
				reportManifestDir, deploymentFiles,
				operatorNamespace, versionCheck, componentCheck, helmValuesCheck, strayCheck, results, &matrix, &warnings,
				presetResults, overall, userConfigPath(), outputFormat)
			exitWithError(err, outputFormat)
		}

		presetCatalog, _, catalogErr := presetCatalogForConfigDir(configDir)
		if catalogErr != nil {
			exitWithReport(apperrors.NewValidationError(
				"invalid --config-dir",
				catalogErr,
				"Provide an accessible directory; when present, l8k-config.yaml must be a file and presets/ a directory",
			))
		}
		log.Log.V(1).Info("Using topology preset catalog", "source", presetCatalog.Source())

		resolved, err := resolveKubeconfig(kubeconfig)
		if err != nil {
			exitWithReport(apperrors.NewValidationError(
				"kubeconfig required for validate",
				err,
				"Set $KUBECONFIG or pass --kubeconfig <path>",
			))
		}

		manifestDir, err := resolveDeploymentDir(deploymentFiles)
		if err != nil {
			exitWithReport(apperrors.NewValidationError(
				"deployment files directory not found",
				err,
				"Run 'l8k generate' first or pass --deployment-files <path>",
			))
		}
		reportManifestDir = manifestDir

		// Best-effort load of user-config — only the networkOperator section
		// is required by validate. Missing or unparseable config softens the
		// version check to "skipped" but does not fail the manifest check.
		// The --network-operator-namespace override is threaded through so a
		// validate run against a namespace not recorded in cluster-config.yaml
		// can still target the right Helm release / live CRs.
		selectedRelease := ""
		profileRouting := config.RoutingDestinationBased
		validationCfg := config.DefaultValidationConfig()
		cfg, cfgPath, cfgErr := loadUserConfig(options.Options{
			ConfigDir:                configDir,
			NetworkOperatorNamespace: networkOperatorNamespace,
		})
		if cfgErr != nil {
			log.Log.V(1).Info("user-config not loaded; version check will be skipped",
				"path", cfgPath, "error", cfgErr.Error())
		} else if cfg != nil {
			if cfg.NetworkOperator != nil && cfg.NetworkOperator.Namespace != "" {
				operatorNamespace = cfg.NetworkOperator.Namespace
			}
			if cfg.NetworkOperator != nil {
				selectedRelease = cfg.NetworkOperator.SelectedRelease
			}
			if cfg.Profile != nil && cfg.Profile.Routing != "" {
				profileRouting = cfg.Profile.Routing
			}
			validationCfg = config.NormalizeValidationConfig(cfg.Validation)
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
			// Re-run preset matching at validate-time. This
			// catches drift between the cluster-config.yaml's
			// stored hardware view and the certified preset
			// even when discover wasn't re-run. Results stay
			// informational — a deviation doesn't fail
			// validate (matches the historical behaviour of
			// presetDeviation), so the exit code is unchanged.
			presetResults = presetmatch.MatchAllWithCatalog(cfg, presetCatalog)
		}
		if err := applyValidateFlagOverrides(cmd, validationCfg); err != nil {
			exitWithReport(apperrors.NewValidationError(
				"invalid validation configuration",
				err,
				"Use --validation-mode quick|full|strict, --validation-checks icmp,rping,ib_write_bw, and positive RDMA parameter values",
			))
		}

		log.Log.Info("Validating deployment",
			"kubeconfig", resolved,
			"manifestDir", manifestDir,
			"operatorNamespace", operatorNamespace,
			"selectedRelease", selectedRelease)

		k8sClient, restConfig, err := kubeclient.New(resolved)
		if err != nil {
			exitWithReport(apperrors.NewClusterError(
				"failed to create Kubernetes client",
				err,
				"Check that kubeconfig is valid and the cluster is reachable",
			))
		}
		reportClient = k8sClient
		reportRestConfig = restConfig

		ctx := context.Background()

		var vcErr error
		versionCheck, vcErr = networkoperatorplugin.CheckHelmReleaseVersion(ctx, k8sClient, operatorNamespace, selectedRelease)
		if vcErr != nil {
			exitWithReport(apperrors.NewClusterError(
				"version check failed",
				vcErr,
				"Check that the kubeconfig has list-secrets permission in the operator namespace",
			))
		}

		// Cross-check the NicClusterPolicy + NicNodePolicy
		// component versions against the catalog. Soft errors only
		// — a failed lookup turns into ComponentCheck.Skipped, not
		// a hard exit, because the underlying Helm release check
		// already covers the dominant "wrong operator version"
		// case. The per-component breakdown is most useful when
		// catching out-of-band kubectl edits or partial upgrades.
		ccCheck, ccErr := networkoperatorplugin.CheckComponentVersions(ctx, k8sClient, operatorNamespace, selectedRelease)
		if ccErr != nil {
			log.Log.V(1).Info("component-version check failed", "error", ccErr.Error())
		}
		componentCheck = ccCheck

		// Helm values drift: compare the deployed release's
		// user-supplied values against the values.yaml that
		// `l8k generate` produced. Mismatches mean re-running
		// `l8k deploy` would change the chart configuration —
		// exit code 4, same as a version mismatch.
		valuesPath := filepath.Join(manifestDir, "values.yaml")
		var generatedValuesYAML []byte
		if b, err := os.ReadFile(valuesPath); err == nil {
			generatedValuesYAML = b
		}
		hvCheck, hvErr := networkoperatorplugin.CheckHelmReleaseValues(ctx, restConfig, operatorNamespace, generatedValuesYAML)
		if hvErr != nil {
			log.Log.V(1).Info("helm-values check failed", "error", hvErr.Error())
		}
		helmValuesCheck = hvCheck

		// Stray-CRs: any Network Operator-managed CR in the operator
		// namespace (or cluster-scoped Kinds cluster-wide) that l8k did
		// NOT render. Surfaces as a soft fail — the verdict picks it up,
		// the HTML report lists every offender, and the user can sweep
		// them with `l8k deploy --overwrite-existing`.
		genRefs, scanErr := preflight.ScanGeneratedManifests(manifestDir)
		if scanErr != nil {
			log.Log.V(1).Info("stray-CR scan failed", "error", scanErr.Error())
		}
		stray := preflight.CheckStrayCRs(ctx, preflight.Inputs{
			KubeClient:         k8sClient,
			OperatorNamespace:  operatorNamespace,
			GeneratedManifests: genRefs,
		})
		strayCheck = &stray

		var valErr error
		results, valErr = networkoperatorplugin.ValidateManifests(ctx, k8sClient, manifestDir)
		if valErr != nil {
			exitWithReport(apperrors.NewGeneralError(
				"manifest validation failed",
				valErr,
			))
		}

		// Optional `--wait`: poll until every in-progress manifest
		// reaches a terminal state (or the deadline elapses). The
		// loop re-runs the registry-backed validate every 10s. The
		// final results / verdict are emitted normally below.
		if validateWait > 0 {
			results = waitForReconcile(ctx, ctrlclient.Client(k8sClient), manifestDir, results, validateWait)
		}

		verdict := emitValidationReport(versionCheck, results, presetDeviations, outputFormat)
		emitComponentVersionReport(componentCheck, outputFormat)
		emitPresetMatchReport(presetResults, outputFormat)
		warnings = append(warnings, collectVerdictWarnings(verdict)...)
		if componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch {
			warnings = append(warnings, "Component versions in NicClusterPolicy / NicNodePolicy diverge from the selectedRelease catalog — see the report's components section.")
		}

		// emitReport writes the HTML file synchronously. Called on
		// every remaining exit path (success, in-progress no-op,
		// connectivity failure) since Go's defer doesn't run on
		// os.Exit and exitWithError does os.Exit.
		emitReport := func() {
			overall := computeOverallVerdict(verdict, componentCheck, helmValuesCheck, strayCheck, matrix, presetResults)
			emitVerdictBanner(overall, outputFormat)
			writeHTMLReportIfWanted(ctx, k8sClient, restConfig, manifestDir, deploymentFiles,
				operatorNamespace, versionCheck, componentCheck, helmValuesCheck, strayCheck, results, &matrix, &warnings,
				presetResults, overall, userConfigPath(), outputFormat)
		}

		// `--connectivity` runs the data-plane matrix when no manifest
		// is broken (error or missing). Version mismatches (Helm
		// release or per-component) are *not* fatal here — the cluster
		// is still up, so the connectivity tests are still meaningful;
		// the final verdict picks up the mismatch as a fail reason.
		// In-progress (without errors) prints a warning and exits 0
		// so CI/operators can re-run later.
		componentMismatch := componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch
		helmValuesMismatch := helmValuesCheck != nil && !helmValuesCheck.Skipped && !helmValuesCheck.AllMatch
		strayMismatch := strayCheck != nil && strayCheck.Failed()
		switch {
		case verdict.HasError || verdict.HasMissing:
			emitReport()
			os.Exit(apperrors.ExitDeployment)
		case verdict.HasInProgress:
			if outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "\nNote: some manifests are still reconciling. Re-run later or use --wait to block.")
			}
			if validationConnectivityEnabled(validationCfg) && outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "Connectivity matrix skipped — cluster has in-progress manifests.")
			}
			warnings = append(warnings, "Connectivity matrix skipped — cluster has in-progress manifests.")
			emitReport()
			if !verdict.VersionOK || componentMismatch || helmValuesMismatch || strayMismatch {
				os.Exit(apperrors.ExitDeployment)
			}
			return
		}

		if validationConnectivityEnabled(validationCfg) {
			uiOutput, _ := ui.NewOutputForFormat(outputFormat, yesFlag)
			ctxWithUI := ui.WithOutput(ctx, uiOutput)
			m, err := connectivity.RunMatrix(ctxWithUI, k8sClient, restConfig, uiOutput, connectivity.Options{
				ManifestDir:             manifestDir,
				Timeout:                 validateConnectivityTimeout,
				Keep:                    validateKeep,
				Mode:                    connectivity.Mode(validationCfg.Mode),
				Routing:                 profileRouting,
				Checks:                  connectivityChecksFromConfig(validationCfg),
				RPingIterations:         validationCfg.RDMA.RPingIterations,
				IBWriteSize:             validationCfg.RDMA.IBWriteSize,
				IBWriteMinBandwidthGbps: *validationCfg.RDMA.IBWriteMinBandwidthGbps,
			})
			matrix = m
			if err != nil {
				exitWithReport(apperrors.NewClusterError(
					"connectivity matrix failed",
					err,
					"See log output for the failing step; re-run with --keep to inspect the test DaemonSet",
				))
			}
			if outputFormat == "json" {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"connectivity": matrix,
				})
			}
			if matrix != nil && matrix.Skipped != nil {
				warnings = append(warnings, "Connectivity matrix skipped: "+matrix.Skipped.Reason)
			}
		}

		emitReport()
		// Final exit code unifies every gating signal: matrix failures,
		// version/component mismatches, AND preset deviations all fail
		// the verdict, but the matrix still gets a chance to run when
		// the only problem is a stale Helm release / catalog mismatch
		// / hardware drift from the catalog preset.
		matrixFailed := matrix != nil && matrix.Summary.Failed > 0
		if matrixFailed || !verdict.VersionOK || componentMismatch || helmValuesMismatch || strayMismatch || hasPresetDeviation(presetResults) {
			os.Exit(apperrors.ExitDeployment)
		}
	},
}

func applyValidateFlagOverrides(cmd *cobra.Command, validationCfg *config.ValidationConfig) error {
	if validationCfg == nil {
		return fmt.Errorf("validation config must not be nil")
	}
	if cmd.Flags().Changed("connectivity") {
		v := validateConnectivity
		validationCfg.Connectivity = &v
	}
	if cmd.Flags().Changed("validation-mode") {
		validationCfg.Mode = strings.TrimSpace(validateMode)
	}
	if cmd.Flags().Changed("validation-checks") {
		validationCfg.Checks = config.NormalizeValidationChecks(validateChecks)
	}
	if validationCfg.RDMA == nil {
		validationCfg.RDMA = &config.ValidationRDMAConfig{}
	}
	if cmd.Flags().Changed("rdma-rping-iterations") {
		if validateRDMAIterations <= 0 {
			return fmt.Errorf("--rdma-rping-iterations must be greater than 0")
		}
		validationCfg.RDMA.RPingIterations = validateRDMAIterations
	}
	if cmd.Flags().Changed("rdma-ib-write-size") {
		if validateRDMAIBWriteSize <= 0 {
			return fmt.Errorf("--rdma-ib-write-size must be greater than 0")
		}
		validationCfg.RDMA.IBWriteSize = validateRDMAIBWriteSize
	}
	if cmd.Flags().Changed("rdma-ib-write-min-bandwidth-gbps") {
		if validateRDMAIBWriteMinGbps < 0 {
			return fmt.Errorf("--rdma-ib-write-min-bandwidth-gbps must be greater than or equal to 0")
		}
		validationCfg.RDMA.IBWriteMinBandwidthGbps = &validateRDMAIBWriteMinGbps
	}
	validationCfg = config.NormalizeValidationConfig(validationCfg)
	return config.ValidateValidationConfig(validationCfg)
}

func validationConnectivityEnabled(validationCfg *config.ValidationConfig) bool {
	if validationCfg == nil || validationCfg.Connectivity == nil {
		return true
	}
	return *validationCfg.Connectivity
}

func connectivityChecksFromConfig(validationCfg *config.ValidationConfig) []connectivity.Check {
	if validationCfg == nil {
		return nil
	}
	checks := make([]connectivity.Check, 0, len(validationCfg.Checks))
	for _, check := range validationCfg.Checks {
		switch check {
		case config.ValidationCheckICMP:
			checks = append(checks, connectivity.CheckICMP)
		case config.ValidationCheckRPing:
			checks = append(checks, connectivity.CheckRPing)
		case config.ValidationCheckIBWriteBW:
			checks = append(checks, connectivity.CheckIBWriteBW)
		}
	}
	return checks
}

// hasPresetDeviation reports whether any group deviated from its
// matched preset. Drives the non-zero exit code in addition to the
// PASS/FAIL banner.
func hasPresetDeviation(results []presetmatch.Result) bool {
	for _, r := range results {
		if r.Status == presetmatch.StatusDeviation {
			return true
		}
	}
	return false
}

// computeOverallVerdict folds every input from the validate run into
// a single pass/fail outcome — the same logic used to decide the
// process exit code, expressed as structured Reasons / Notes so the
// banner can list what failed (and what was merely informational).
func computeOverallVerdict(
	verdict validationVerdict,
	componentCheck *networkoperatorplugin.ComponentVersionCheck,
	helmValuesCheck *networkoperatorplugin.HelmValuesCheck,
	strayCheck *preflight.Result,
	matrix *connectivity.MatrixResult,
	presetResults []presetmatch.Result,
) connectivity.OverallVerdict {
	out := connectivity.OverallVerdict{Pass: true}
	if !verdict.VersionOK {
		out.Pass = false
		out.Reasons = append(out.Reasons, "Network Operator Helm release version does not match the selectedRelease in cluster-config.yaml")
	}
	if verdict.HasMissing {
		out.Pass = false
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d manifest(s) not found in the cluster — `l8k deploy` not run or partial", verdict.MissingCount))
	}
	if verdict.HasError {
		out.Pass = false
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d manifest(s) reported an error state", verdict.ErrorCount))
	}
	if componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch {
		mismatches := 0
		for _, c := range componentCheck.Components {
			if !c.Match {
				mismatches++
			}
		}
		out.Pass = false
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d component version(s) in NicClusterPolicy/NicNodePolicy diverge from the selectedRelease catalog", mismatches))
	}
	if helmValuesCheck != nil && !helmValuesCheck.Skipped && !helmValuesCheck.AllMatch {
		out.Pass = false
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("%d helm value(s) on the deployed release diverge from the generated values.yaml — re-run `l8k deploy --overwrite-existing` to converge", len(helmValuesCheck.Diff)))
	}
	if strayCheck != nil && strayCheck.Failed() {
		out.Pass = false
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("%d existing Network Operator resource(s) in the operator namespace conflict with the rendered manifests — re-run `l8k deploy --overwrite-existing` to delete them", len(strayCheck.Mismatches)))
	}
	if matrix != nil {
		if matrix.Summary.Failed > 0 {
			out.Pass = false
			out.Reasons = append(out.Reasons, fmt.Sprintf("%d connectivity test(s) failed in the connectivity matrix", matrix.Summary.Failed))
		}
		if matrix.Skipped != nil {
			out.Notes = append(out.Notes, "Connectivity matrix skipped: "+matrix.Skipped.Reason)
		}
	}
	if verdict.HasInProgress {
		out.Notes = append(out.Notes, fmt.Sprintf("%d manifest(s) still reconciling — re-run later or use --wait to block (does not gate the verdict)", verdict.InProgressCount))
	}
	// Platform topology mismatches fail the verdict but do NOT block
	// the other stages (same pattern as version mismatch): when the
	// discovered hardware drifts from the certified topology for the
	// server type, it's a real validation failure, but the data
	// plane is still testable. The HTML report's Node groups section
	// shows the in-place diff per PF row — the banner just names the
	// affected server type so operators know where to look.
	for _, r := range presetResults {
		if r.Status != presetmatch.StatusDeviation {
			continue
		}
		out.Pass = false
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("The detected platform topology does not match the certified topology for %s server type (see Node groups section for the per-device diff)",
				platformLabel(r)))
	}
	return out
}

// platformLabel renders the server-type identifier used in user
// messages as `<manufacturer>-<machineType>-<gpuType>`, dropping any
// empty segments. The manufacturer is the leading segment when the
// matched preset's topology.yaml declares it (e.g. "Dell-PowerEdge-
// XE9680-H200"); on a not-found match it's omitted because we have
// no manufacturer signal outside the catalog. Falls back to the
// group identifier if nothing useful is set.
func platformLabel(r presetmatch.Result) string {
	parts := make([]string, 0, 3)
	if r.Manufacturer != "" {
		parts = append(parts, r.Manufacturer)
	}
	if r.MachineType != "" {
		parts = append(parts, r.MachineType)
	}
	if r.GPUType != "" {
		parts = append(parts, r.GPUType)
	}
	if len(parts) == 0 {
		return r.Group
	}
	return strings.Join(parts, "-")
}

// emitVerdictBanner prints the PASS/FAIL banner at the top of text
// output (above the per-check details that follow). JSON mode skips
// this — the structured Verdict field travels with the report data.
func emitVerdictBanner(v connectivity.OverallVerdict, format string) {
	if format == "json" {
		return
	}
	if v.Pass {
		fmt.Println("\n══════════════════════════════════════════════════════════")
		fmt.Println("                  ✓ VALIDATION PASSED")
		fmt.Println("══════════════════════════════════════════════════════════")
	} else {
		fmt.Println("\n══════════════════════════════════════════════════════════")
		fmt.Println("                  ✗ VALIDATION FAILED")
		fmt.Println("══════════════════════════════════════════════════════════")
		for _, r := range v.Reasons {
			fmt.Printf("  • %s\n", r)
		}
	}
	for _, n := range v.Notes {
		fmt.Printf("  ⓘ %s\n", n)
	}
}

// emitPresetMatchReport prints the per-group platform-topology check
// in text mode. JSON mode skips this — the structured field rides
// downstream on the HTML/JSON envelope.
func emitPresetMatchReport(results []presetmatch.Result, format string) {
	if len(results) == 0 || format == "json" {
		return
	}
	fmt.Println()
	fmt.Println("Platform topology validation")
	for _, r := range results {
		var status string
		switch r.Status {
		case presetmatch.StatusMatch:
			status = "MATCH        "
		case presetmatch.StatusDeviation:
			status = "MISMATCH     "
		case presetmatch.StatusNotFound:
			status = "NOT CERTIFIED"
		case presetmatch.StatusSkipped:
			status = "SKIPPED      "
		default:
			status = "UNKNOWN      "
		}
		label := r.Group
		if r.MachineType != "" || r.GPUType != "" {
			label = fmt.Sprintf("%s (%s/%s)", r.Group, r.MachineType, r.GPUType)
		}
		detail := r.Reason
		switch r.Status {
		case presetmatch.StatusMatch:
			detail = fmt.Sprintf("matches certified topology for %s server type", platformLabel(r))
		case presetmatch.StatusDeviation:
			detail = fmt.Sprintf("does not match certified topology for %s server type · %s",
				platformLabel(r), r.Reason)
		case presetmatch.StatusNotFound:
			detail = fmt.Sprintf("no certified topology available for %s server type", platformLabel(r))
		}
		fmt.Printf("  [%s] %s — %s\n", status, label, detail)
	}
}

// emitComponentVersionReport prints the component-version cross-check
// in text mode (the JSON consumer reads the structured field added by
// the HTML/JSON writers downstream). Skipped checks are surfaced as a
// short note so operators understand why the section is absent.
func emitComponentVersionReport(cv *networkoperatorplugin.ComponentVersionCheck, format string) {
	if cv == nil || format == "json" {
		return
	}
	fmt.Println()
	fmt.Println("Component versions (NicClusterPolicy / NicNodePolicy vs. catalog)")
	if cv.Skipped {
		reason := cv.Reason
		if reason == "" {
			reason = "skipped"
		}
		fmt.Printf("  status: SKIPPED (%s)\n", reason)
		return
	}
	if len(cv.Components) == 0 {
		fmt.Println("  (no version-bearing sections found in cluster)")
		return
	}
	matched := 0
	for _, c := range cv.Components {
		status := "MISMATCH"
		if c.Match {
			status = "MATCH   "
			matched++
		}
		expected := c.Expected
		if expected == "" {
			expected = "(none)"
		}
		fmt.Printf("  [%s] %s — %s: expected=%s got=%s\n",
			status, c.Source, c.Section, expected, c.Actual)
	}
	verdict := "MATCH"
	if !cv.AllMatch {
		verdict = "MISMATCH"
	}
	fmt.Printf("  result: %s (%d/%d match)\n", verdict, matched, len(cv.Components))
}

// collectVerdictWarnings turns aggregate verdict counts into
// human-readable lines for the report's Warnings rollup. The validate
// CLI surfaces the same notes interactively; rendering them in the
// report too keeps the file self-contained.
func collectVerdictWarnings(v validationVerdict) []string {
	var w []string
	if v.HasInProgress {
		w = append(w, fmt.Sprintf("%d manifest(s) still reconciling — re-run later or use --wait to block.", v.InProgressCount))
	}
	if v.HasMissing {
		w = append(w, fmt.Sprintf("%d manifest(s) not found in the cluster — run `l8k deploy` first.", v.MissingCount))
	}
	if v.HasError {
		w = append(w, fmt.Sprintf("%d manifest(s) reported an error state.", v.ErrorCount))
	}
	if !v.VersionOK {
		w = append(w, "Network Operator Helm release appVersion does not match the selectedRelease in cluster-config.yaml.")
	}
	return w
}

// resolveReportPath maps --report-path into the file we'll write to.
// Empty value → auto-place under <deployment-files>; literal "-"
// disables; anything else is used verbatim.
func resolveReportPath(flag, deploymentDir string) string {
	switch flag {
	case "-":
		return ""
	case "":
		return filepath.Join(deploymentDir, "k8s-launch-kit-validation-report.html")
	default:
		return flag
	}
}

// writeHTMLReportIfWanted is the deferred report writer. It builds a
// ReportData snapshot from everything validate has seen and writes
// the HTML to disk. Errors are logged but do not change the exit
// code — the report is a "best effort" debugging aid, not a gate.
func writeHTMLReportIfWanted(
	ctx context.Context,
	c ctrlclient.Client,
	restConfig *rest.Config,
	manifestDir string,
	deploymentDir string,
	operatorNamespace string,
	versionCheck *networkoperatorplugin.VersionCheck,
	componentCheck *networkoperatorplugin.ComponentVersionCheck,
	helmValuesCheck *networkoperatorplugin.HelmValuesCheck,
	strayCheck *preflight.Result,
	results []networkoperatorplugin.ValidationResult,
	matrix **connectivity.MatrixResult,
	warnings *[]string,
	presetResults []presetmatch.Result,
	overall connectivity.OverallVerdict,
	userCfgPath string,
	outputFormat string,
) {
	path := resolveReportPath(validateReportPath, deploymentDir)
	if path == "" {
		return
	}

	data := connectivity.ReportData{
		Verdict: overall,
		Cluster: connectivity.ClusterInfo{
			L8kVersion:        Version,
			GeneratedAt:       time.Now().UTC(),
			KubeContext:       readKubeContext(),
			APIServerVersion:  probeAPIServerVersion(restConfig),
			OperatorNamespace: operatorNamespace,
		},
		Profile:        loadProfileInfo(userCfgPath, manifestDir),
		NodeGroups:     loadNodeGroups(userCfgPath, presetResults),
		Nodes:          listNodesForReport(ctx, c),
		Release:        versionCheck,
		ComponentCheck: componentCheck,
		HelmValues:     helmValuesCheck,
		StrayCRs:       strayCheck,
		PresetMatches:  presetResults,
		Manifests:      results,
		Matrix:         *matrix,
		Warnings:       *warnings,
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Log.Error(err, "Failed to create report directory", "path", path)
		return
	}
	f, err := os.Create(path)
	if err != nil {
		log.Log.Error(err, "Failed to create report file", "path", path)
		return
	}
	defer f.Close()
	if err := connectivity.RenderHTML(f, data); err != nil {
		log.Log.Error(err, "Failed to render report", "path", path)
		return
	}
	abs, _ := filepath.Abs(path)
	if outputFormat == "json" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"reportPath": abs})
	} else {
		fmt.Fprintf(os.Stderr, "\nHTML report written to %s\n", abs)
	}
}

// readKubeContext returns the current-context from the resolved
// kubeconfig, or "" when the lookup fails — purely cosmetic for the
// report header.
func readKubeContext() string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	cfg, err := loadingRules.Load()
	if err != nil {
		return ""
	}
	return cfg.CurrentContext
}

// probeAPIServerVersion is a one-shot best-effort lookup of the
// apiserver's git version, rendered as e.g. "v1.35.0" in the report
// header. Failure → empty string; the renderer renders "—".
func probeAPIServerVersion(restConfig *rest.Config) string {
	if restConfig == nil {
		return ""
	}
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return ""
	}
	info, err := cs.Discovery().ServerVersion()
	if err != nil {
		return ""
	}
	return info.GitVersion
}

// loadProfileInfo reads the profile section out of cluster-config.yaml
// (when present) and projects it into the report's ProfileInfo. When
// the config doesn't carry an explicit `profile:` block (which is
// normal — `l8k discover` writes only `clusterConfig:` and
// `networkOperator:`), this falls back to inferring the profile from
// the Kinds present in the rendered deployment manifests.
func loadProfileInfo(userCfgPath, manifestDir string) connectivity.ProfileInfo {
	if userCfgPath != "" {
		if cfg, err := config.LoadFullConfig(userCfgPath, log.Log); err == nil && cfg != nil && cfg.Profile != nil {
			info := connectivity.ProfileInfo{
				Fabric:         cfg.Profile.Fabric,
				DeploymentType: cfg.Profile.Deployment,
				Multirail:      cfg.Profile.Multirail,
			}
			if cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable {
				info.SpectrumX = &connectivity.ProfileSpectrumX{
					Version:        cfg.Profile.SpectrumX.SPCXVersion,
					MultiplaneMode: cfg.Profile.SpectrumX.MultiplaneMode,
					NumberOfPlanes: cfg.Profile.SpectrumX.NumberOfPlanes,
				}
			}
			return info
		}
	}
	return inferProfileFromManifests(manifestDir)
}

// inferProfileFromManifests walks the deployment-files directory and
// reads the Kinds (and a few spec fields) out of every YAML to deduce
// the profile that produced them.
//
// Detection rules — first match wins, so order matters:
//
//   - SpectrumXRailPoolConfig present     → spectrum-x  / ethernet / sriov / multirail
//   - HostDeviceNetwork present           → host_device / linkType from CR (or "infiniband" if no marker)
//   - IPoIBNetwork present                → rdma_shared / infiniband
//   - MacvlanNetwork present              → rdma_shared / ethernet
//   - SriovNetworkNodePolicy + IB linkType→ sriov       / infiniband
//   - SriovNetworkNodePolicy (no IB)      → sriov       / ethernet
//   - else                                → empty ProfileInfo
//
// Multirail is set true when more than one SriovNetwork / Network CR is
// present (one per rail) — single-rail deployments yield exactly one.
func inferProfileFromManifests(manifestDir string) connectivity.ProfileInfo {
	if manifestDir == "" {
		return connectivity.ProfileInfo{}
	}
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return connectivity.ProfileInfo{}
	}
	kinds := map[string]int{}
	railNetworks := 0
	sawIBLinkType := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		// Skip example manifests — they're not part of the
		// network-operator surface we're trying to identify.
		if networkoperatorplugin.IsExampleManifest(e.Name()) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		for _, doc := range splitYAMLDocs(string(content)) {
			meta := sniffManifest(doc)
			if meta.Kind == "" {
				continue
			}
			kinds[meta.Kind]++
			if meta.Kind == "SriovNetwork" || meta.Kind == "SriovIBNetwork" ||
				meta.Kind == "HostDeviceNetwork" || meta.Kind == "IPoIBNetwork" ||
				meta.Kind == "MacvlanNetwork" {
				railNetworks++
			}
			if meta.LinkType == "IB" || meta.LinkType == "Infiniband" {
				sawIBLinkType = true
			}
		}
	}

	info := connectivity.ProfileInfo{Multirail: railNetworks > 1}
	switch {
	case kinds["SpectrumXRailPoolConfig"] > 0:
		info.Fabric = "ethernet"
		info.DeploymentType = "sriov"
		info.Multirail = true
		info.SpectrumX = &connectivity.ProfileSpectrumX{Version: "RA2.2"} // best-effort
	case kinds["HostDeviceNetwork"] > 0:
		info.DeploymentType = "host_device"
		if sawIBLinkType {
			info.Fabric = "infiniband"
		} else {
			info.Fabric = "ethernet"
		}
	case kinds["IPoIBNetwork"] > 0:
		info.DeploymentType = "rdma_shared"
		info.Fabric = "infiniband"
	case kinds["MacvlanNetwork"] > 0:
		info.DeploymentType = "rdma_shared"
		info.Fabric = "ethernet"
	case kinds["SriovNetworkNodePolicy"] > 0 || kinds["SriovNetwork"] > 0 || kinds["SriovIBNetwork"] > 0:
		info.DeploymentType = "sriov"
		if sawIBLinkType || kinds["SriovIBNetwork"] > 0 {
			info.Fabric = "infiniband"
		} else {
			info.Fabric = "ethernet"
		}
	}
	return info
}

// sniffManifest is a tiny YAML reader that extracts the few fields
// inferProfileFromManifests needs without round-tripping through
// Unstructured. linkType lives under spec.linkType on SR-IOV node
// policies + most Network CRs.
type manifestSniff struct {
	Kind     string
	LinkType string
}

func sniffManifest(doc string) manifestSniff {
	type metaOnly struct {
		Kind string `yaml:"kind"`
		Spec struct {
			LinkType string `yaml:"linkType"`
		} `yaml:"spec"`
	}
	var m metaOnly
	if err := sigsYaml.Unmarshal([]byte(doc), &m); err != nil {
		return manifestSniff{}
	}
	return manifestSniff{Kind: m.Kind, LinkType: m.Spec.LinkType}
}

// splitYAMLDocs is a local copy of the splitter — the parent
// networkoperatorplugin package keeps its own private one; copying
// the four lines here avoids exposing it.
func splitYAMLDocs(s string) []string {
	var docs []string
	var cur []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "---") {
			if len(cur) > 0 {
				docs = append(docs, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		docs = append(docs, strings.Join(cur, "\n"))
	}
	return docs
}

// loadNodeGroups projects every cluster-config.yaml `clusterConfig[]`
// entry into a NodeGroupInfo for the report. Empty result when the
// config wasn't found / parsed — the section just renders empty.
//
// presetResults carries the runtime-fresh per-group preset match
// (computed by presetmatch.MatchAll at validate-time). Its
// Deviations are preferred over the snapshot stored in
// `cluster-config.yaml.clusterConfig[].presetDeviation`, which only
// gets populated by `l8k discover` and goes stale as soon as the
// catalog changes or the hardware is swapped. Falling back to the
// stored snapshot when the runtime entry is absent keeps the older
// flow working (e.g. validate run without l8k discover access to
// the cluster).
func loadNodeGroups(userCfgPath string, presetResults []presetmatch.Result) []connectivity.NodeGroupInfo {
	if userCfgPath == "" {
		return nil
	}
	cfg, err := config.LoadFullConfig(userCfgPath, log.Log)
	if err != nil || cfg == nil {
		return nil
	}
	// Build a per-group index of fresh match results so the
	// PF-table markers reflect what the banner is currently
	// complaining about. The full Result is indexed (not just
	// Deviations) because the marker pass also needs Preset.PFs
	// to enrich "missing PCI" rows with the expected
	// deviceID / rail / netdev that the certified topology
	// declares for those addresses.
	freshResultsByGroup := map[string]*presetmatch.Result{}
	for i := range presetResults {
		r := &presetResults[i]
		if r.Status == presetmatch.StatusDeviation {
			freshResultsByGroup[r.Group] = r
		}
	}
	out := make([]connectivity.NodeGroupInfo, 0, len(cfg.ClusterConfig))
	for _, g := range cfg.ClusterConfig {
		ng := connectivity.NodeGroupInfo{
			Identifier:    g.Identifier,
			MachineType:   g.MachineType,
			GPUType:       g.GPUType,
			LinkType:      g.LinkType,
			NodeSelector:  g.NodeSelector,
			WorkerNodes:   g.WorkerNodes,
			PresetApplied: g.PresetApplied,
		}
		if g.Capabilities != nil && g.Capabilities.Nodes != nil {
			ng.SriovCapable = g.Capabilities.Nodes.Sriov
			ng.RdmaCapable = g.Capabilities.Nodes.Rdma
			ng.IbCapable = g.Capabilities.Nodes.Ib
		}
		// Pick the runtime-fresh result when available, falling back
		// to whatever `l8k discover` stamped into cluster-config.
		// The runtime path also carries the matched Preset (used to
		// enrich missing-PCI rows below); the fallback only has
		// stored deviations so missing rows there can't be enriched.
		freshResult := freshResultsByGroup[g.Identifier]
		var deviations []config.PresetDeviationEntry
		if freshResult != nil {
			deviations = freshResult.Deviations
		} else {
			deviations = g.PresetDeviation
		}
		for _, d := range deviations {
			ng.PresetDeviations = append(ng.PresetDeviations, connectivity.PresetDeviation{
				Field: d.Field, Expected: d.Expected, Got: d.Got, Detail: d.Detail,
			})
		}
		for _, pf := range g.PFs {
			row := connectivity.PFInfo{
				PciAddress:       pf.PciAddress,
				DeviceID:         pf.DeviceID,
				Rail:             "—",
				Traffic:          pf.Traffic,
				NetworkInterface: pf.NetworkInterface,
				RdmaDevice:       pf.RdmaDevice,
				PSID:             pf.PSID,
				PartNumber:       pf.PartNumber,
				ConnectedGPU:     pf.ConnectedGPU,
				GPUProximity:     pf.GPUProximity,
			}
			if pf.Rail != nil {
				row.Rail = fmt.Sprintf("%d", *pf.Rail)
			}
			if pf.NumaNode != nil {
				row.NumaNode = fmt.Sprintf("%d", *pf.NumaNode)
			}
			switch pf.Traffic {
			case "east-west":
				ng.EastWestPFs = append(ng.EastWestPFs, row)
			case "north-south":
				ng.NorthSouthPFs = append(ng.NorthSouthPFs, row)
			default:
				// Unspecified traffic — treat as east-west so it
				// still shows up.
				ng.EastWestPFs = append(ng.EastWestPFs, row)
			}
		}
		applyPlatformTopologyMarkers(&ng, deviations, freshResult)
		out = append(out, ng)
	}
	return out
}

// applyPlatformTopologyMarkers populates the report's two paired
// PF views (Actual = discovered hardware, Expected = certified
// topology) and flags mismatched rows in each.
//
//   - Actual row is Mismatched when its (PCI, deviceID) pair has no
//     matching entry in the preset.
//   - Expected row is Mismatched when its (PCI, deviceID) pair has
//     no matching entry on the cluster.
//   - PFCountMismatch is set from the pfCount deviation.
//
// matchResult may be nil when we're falling back to stored
// cluster-config deviations (no preset object available); in that
// case only Mismatched markers based on raw deviations are applied
// and the Expected tables stay empty.
func applyPlatformTopologyMarkers(ng *connectivity.NodeGroupInfo, deviations []config.PresetDeviationEntry, matchResult *presetmatch.Result) {
	for _, d := range deviations {
		if d.Field == "pfCount" {
			var got, want int
			_, _ = fmt.Sscanf(d.Got, "%d", &got)
			_, _ = fmt.Sscanf(d.Expected, "%d", &want)
			ng.PFCountMismatch = &connectivity.PFCountMismatch{Expected: want, Got: got}
		}
	}

	if matchResult == nil || matchResult.Preset == nil {
		// No live preset to compare against — fall back to flagging
		// Actual rows whose PCI appears in any pciAddress/deviceID
		// deviation from the stored snapshot. Expected tables stay
		// empty.
		flagged := map[string]bool{}
		for _, d := range deviations {
			switch d.Field {
			case "deviceID":
				if _, pci := splitDeviceIDAtPCI(d.Got); pci != "" {
					flagged[pci] = true
				}
			case "pciAddress":
				if d.Got != "" {
					flagged[d.Got] = true
				}
			}
		}
		markActualMismatched(ng.EastWestPFs, flagged)
		markActualMismatched(ng.NorthSouthPFs, flagged)
		return
	}

	// Index actuals by PCI so the Expected pass can quickly check
	// whether a preset PF has a matching cluster PF (same PCI +
	// same deviceID).
	type actualEntry struct{ deviceID string }
	actualByPCI := map[string]actualEntry{}
	for _, pf := range ng.EastWestPFs {
		actualByPCI[pf.PciAddress] = actualEntry{deviceID: pf.DeviceID}
	}
	for _, pf := range ng.NorthSouthPFs {
		actualByPCI[pf.PciAddress] = actualEntry{deviceID: pf.DeviceID}
	}
	// Index preset by PCI for the Actual pass.
	presetByPCI := map[string]presets.PresetPF{}
	for _, pf := range matchResult.Preset.PFs {
		presetByPCI[pf.PciAddress] = pf
	}

	// Actual: flag rows whose PCI isn't in the preset, or whose
	// deviceID at that PCI doesn't match the preset.
	for i := range ng.EastWestPFs {
		pf := &ng.EastWestPFs[i]
		expected, ok := presetByPCI[pf.PciAddress]
		if !ok || expected.DeviceID != pf.DeviceID {
			pf.Mismatched = true
		}
	}
	for i := range ng.NorthSouthPFs {
		pf := &ng.NorthSouthPFs[i]
		expected, ok := presetByPCI[pf.PciAddress]
		if !ok || expected.DeviceID != pf.DeviceID {
			pf.Mismatched = true
		}
	}

	// Expected: project the preset's PFs into the same PFInfo
	// shape, bucketed by traffic type. Flag rows whose (PCI,
	// deviceID) pair isn't present on the cluster.
	for _, pf := range matchResult.Preset.PFs {
		row := connectivity.PFInfo{
			PciAddress:       pf.PciAddress,
			DeviceID:         pf.DeviceID,
			Rail:             "—",
			Traffic:          pf.Traffic,
			NetworkInterface: pf.NetworkInterface,
			RdmaDevice:       pf.RdmaDevice,
			PSID:             pf.PSID,
			PartNumber:       pf.PartNumber,
			ConnectedGPU:     pf.ConnectedGPU,
			GPUProximity:     pf.GPUProximity,
		}
		if pf.Rail != nil {
			row.Rail = fmt.Sprintf("%d", *pf.Rail)
		}
		if pf.NumaNode != nil {
			row.NumaNode = fmt.Sprintf("%d", *pf.NumaNode)
		}
		if actual, ok := actualByPCI[pf.PciAddress]; !ok || actual.deviceID != pf.DeviceID {
			row.Mismatched = true
		}
		switch pf.Traffic {
		case "north-south":
			ng.ExpectedNorthSouthPFs = append(ng.ExpectedNorthSouthPFs, row)
		default:
			ng.ExpectedEastWestPFs = append(ng.ExpectedEastWestPFs, row)
		}
	}
}

// markActualMismatched flags any Actual PFInfo whose PCI is in the
// flagged set. Used as the fallback path when we don't have a live
// preset object to drive the full Actual/Expected pairing.
func markActualMismatched(pfs []connectivity.PFInfo, flagged map[string]bool) {
	for i := range pfs {
		if flagged[pfs[i].PciAddress] {
			pfs[i].Mismatched = true
		}
	}
}

// splitDeviceIDAtPCI splits a "<deviceID>@<pciAddr>" composite (the
// format ValidatePreset emits for deviceID deviations) back into its
// two parts. Returns ("", "") on malformed input.
func splitDeviceIDAtPCI(s string) (deviceID, pci string) {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return "", ""
	}
	return s[:at], s[at+1:]
}

// listNodesForReport pulls the cluster's node list and projects each
// into a NodeInfo for the report. Reads l8k's machine/gpu labels
// (config.MachineLabelKey / config.GPULabelKey) plus a best-effort
// role inferred from the node-role.kubernetes.io/* labels. Failures
// are logged and the report just gets an empty Nodes section.
func listNodesForReport(ctx context.Context, c ctrlclient.Client) []connectivity.NodeInfo {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		log.Log.V(1).Info("listNodesForReport failed", "error", err.Error())
		return nil
	}
	out := make([]connectivity.NodeInfo, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		out = append(out, connectivity.NodeInfo{
			Name:         n.Name,
			MachineLabel: n.Labels[config.MachineLabelKey],
			GpuLabel:     n.Labels[config.GPULabelKey],
			Role:         nodeRoles(n.Labels),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeRoles collapses the node-role.kubernetes.io/* labels into a
// comma-joined string ("control-plane,worker"). Empty when no role
// labels are set.
func nodeRoles(labels map[string]string) string {
	const prefix = "node-role.kubernetes.io/"
	var roles []string
	for k := range labels {
		if strings.HasPrefix(k, prefix) {
			roles = append(roles, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
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
		fmt.Println("Platform topology mismatches (detected hardware differs from the certified topology)")
		for _, gd := range presetDeviations {
			label := gd.Group
			if gd.MachineType != "" || gd.GPUType != "" {
				label = fmt.Sprintf("%s (%s/%s)", gd.Group, gd.MachineType, gd.GPUType)
			}
			fmt.Printf("  %s — %d mismatch(es):\n", label, len(gd.Deviations))
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
	fmt.Printf("Summary: %d/%d ready, %d in-progress, %d error, %d missing; version: %s; topology mismatches: %d group(s)\n",
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
	// --network-operator-namespace overrides cfg.NetworkOperator.Namespace at
	// load time (via loadUserConfig). Used by the Helm-release version check
	// and the manifest state validators to target the right namespace.
	validateCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override the network operator namespace from cluster-config.yaml")

	// Phase 2 flags. `--connectivity` defaults to true — every
	// `l8k validate` exercises the data plane unless explicitly
	// disabled. Pass `--connectivity=false` to limit validate to
	// the static manifest-presence + Helm release-version checks.
	validateCmd.Flags().BoolVar(&validateConnectivity, "connectivity", true, "Run a source-bound connectivity matrix (icmp + rping + ib_write_bw) between pods of the example DaemonSet. Default true. Pass --connectivity=false to skip when only the static manifest checks are wanted.")
	validateCmd.Flags().BoolVar(&validateKeep, "keep", false, "Leave the example DaemonSet running after --connectivity completes (useful for debugging).")
	validateCmd.Flags().DurationVar(&validateConnectivityTimeout, "connectivity-timeout", 5*time.Minute, "Wall-clock budget for the connectivity matrix (DaemonSet rollout + icmp + rping + ib_write_bw execs).")
	validateCmd.Flags().StringVar(&validateMode, "validation-mode", "", "Connectivity validation mode: quick, full, or strict. Overrides validation.mode from cluster-config.yaml.")
	validateCmd.Flags().StringSliceVar(&validateChecks, "validation-checks", nil, "Comma-separated checks to run during connectivity validation. Supported: icmp, rping, ib_write_bw. Overrides validation.checks from cluster-config.yaml.")
	validateCmd.Flags().IntVar(&validateRDMAIterations, "rdma-rping-iterations", 0, "Number of rping client iterations. Overrides validation.rdma.rpingIterations from cluster-config.yaml.")
	validateCmd.Flags().IntVar(&validateRDMAIBWriteSize, "rdma-ib-write-size", 0, "Message size for ib_write_bw -s. Overrides validation.rdma.ibWriteSize from cluster-config.yaml.")
	validateCmd.Flags().Float64Var(&validateRDMAIBWriteMinGbps, "rdma-ib-write-min-bandwidth-gbps", 0, "Minimum observed ib_write_bw peak bandwidth in Gbps required for a test to pass. Use 0 to disable bandwidth gating. Overrides validation.rdma.ibWriteMinBandwidthGbps from cluster-config.yaml.")
	validateCmd.Flags().DurationVar(&validateWait, "wait", 0, "Block validate up to this duration waiting for in-progress manifests to reach a terminal state. 0 (default) returns immediately on the first snapshot.")
	validateCmd.Flags().StringVar(&validateReportPath, "report-path", "", "Write the HTML validation report to this path. When empty (default), writes to <deployment-files>/k8s-launch-kit-validation-report.html. Pass '-' to skip the report file entirely.")

	setFlagGroup(validateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(validateCmd, "user-config", GroupCommon)
	setFlagGroup(validateCmd, "deployment-files", GroupGeneration)
	setFlagGroup(validateCmd, "network-operator-namespace", GroupCommon)
	setFlagGroup(validateCmd, "connectivity", GroupValidation)
	setFlagGroup(validateCmd, "connectivity-timeout", GroupValidation)
	setFlagGroup(validateCmd, "keep", GroupValidation)
	setFlagGroup(validateCmd, "validation-mode", GroupValidation)
	setFlagGroup(validateCmd, "validation-checks", GroupValidation)
	setFlagGroup(validateCmd, "rdma-rping-iterations", GroupValidation)
	setFlagGroup(validateCmd, "rdma-ib-write-size", GroupValidation)
	setFlagGroup(validateCmd, "rdma-ib-write-min-bandwidth-gbps", GroupValidation)
	setFlagGroup(validateCmd, "wait", GroupValidation)
	setFlagGroup(validateCmd, "report-path", GroupValidation)
}
