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
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// Phase 2 connectivity-test flags. `--connectivity` defaults to ON —
// every `l8k validate` verifies the data plane (apply example DS,
// wait Ready, ping matrix, cleanup) unless the caller passes
// `--connectivity=false`. The other flags tune matrix behaviour
// (--ping-count, --connectivity-timeout, --keep) or extend the
// validate semantics (--wait blocks until in-progress manifests reach
// a terminal state).
//
// Phase 3 adds --report-path: emit an HTML verify-report alongside
// the text/JSON output. Empty default means "auto-place at
// <deployment-files>/verify-report.html"; the literal "-" disables
// report writing.
var (
	validateConnectivity        bool
	validateKeep                bool
	validateConnectivityTimeout time.Duration
	validatePingCount           int
	validateWait                time.Duration
	validateReportPath          string
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
		// State accumulated during the run. Captured by reference
		// in exitWithReport so that EVERY error path — including
		// the ones that go through exitWithError(...).os.Exit —
		// still emits the HTML report with whatever was observed.
		// Go's defer doesn't run on os.Exit, so we can't rely on
		// a deferred report write.
		var (
			versionCheck     *networkoperatorplugin.VersionCheck
			componentCheck   *networkoperatorplugin.ComponentVersionCheck
			results          []networkoperatorplugin.ValidationResult
			matrix           *connectivity.MatrixResult
			warnings         []string
			presetDeviations []groupDeviationReport
			reportClient     ctrlclient.Client
			reportRestConfig *rest.Config
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
			writeHTMLReportIfWanted(context.Background(), reportClient, reportRestConfig,
				reportManifestDir, deploymentFiles,
				operatorNamespace, versionCheck, componentCheck, results, &matrix, &warnings,
				userConfigPath(), outputFormat)
			exitWithError(err, outputFormat)
		}

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
		selectedRelease := ""
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
		warnings = append(warnings, collectVerdictWarnings(verdict)...)
		if componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch {
			warnings = append(warnings, "Component versions in NicClusterPolicy / NicNodePolicy diverge from the selectedRelease catalog — see the report's components section.")
		}

		// emitReport writes the HTML file synchronously. Called on
		// every remaining exit path (success, in-progress no-op,
		// connectivity failure) since Go's defer doesn't run on
		// os.Exit and exitWithError does os.Exit.
		emitReport := func() {
			writeHTMLReportIfWanted(ctx, k8sClient, restConfig, manifestDir, deploymentFiles,
				operatorNamespace, versionCheck, componentCheck, results, &matrix, &warnings,
				userConfigPath(), outputFormat)
		}

		// `--connectivity` runs the data-plane ping matrix only when
		// every CR is reconciled — otherwise we'd just produce noise.
		// In-progress (without errors) prints a warning and exits 0
		// so CI/operators can re-run later.
		componentMismatch := componentCheck != nil && !componentCheck.Skipped && !componentCheck.AllMatch
		switch {
		case verdict.HasError || verdict.HasMissing || !verdict.VersionOK || componentMismatch:
			emitReport()
			os.Exit(apperrors.ExitDeployment)
		case verdict.HasInProgress:
			if outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "\nNote: some manifests are still reconciling. Re-run later or use --wait to block.")
			}
			if validateConnectivity && outputFormat != "json" {
				fmt.Fprintln(os.Stderr, "Connectivity matrix skipped — cluster has in-progress manifests.")
			}
			warnings = append(warnings, "Connectivity matrix skipped — cluster has in-progress manifests.")
			emitReport()
			return
		}

		if validateConnectivity {
			uiOutput, _ := ui.NewOutputForFormat(outputFormat, yesFlag)
			ctxWithUI := ui.WithOutput(ctx, uiOutput)
			m, err := connectivity.RunMatrix(ctxWithUI, k8sClient, restConfig, uiOutput, connectivity.Options{
				ManifestDir: manifestDir,
				Timeout:     validateConnectivityTimeout,
				PingCount:   validatePingCount,
				Keep:        validateKeep,
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
			if matrix != nil && (matrix.Summary.Failed > 0 || matrix.Summary.ExecErrors > 0) {
				emitReport()
				os.Exit(apperrors.ExitDeployment)
			}
		}

		emitReport()
	},
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
		return filepath.Join(deploymentDir, "verify-report.html")
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
	results []networkoperatorplugin.ValidationResult,
	matrix **connectivity.MatrixResult,
	warnings *[]string,
	userCfgPath string,
	outputFormat string,
) {
	path := resolveReportPath(validateReportPath, deploymentDir)
	if path == "" {
		return
	}

	data := connectivity.ReportData{
		Cluster: connectivity.ClusterInfo{
			L8kVersion:        Version,
			GeneratedAt:       time.Now().UTC(),
			KubeContext:       readKubeContext(),
			APIServerVersion:  probeAPIServerVersion(restConfig),
			OperatorNamespace: operatorNamespace,
		},
		Profile:         loadProfileInfo(userCfgPath, manifestDir),
		NodeGroups:      loadNodeGroups(userCfgPath),
		Nodes:           listNodesForReport(ctx, c),
		Release:         versionCheck,
		ComponentCheck:  componentCheck,
		Manifests:       results,
		Matrix:          *matrix,
		Warnings:        *warnings,
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
func loadNodeGroups(userCfgPath string) []connectivity.NodeGroupInfo {
	if userCfgPath == "" {
		return nil
	}
	cfg, err := config.LoadFullConfig(userCfgPath, log.Log)
	if err != nil || cfg == nil {
		return nil
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
		for _, d := range g.PresetDeviation {
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
		out = append(out, ng)
	}
	return out
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

// userConfigPath returns the user-config path to read. Lookup order:
//
//   1. The explicit --user-config when set.
//   2. ./cluster-config.yaml in the current working directory (the
//      historical default — `l8k discover` writes here when no path
//      is given).
//   3. <deployment-files>/../cluster-config.yaml — the convention
//      `l8k discover --save-cluster-config <dir>/cluster-config.yaml
//      --save-deployment-files <dir>/deployment` produces, so an
//      operator running `l8k validate --deployment-files
//      <dir>/deployment` from anywhere finds the matching config.
//   4. <deployment-files>/cluster-config.yaml — fallback for users
//      who keep the config inside the deployment dir.
//
// Returns "" when none of these resolve to a readable file; the
// caller (validate) softens its version check to "skipped" in that
// case.
func userConfigPath() string {
	candidates := []string{}
	if userConfig != "" {
		candidates = append(candidates, userConfig)
	}
	candidates = append(candidates, defaultUserConfigPath)
	if deploymentFiles != "" {
		candidates = append(candidates,
			filepath.Join(deploymentFiles, "..", "cluster-config.yaml"),
			filepath.Join(deploymentFiles, "cluster-config.yaml"),
		)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
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
	validateCmd.Flags().StringVar(&validateReportPath, "report-path", "", "Write an HTML verify-report to this path. When empty (default), writes to <deployment-files>/verify-report.html. Pass '-' to skip the report file entirely.")

	setFlagGroup(validateCmd, "kubeconfig", GroupCommon)
	setFlagGroup(validateCmd, "user-config", GroupCommon)
	setFlagGroup(validateCmd, "deployment-files", GroupGeneration)
}
