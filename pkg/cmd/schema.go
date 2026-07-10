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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
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
					Description: "Discover cluster network hardware, resolve profile settings, and persist both to cluster-config.yaml",
					Example:     "l8k discover --kubeconfig ~/.kube/config --fabric ethernet --save-cluster-config ./cluster-config.yaml",
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
					Description: "Verify a deployment matches the selected Network Operator release (Helm chart version + manifest state + configurable RDMA connectivity checks)",
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
				"--config-dir": {
					Type:        "string",
					Description: "Directory containing optional l8k-config.yaml and presets/ overrides",
				},
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
					Description: "Override the resolved fabric type: infiniband, ethernet. Accepted by discover and generate.",
				},
				"--deployment-type": {
					Type:        "string",
					Description: "Override the resolved deployment type: sriov, rdma_shared, host_device. Accepted by discover and generate.",
				},
				"--multirail": {
					Type:        "bool",
					Default:     "true when absent",
					Description: "Override multirail deployment. Explicit false values from YAML or --multirail=false are preserved.",
				},
				"--routing": {
					Type:        "string",
					Default:     config.RoutingDestinationBased,
					Description: "Secondary-network routing mode: destination-based or source-based. source-based chains the automatic sbr CNI meta-plugin. Not applied to Spectrum-X profiles.",
				},
				"--ignore-arp": {
					Type:        "bool",
					Default:     "false",
					Description: "Chain the tuning CNI meta-plugin to make ARP ownership interface-local and prevent ARP flux across pod rails. Not applied to Spectrum-X profiles.",
				},
				"--spectrum-x": {
					Type:        "string",
					Description: "Enable Spectrum-X by passing the SPC-X RA version (e.g. RA2.1, RA2.2, RA2.3). Implies ethernet, sriov, and multirail; hardware-derived mode and plane defaults are persisted by discover.",
				},
				"--multiplane-mode": {
					Type:        "string",
					Description: "Spectrum-X multiplane mode override: none, swplb, hwplb, uniplane. Auto-derived from NIC device ID when omitted.",
				},
				"--number-of-planes": {
					Type:        "int",
					Description: "Spectrum-X plane count override: 1, 2, or 4. Auto-derived from NIC device ID when omitted.",
				},
				"--spectrum-x-config": {
					Type:        "string",
					Description: "Path to full Spectrum-X profile ConfigMap YAML or raw data.profile YAML. Required for SPC-X RA versions newer than RA2.2.",
				},
				"--spectrum-x-configmap-name": {
					Type:        "string",
					Description: "Spectrum-X profile ConfigMap name when --spectrum-x-config contains raw data.profile YAML.",
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
				"--validation-checks": {
					Type:        "[]string",
					Default:     "inherit from validation.checks",
					Description: "Comma-separated RDMA checks to run during validate connectivity: rping, ib_write_bw.",
				},
				"--rdma-rping-iterations": {
					Type:        "int",
					Default:     "0 (inherit from validation.rdma.rpingIterations)",
					Description: "Number of rping client iterations during validate connectivity.",
				},
				"--rdma-ib-write-size": {
					Type:        "int",
					Default:     "0 (inherit from validation.rdma.ibWriteSize)",
					Description: "Message size for ib_write_bw -s during validate connectivity.",
				},
				"--rdma-ib-write-min-bandwidth-gbps": {
					Type:        "float",
					Default:     "0 (inherit from validation.rdma.ibWriteMinBandwidthGbps)",
					Description: "Minimum observed ib_write_bw peak bandwidth in Gbps required for a validate connectivity test to pass. Use 0 to disable bandwidth gating.",
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
