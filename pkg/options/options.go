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
	SaveClusterConfig       string // Path to save discovered config
	NetworkOperatorNamespace string   // Override namespace for Network Operator (optional)
	ImagePullSecrets        []string // Image pull secret names for NicClusterPolicy

	// Phase 2: Deployment Generation
	Fabric              string // Fabric type to deploy
	DeploymentType      string // Deployment type to deploy
	Multirail           bool   // Whether to deploy with multirail
	SpectrumX           bool   // Whether to deploy with Spectrum X
	Ai                  bool   // Whether to deploy with AI
	SPCXVersion         string // Spectrum-X firmware version (default: RA2.2)
	MultiplaneMode      string // Spectrum-X multiplane mode (default: swplb)
	NumberOfPlanes      int    // Number of planes for Spectrum-X (default: 4)
	Prompt              string // Path to file with a prompt to use for LLM-assisted profile generation
	Group               string // Generate templates for a specific group identifier only
	NodeSelector        string // Filter nodes for discovery and manifests (e.g., "key1=val1,key2=val2")
	PodNamespace        string // Namespace for pods and network resources (overrides config)
	SaveDeploymentFiles string // Directory to save generated files

	LLMApiKey      string // API key for the LLM API
	LLMApiUrl      string // API URL for the LLM API
	LLMVendor      string // Vendor of the LLM API
	LLMModel       string // Model name for the LLM API
	LLMInteractive bool   // Enable interactive chat mode
	LLMThrottle    bool   // Enable rate limit throttling for API calls

	EnabledPlugins []string // Enabled plugins

	// Workload
	WorkloadManifest string // Path to user-defined workload manifest

	// DOCA Driver
	EnableDocaDriver *bool // Override docaDriver.enable from config (nil = use config value)

	// Troubleshooting (used in interactive mode)
	SosreportPath string // Pre-collected sosreport directory (skip collection)

	// Phase 3: Cluster Deployment
	Deploy     bool   // Whether to deploy to cluster
	Kubeconfig string // Path to kubeconfig for discovery and deployment

	// Output control
	OutputFormat string // Output format: "text" (default) or "json"
	Yes          bool   // Auto-confirm all prompts (--yes)
	Quiet        bool   // Suppress informational output (--quiet)
	DryRun       bool   // Preview without applying changes (--dry-run)
}
