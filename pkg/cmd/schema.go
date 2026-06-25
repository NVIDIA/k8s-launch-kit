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
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
)

// commandSchema describes a subcommand for AI agent discovery.
type commandSchema struct {
	Description string `json:"description"`
	Example     string `json:"example"`
}

// schema represents the tool's capabilities in a machine-readable format.
type schema struct {
	Version                          string                   `json:"version"`
	Description                      string                   `json:"description"`
	Commands                         map[string]commandSchema `json:"commands"`
	Phases                           []string                 `json:"phases"`
	Fabrics                          []string                 `json:"fabrics"`
	DeploymentTypes                  []string                 `json:"deploymentTypes"`
	OutputFormats                    []string                 `json:"outputFormats"`
	SupportedNetworkOperatorReleases []string                 `json:"supportedNetworkOperatorReleases"`
	ExitCodes                        map[string]string        `json:"exitCodes"`
	Flags                            map[string]flagSchema    `json:"flags"`
}

type flagSchema struct {
	Type        string `json:"type"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Print tool capabilities as JSON (for AI agents and automation)",
	Long:  `Output a machine-readable JSON description of l8k's capabilities, flags, and exit codes. Designed for AI agents to programmatically discover what this tool can do.`,
	Run: func(cmd *cobra.Command, args []string) {
		s := schema{
			Version:     Version,
			Description: "CLI tool for deploying NVIDIA cloud-native networking solutions on Kubernetes",
			Commands: map[string]commandSchema{
				"discover": {
					Description: "Discover cluster network hardware capabilities",
					Example:     "l8k discover --kubeconfig ~/.kube/config --save-cluster-config ./cluster-config.yaml",
				},
				"generate": {
					Description: "Generate deployment manifests for a network profile",
					Example:     "l8k generate --user-config cluster-config.yaml --fabric ethernet --deployment-type sriov --save-deployment-files ./output",
				},
				"deploy": {
					Description: "Apply previously generated manifests to a Kubernetes cluster (NicClusterPolicy → per-group NicNodePolicy → remaining)",
					Example:     "l8k deploy --deployment-files ./deployment --kubeconfig ~/.kube/config",
				},
				"validate": {
					Description: "Verify a deployment matches the selected Network Operator release (Helm chart version + manifest presence in cluster)",
					Example:     "l8k validate --user-config ./cluster-config.yaml --deployment-files ./deployment",
				},
				"sosreport": {
					Description: "Collect diagnostic sosreport from a Kubernetes cluster",
					Example:     "l8k sosreport --kubeconfig ~/.kube/config --output-dir ./sosreport",
				},
				"schema": {
					Description: "Print tool capabilities as JSON (this command)",
					Example:     "l8k schema",
				},
			},
			Phases:                           []string{"discover", "generate", "deploy"},
			Fabrics:                          []string{"ethernet", "infiniband"},
			DeploymentTypes:                  []string{"sriov", "rdma_shared", "host_device"},
			OutputFormats:                    []string{"text", "json"},
			SupportedNetworkOperatorReleases: releases.SupportedReleases(),
			ExitCodes: map[string]string{
				"0": "success",
				"1": "general_error",
				"2": "validation_error",
				"3": "cluster_error",
				"4": "deployment_error",
				"5": "partial_success",
			},
			Flags: map[string]flagSchema{
				"--kubeconfig": {
					Type:        "string",
					Description: "Path to kubeconfig file for cluster access",
				},
				"--discover-cluster-config": {
					Type:        "bool",
					Default:     "false",
					Description: "Deploy a thin Network Operator profile to discover cluster capabilities",
				},
				"--user-config": {
					Type:        "string",
					Description: "Use provided cluster configuration file (as base config for discovery or as full config without discovery)",
				},
				"--fabric": {
					Type:        "string",
					Description: "Select the fabric type: infiniband, ethernet",
				},
				"--deployment-type": {
					Type:        "string",
					Description: "Select the deployment type: sriov, rdma_shared, host_device",
				},
				"--multirail": {
					Type:        "bool",
					Default:     "false",
					Description: "Enable multirail deployment",
				},
				"--spectrum-x": {
					Type:        "string",
					Description: "Enable Spectrum-X by passing the SPC-X RA version (e.g. RA2.1, RA2.2). Folds in the legacy --spcx-version. Implies --fabric ethernet --deployment-type sriov --multirail. --multiplane-mode, --number-of-planes, and --network-operator-release are all mandatory under --spectrum-x; the (RA, release) pair is validated against the supported set (RA2.1 → 26.1, RA2.2 → 26.4).",
				},
				"--multiplane-mode": {
					Type:        "string",
					Description: "Spectrum-X multiplane mode: none, swplb, hwplb, uniplane (required with --spectrum-x)",
				},
				"--number-of-planes": {
					Type:        "int",
					Description: "Number of planes for Spectrum-X: 1, 2, or 4 (required with --spectrum-x)",
				},
				"--save-deployment-files": {
					Type:        "string",
					Default:     "./deployment",
					Description: "Save generated deployment files to the specified directory",
				},
				"--deploy": {
					Type:        "bool",
					Default:     "false",
					Description: "Deploy the generated files to the Kubernetes cluster",
				},
				"--dry-run": {
					Type:        "bool",
					Default:     "false",
					Description: "Preview what would be deployed without applying changes (requires --deploy)",
				},
				"--output": {
					Type:        "string",
					Default:     "text",
					Description: "Output format: text (human-readable) or json (structured for automation)",
				},
				"--yes": {
					Type:        "bool",
					Default:     "false",
					Description: "Auto-confirm all prompts without interactive input",
				},
				"--quiet": {
					Type:        "bool",
					Default:     "false",
					Description: "Suppress informational output (errors still shown)",
				},
				"--groups": {
					Type:        "[]string",
					Description: "Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.",
				},
				"--gpu-type": {
					Type:        "string",
					Description: "Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.",
				},
				"--node-selector": {
					Type:        "string",
					Default:     "feature.node.kubernetes.io/pci-15b3.present=true",
					Description: "Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe",
				},
				"--image-pull-secrets": {
					Type:        "[]string",
					Description: "Image pull secret names for NicClusterPolicy (comma-separated)",
				},
				"--network-operator-release": {
					Type:        "string",
					Description: "Network Operator release line (MAJOR.MINOR). See supportedNetworkOperatorReleases for valid values; populates component versions and gates version-specific template sections.",
				},
			},
		}
		data, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(data))
	},
}

func init() {
	rootCmd.AddCommand(schemaCmd)
}
