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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const defaultOperatorNamespace = "nvidia-network-operator"

const skipNetworkOperatorHelmReason = "Network Operator Helm management disabled by configuration"

type validateRunner struct {
	newKubeClient         func(string) (ctrlclient.Client, *rest.Config, error)
	runConnectivityMatrix func(context.Context, ctrlclient.Client, *rest.Config, ui.Output, connectivity.Options) (*connectivity.MatrixResult, error)
}

// NewValidateRunner returns the production standalone Host validation service.
func NewValidateRunner() ValidateRunner {
	return validateRunner{
		newKubeClient:         kubeclient.New,
		runConnectivityMatrix: connectivity.RunMatrix,
	}
}

func (runner validateRunner) Run(operationContext context.Context, request ValidateRequest) error {
	skipNetworkOperatorHelm := request.SkipNetworkOperatorHelm.Set && request.SkipNetworkOperatorHelm.Value

	// State accumulated during the run. exitWithReport captures it by
	// reference so every error path emits the HTML report with whatever was
	// observed before returning to the outer process boundary.
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
		reportConfig      *config.LaunchKitConfig
		cfgPath           string
		operatorNamespace = defaultOperatorNamespace
	)

	// exitWithReport flushes the HTML report synchronously (best-effort) before
	// returning the structured error, so the operator gets a partial report on
	// failure.
	exitWithReport := func(err *apperrors.StructuredError) error {
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
		emitVerdictBanner(overall, request.OutputFormat)
		writeHTMLReportIfWanted(context.Background(), reportClient, reportRestConfig,
			reportManifestDir, request.DeploymentFiles,
			operatorNamespace, versionCheck, componentCheck, helmValuesCheck, strayCheck, results, &matrix, &warnings,
			presetResults, overall, reportConfig, request.OutputFormat,
			request.ReportPath, request.Version, request.Kubeconfig)
		return err
	}

	presetCatalog, _, catalogErr := PresetCatalogForConfigDir(request.ConfigDir)
	if catalogErr != nil {
		return exitWithReport(apperrors.NewValidationError(
			"invalid --config-dir",
			catalogErr,
			"Provide an accessible directory; when present, l8k-config.yaml must be a file and presets/ a directory",
		))
	}
	log.Log.V(1).Info("Using topology preset catalog", "source", presetCatalog.Source())

	resolved, err := ResolveKubeconfig(request.Kubeconfig)
	if err != nil {
		return exitWithReport(apperrors.NewValidationError(
			"kubeconfig required for validate",
			err,
			"Set $KUBECONFIG or pass --kubeconfig <path>",
		))
	}

	manifestDir, err := ResolveDeploymentDir(request.DeploymentFiles)
	if err != nil {
		return exitWithReport(apperrors.NewValidationError(
			"deployment files directory not found",
			err,
			"Run 'l8k generate' first or pass --deployment-files <path>",
		))
	}
	reportManifestDir = manifestDir

	// Static validation keeps the historical best-effort config behavior.
	// Connectivity is stricter because routing and GPUDirect expectations
	// cannot be inferred safely from the workload or embedded defaults.
	selectedRelease := ""
	profileRouting := config.RoutingDestinationBased
	validationCfg := config.DefaultValidationConfig()
	var clusterConfig []config.ClusterConfig
	configInput := UserConfigInput{
		Explicit:        request.UserConfig,
		DeploymentFiles: request.DeploymentFiles,
		ConfigDir:       request.ConfigDir,
	}
	userOwnedCfgPath := UserConfigPathBeforeDefaults(configInput)
	cfg, loadedCfgPath, cfgErr := LoadUserConfig(configInput, options.Options{
		ConfigDir:                request.ConfigDir,
		UserConfig:               request.UserConfig,
		NetworkOperatorNamespace: request.OperatorNamespace,

		SkipNetworkOperatorHelm:    request.SkipNetworkOperatorHelm.Value,
		SkipNetworkOperatorHelmSet: request.SkipNetworkOperatorHelm.Set,
	})
	cfgPath = loadedCfgPath
	// Preserve successfully parsed user input in partial reports even when a
	// release-catalog overlay fails. Operational validation still follows the
	// existing soft-failure path below and skips config-derived version checks.
	if cfg != nil {
		reportConfig = cfg
		if cfg.NetworkOperator != nil {
			skipNetworkOperatorHelm = cfg.NetworkOperator.SkipHelmChart
		}
		validationCfg = config.NormalizeValidationConfig(cfg.Validation)
	}
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
		clusterConfig = cfg.ClusterConfig
	}
	if err := applyValidationOverrides(request, validationCfg); err != nil {
		return exitWithReport(apperrors.NewValidationError(
			"invalid validation configuration",
			err,
			"Use --validation-mode quick|full|strict, --validation-checks icmp,rping,ib_write_bw, and positive RDMA parameter values",
		))
	}

	connectivityEnabled := validationConnectivityEnabled(validationCfg)
	connectivityChecks := connectivityChecksFromConfig(validationCfg)
	connectivityOnly := false
	if connectivityEnabled {
		if cfgErr != nil {
			return exitWithReport(apperrors.NewValidationError(
				"cluster config required for connectivity validation",
				cfgErr,
				"Provide a readable --user-config with profile.routing and validation.gpuDirect.enabled",
			))
		}
		if err := validateRequiredConnectivityConfig(userOwnedCfgPath, cfg, connectivityChecks); err != nil {
			return exitWithReport(apperrors.NewValidationError(
				"insufficient cluster config for connectivity validation",
				err,
				"Set profile.routing and validation.gpuDirect.enabled explicitly; GPUDirect also requires worker and rail topology",
			))
		}
		objects, refs, err := connectivity.LoadExampleDaemonSets(manifestDir)
		if err != nil {
			return exitWithReport(apperrors.NewValidationError(
				"invalid connectivity test workload",
				err,
				"Provide an apps/v1 test DaemonSet in an *example*.yaml file under --deployment-files",
			))
		}
		if err := validateConnectivityDaemonSets(objects, refs, connectivityChecks); err != nil {
			return exitWithReport(apperrors.NewValidationError(
				"invalid connectivity test workload",
				err,
				"Provide an apps/v1 test DaemonSet with a namespace and the containers required by the selected checks",
			))
		}
		hasDeploymentInputs, err := hasDeploymentValidationInputs(manifestDir)
		if err != nil {
			return exitWithReport(apperrors.NewValidationError(
				"failed to inspect deployment validation inputs",
				err,
				"Provide a readable --deployment-files directory",
			))
		}
		connectivityOnly = !hasDeploymentInputs
	}

	if !connectivityOnly && cfgErr == nil && cfg != nil {
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
		// Re-run preset matching only when the deployment-validation inputs
		// are present. A test-workload-only directory deliberately selects
		// connectivity-only validation.
		presetResults = presetmatch.MatchAllWithCatalog(cfg, presetCatalog)
	}

	log.Log.Info("Validating deployment",
		"kubeconfig", resolved,
		"manifestDir", manifestDir,
		"connectivityOnly", connectivityOnly,
		"operatorNamespace", operatorNamespace,
		"selectedRelease", selectedRelease)

	k8sClient, restConfig, err := runner.newKubeClient(resolved)
	if err != nil {
		return exitWithReport(apperrors.NewClusterError(
			"failed to create Kubernetes client",
			err,
			"Check that kubeconfig is valid and the cluster is reachable",
		))
	}
	reportClient = k8sClient
	reportRestConfig = restConfig

	ctx := operationContext
	verdict := validationVerdict{OK: true, VersionOK: true}
	componentMismatch := false
	helmValuesMismatch := false
	strayMismatch := false

	// emitReport writes the HTML file synchronously on every remaining exit
	// path, including success, in-progress no-op, and connectivity failure.
	emitReport := func() {
		overall := computeOverallVerdict(verdict, componentCheck, helmValuesCheck, strayCheck, matrix, presetResults)
		if connectivityOnly {
			overall = connectivityOnlyVerdict(matrix, connectivityChecks)
		}
		emitVerdictBanner(overall, request.OutputFormat)
		writeHTMLReportIfWanted(ctx, k8sClient, restConfig, manifestDir, request.DeploymentFiles,
			operatorNamespace, versionCheck, componentCheck, helmValuesCheck, strayCheck, results, &matrix, &warnings,
			presetResults, overall, reportConfig, request.OutputFormat,
			request.ReportPath, request.Version, request.Kubeconfig)
	}

	if !connectivityOnly {

		checkStarted := time.Now()
		if skipNetworkOperatorHelm {
			versionCheck = &networkoperatorplugin.VersionCheck{Skipped: true, Reason: skipNetworkOperatorHelmReason}
		} else {
			var vcErr error
			versionCheck, vcErr = networkoperatorplugin.CheckHelmReleaseVersion(ctx, k8sClient, operatorNamespace, selectedRelease)
			if vcErr != nil {
				return exitWithReport(apperrors.NewClusterError(
					"version check failed",
					vcErr,
					"Check that the kubeconfig has list-secrets permission in the operator namespace",
				))
			}
		}
		log.Log.V(1).Info("validation check completed", "check", "helm release version",
			"skipped", versionCheck.Skipped, "duration", time.Since(checkStarted).Round(time.Millisecond).String())

		// Cross-check the NicClusterPolicy + NicNodePolicy
		// component versions against the catalog. Soft errors only
		// — a failed lookup turns into ComponentCheck.Skipped, not
		// a hard exit, because the underlying Helm release check
		// already covers the dominant "wrong operator version"
		// case. The per-component breakdown is most useful when
		// catching out-of-band kubectl edits or partial upgrades.
		checkStarted = time.Now()
		ccCheck, ccErr := networkoperatorplugin.CheckComponentVersions(ctx, k8sClient, operatorNamespace, selectedRelease)
		log.Log.V(1).Info("validation check completed", "check", "component versions",
			"success", ccErr == nil, "duration", time.Since(checkStarted).Round(time.Millisecond).String())
		if ccErr != nil {
			log.Log.V(1).Info("component-version check failed", "error", ccErr.Error())
		}
		componentCheck = ccCheck

		// Helm values drift: compare the deployed release's
		// user-supplied values against the values.yaml that
		// `l8k generate` produced. Mismatches mean re-running
		// `l8k deploy` would change the chart configuration —
		// exit code 4, same as a version mismatch.
		checkStarted = time.Now()
		var hvCheck *networkoperatorplugin.HelmValuesCheck
		var hvErr error
		if skipNetworkOperatorHelm {
			hvCheck = &networkoperatorplugin.HelmValuesCheck{Skipped: true, Reason: skipNetworkOperatorHelmReason}
		} else {
			valuesPath := filepath.Join(manifestDir, "values.yaml")
			var generatedValuesYAML []byte
			if b, err := os.ReadFile(valuesPath); err == nil {
				generatedValuesYAML = b
			}
			hvCheck, hvErr = networkoperatorplugin.CheckHelmReleaseValues(ctx, restConfig, operatorNamespace, generatedValuesYAML)
		}
		log.Log.V(1).Info("validation check completed", "check", "Helm values drift",
			"success", hvErr == nil, "duration", time.Since(checkStarted).Round(time.Millisecond).String())
		if hvErr != nil {
			log.Log.V(1).Info("helm-values check failed", "error", hvErr.Error())
		}
		helmValuesCheck = hvCheck

		// Stray-CRs: any Network Operator-managed CR in the operator
		// namespace (or cluster-scoped Kinds cluster-wide) that l8k did
		// NOT render. Surfaces as a soft fail — the verdict picks it up,
		// the HTML report lists every offender, and the user can sweep
		// them with `l8k deploy --overwrite-existing`.
		checkStarted = time.Now()
		genRefs, scanErr := preflight.ScanGeneratedManifests(manifestDir)
		if scanErr != nil {
			log.Log.V(1).Info("stray-CR scan failed", "error", scanErr.Error())
		}
		stray := preflight.CheckStrayCRs(ctx, preflight.Inputs{
			KubeClient:         k8sClient,
			OperatorNamespace:  operatorNamespace,
			GeneratedManifests: genRefs,
		})
		log.Log.V(1).Info("validation check completed", "check", "stray custom resources",
			"success", scanErr == nil, "mismatches", len(stray.Mismatches),
			"duration", time.Since(checkStarted).Round(time.Millisecond).String())
		strayCheck = &stray

		var valErr error
		checkStarted = time.Now()
		results, valErr = networkoperatorplugin.ValidateManifests(ctx, k8sClient, manifestDir)
		log.Log.V(1).Info("validation check completed", "check", "manifest readiness",
			"success", valErr == nil, "objects", len(results),
			"duration", time.Since(checkStarted).Round(time.Millisecond).String())
		if valErr != nil {
			return exitWithReport(apperrors.NewGeneralError(
				"manifest validation failed",
				valErr,
			))
		}

		// Optional `--wait`: poll until every in-progress manifest
		// reaches a terminal state (or the deadline elapses). The
		// loop re-runs the registry-backed validate every 10s. The
		// final results / verdict are emitted normally below.
		if request.Wait > 0 {
			results = waitForReconcile(ctx, ctrlclient.Client(k8sClient), manifestDir, results, request.Wait)
		}

		verdict = emitValidationReport(versionCheck, results, presetDeviations, request.OutputFormat)
		emitComponentVersionReport(componentCheck, request.OutputFormat)
		emitPresetMatchReport(presetResults, request.OutputFormat)
		warnings = append(warnings, collectVerdictWarnings(verdict)...)
		if componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch {
			warnings = append(warnings, "Component versions in NicClusterPolicy / NicNodePolicy diverge from the selectedRelease catalog — see the report's components section.")
		}

		// `--connectivity` runs the data-plane matrix when no manifest
		// is broken (error or missing). Version mismatches (Helm
		// release or per-component) are *not* fatal here — the cluster
		// is still up, so the connectivity tests are still meaningful;
		// the final verdict picks up the mismatch as a fail reason.
		// In-progress (without errors) prints a warning and exits 0
		// so CI/operators can re-run later.
		componentMismatch = componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch
		helmValuesMismatch = helmValuesCheck != nil && !helmValuesCheck.Skipped && !helmValuesCheck.AllMatch
		strayMismatch = strayCheck != nil && strayCheck.Failed()
		switch {
		case verdict.HasError || verdict.HasMissing:
			emitReport()
			return apperrors.NewExitStatus(apperrors.ExitDeployment)
		case verdict.HasInProgress:
			if request.OutputFormat != "json" {
				fmt.Fprintln(os.Stderr, "\nNote: some manifests are still reconciling. Re-run later or use --wait to block.")
			}
			if validationConnectivityEnabled(validationCfg) && request.OutputFormat != "json" {
				fmt.Fprintln(os.Stderr, "Connectivity matrix skipped — cluster has in-progress manifests.")
			}
			warnings = append(warnings, "Connectivity matrix skipped — cluster has in-progress manifests.")
			emitReport()
			if !verdict.VersionOK || componentMismatch || helmValuesMismatch || strayMismatch {
				return apperrors.NewExitStatus(apperrors.ExitDeployment)
			}
			return nil
		}
	} else {
		log.Log.Info("Deployment validation inputs not found; running connectivity checks only",
			"manifestDir", manifestDir)
	}

	if connectivityEnabled {
		uiOutput, _ := ui.NewOutputForFormat(request.OutputFormat, request.AutoApprove)
		ctxWithUI := ui.WithOutput(ctx, uiOutput)
		m, err := runner.runConnectivityMatrix(ctxWithUI, k8sClient, restConfig, uiOutput, connectivity.Options{
			ManifestDir:             manifestDir,
			Timeout:                 request.ConnectivityTime,
			Keep:                    request.Keep,
			Mode:                    connectivity.Mode(validationCfg.Mode),
			Routing:                 profileRouting,
			Checks:                  connectivityChecks,
			RPingIterations:         validationCfg.RDMA.RPingIterations,
			IBWriteSize:             validationCfg.RDMA.IBWriteSize,
			IBWriteMinBandwidthGbps: *validationCfg.RDMA.IBWriteMinBandwidthGbps,
			ClusterConfig:           clusterConfig,
		})
		matrix = m
		if err != nil {
			return exitWithReport(apperrors.NewClusterError(
				"connectivity matrix failed",
				err,
				"Re-run with --log-level trace for bounded command output; add --keep to inspect the test DaemonSet",
			))
		}
		if request.OutputFormat == "json" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
				"connectivity": matrix,
			})
		}
		if matrix != nil && matrix.Skipped != nil {
			warnings = append(warnings, "Connectivity matrix skipped: "+matrix.Skipped.Reason)
		}
	}

	emitReport()
	if connectivityOnly && !connectivityOnlyVerdict(matrix, connectivityChecks).Pass {
		return apperrors.NewExitStatus(apperrors.ExitDeployment)
	}
	// Final exit code unifies every gating signal: matrix failures,
	// version/component mismatches, AND preset deviations all fail
	// the verdict, but the matrix still gets a chance to run when
	// the only problem is a stale Helm release / catalog mismatch
	// / hardware drift from the catalog preset.
	matrixFailed := matrix != nil && matrix.Summary.Failed > 0
	if matrixFailed || !verdict.VersionOK || componentMismatch || helmValuesMismatch || strayMismatch || hasPresetDeviation(presetResults) {
		return apperrors.NewExitStatus(apperrors.ExitDeployment)
	}
	return nil
}
