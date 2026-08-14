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
	"time"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/target"
	hosttarget "github.com/nvidia/k8s-launch-kit/pkg/target/host"
)

const defaultOperatorNamespace = "nvidia-network-operator"

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
	Run: func(cmd *cobra.Command, _ []string) {
		runTargetCommand(cmd, target.Validate, hosttarget.NewValidateAdapter(
			newHostValidateRequest(cmd),
			hosttarget.NewValidateRunner(),
		))
	},
}

func newHostValidateRequest(cmd *cobra.Command) hosttarget.ValidateRequest {
	return hosttarget.ValidateRequest{
		Kubeconfig:        kubeconfig,
		DeploymentFiles:   deploymentFiles,
		UserConfig:        userConfig,
		ConfigDir:         configDir,
		OperatorNamespace: networkOperatorNamespace,
		Connectivity: hosttarget.Explicit[bool]{
			Value: validateConnectivity,
			Set:   cmd.Flags().Changed("connectivity"),
		},
		Keep:             validateKeep,
		ConnectivityTime: validateConnectivityTimeout,
		Mode: hosttarget.Explicit[string]{
			Value: validateMode,
			Set:   cmd.Flags().Changed("validation-mode"),
		},
		Checks: hosttarget.Explicit[[]string]{
			Value: validateChecks,
			Set:   cmd.Flags().Changed("validation-checks"),
		},
		RDMAPIterations: hosttarget.Explicit[int]{
			Value: validateRDMAIterations,
			Set:   cmd.Flags().Changed("rdma-rping-iterations"),
		},
		RDMAIBWriteSize: hosttarget.Explicit[int]{
			Value: validateRDMAIBWriteSize,
			Set:   cmd.Flags().Changed("rdma-ib-write-size"),
		},
		RDMAMinBandwidth: hosttarget.Explicit[float64]{
			Value: validateRDMAIBWriteMinGbps,
			Set:   cmd.Flags().Changed("rdma-ib-write-min-bandwidth-gbps"),
		},
		Wait:       validateWait,
		ReportPath: validateReportPath,
		Version:    Version,
	}
}

func init() {
	rootCmd.AddCommand(validateCmd)
	addTargetFlag(validateCmd)

	validateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (falls back to $KUBECONFIG, then ~/.kube/config)")
	validateCmd.Flags().StringVar(&deploymentFiles, "deployment-files", DefaultDeploymentDir, "Directory containing the manifests to verify")
	validateCmd.Flags().StringVar(&userConfig, "user-config", "", "Cluster config file (auto-detected from ./cluster-config.yaml). Used to read networkOperator.selectedRelease and operator namespace.")
	validateCmd.Flags().StringVar(&networkOperatorNamespace, "network-operator-namespace", "", "Override the network operator namespace from cluster-config.yaml")
	validateCmd.Flags().BoolVar(&validateConnectivity, "connectivity", true, "Run a source-bound connectivity matrix (icmp + rping + ib_write_bw) between pods of the example DaemonSet. Default true. Pass --connectivity=false to skip when only the static manifest checks are wanted.")
	validateCmd.Flags().BoolVar(&validateKeep, "keep", false, "Leave the example DaemonSet running after --connectivity completes (useful for debugging).")
	validateCmd.Flags().DurationVar(&validateConnectivityTimeout, "connectivity-timeout", 0, "Maximum wall-clock budget for connectivity workload setup and test execution. 0 (default) calculates the budget from the generated matrix plan.")
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
	markValidateTargetScopes()
}
