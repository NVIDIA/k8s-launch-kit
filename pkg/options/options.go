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

package options

// Options holds all the configuration parameters for the application
type Options struct {
	// Logging
	LogLevel string
	LogFile  string // Path to log file (optional)

	// Phase 1: Cluster Discovery
	UserConfig              string // Path to user-provided config (skips discovery)
	DiscoverClusterConfig   bool   // Whether to discover cluster config
	// DiscoverOnly skips Phase 2 (manifest generation) entirely. Set by
	// the standalone `l8k discover` subcommand so its run produces only
	// cluster-config.yaml and never errors on "no profile selected".
	DiscoverOnly      bool
	SaveClusterConfig string // Path to save discovered config
	NetworkOperatorNamespace string   // Override namespace for Network Operator (optional)
	// NetworkOperatorRelease is a MAJOR.MINOR catalog key (e.g. "26.4"), not
	// a full semver. Selects component image tags + repository from the
	// embedded releases catalog and drives version-gated template sections.
	NetworkOperatorRelease  string
	ImagePullSecrets        []string // Image pull secret names for NicClusterPolicy

	// Phase 2: Deployment Generation
	Fabric              string // Fabric type to deploy
	DeploymentType      string // Deployment type to deploy
	Multirail           bool   // Whether to deploy with multirail
	SpectrumX           bool   // True when --spectrum-x is set; derived from SPCXVersion != ""
	SPCXVersion         string // Spectrum-X RA version (the value of --spectrum-x; empty = disabled)
	MultiplaneMode      string // Spectrum-X multiplane mode (default: swplb)
	NumberOfPlanes      int    // Number of planes for Spectrum-X (default: 4)
	// Groups limits `l8k generate` to the named source groups (matched
	// case-sensitively against `clusterConfig[].identifier`). Comma-separated
	// on the CLI (`--groups a,b`). Mutually exclusive with GpuType.
	Groups []string
	// GpuType limits `l8k generate` to source groups whose `gpuType` matches.
	// Single value, case-insensitive (`--gpu-type NVIDIA-H200`). Mutually
	// exclusive with Groups.
	GpuType      string
	NodeSelector string // Filter nodes for discovery and manifests (e.g., "key1=val1,key2=val2")
	// ForPreset is the directory name of a topology preset under presets/. When
	// set, generate replaces fullConfig.ClusterConfig with a single group
	// synthesized from the preset (skipping cluster discovery). Requires
	// NodeSelector since the preset has no live worker-node list.
	ForPreset           string
	PodNamespace        string // Namespace for pods and network resources (overrides config)
	SaveDeploymentFiles string // Directory to save generated files

	EnabledPlugins []string // Enabled plugins

	// Workload
	WorkloadManifest string // Path to user-defined workload manifest

	// DOCA Driver
	EnableDocaDriver *bool // Override docaDriver.enable from config (nil = use config value)

	// Phase 3: Cluster Deployment
	Deploy     bool   // Whether to deploy to cluster
	Kubeconfig string // Path to kubeconfig for discovery and deployment

	// Output control
	OutputFormat string // Output format: "text" (default) or "json"
	Yes          bool   // Auto-confirm all prompts (--yes)
	Quiet        bool   // Suppress informational output (--quiet)
	DryRun       bool   // Preview without applying changes (--dry-run)
}
