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

import (
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
)

// Options holds all the configuration parameters for the application
type Options struct {
	// LaunchKitVersion is the version reported by `l8k version`. The CLI
	// passes it through so generated resources can record their producer.
	LaunchKitVersion string

	// Logging
	LogLevel string
	LogFile  string // Path to log file (optional)

	// Phase 1: Cluster Discovery
	// ConfigDir is an optional root containing l8k-config.yaml and/or presets/.
	// Explicit --user-config still has higher precedence for the config file.
	ConfigDir             string
	UserConfig            string // Path to user-provided config (skips discovery)
	DiscoverClusterConfig bool   // Whether to discover cluster config
	// DiscoverOnly skips Phase 2 (manifest generation) entirely. Set by
	// the standalone `l8k discover` subcommand so its run produces only
	// cluster-config.yaml and never errors on "no profile selected".
	DiscoverOnly             bool
	SaveClusterConfig        string // Path to save discovered config
	NetworkOperatorNamespace string `flag:"network-operator-namespace" config:"networkOperator.namespace" scopes:"root,generate,discover" usage:"Override the Network Operator namespace from the config file"`
	// KeepNamespace, when true, suppresses teardown of the
	// nvidia-k8s-launch-kit bootstrap namespace at the end of `discover` —
	// useful for debugging a failed run.
	KeepNamespace bool
	// CollapseNicRails (default true; --collapse-nic-rails) makes discovery
	// advertise one rail per NIC — multi-plane NICs collapse to their master
	// PF, while genuinely dual-port NIC models keep a rail per port. False
	// restores the legacy one-rail-per-PF behaviour for dev setups.
	CollapseNicRails bool
	// NetworkOperatorRelease is a MAJOR.MINOR catalog key (e.g. "26.4"), not
	// a full semver. Selects component image tags + repository from the
	// embedded releases catalog and drives version-gated template sections.
	NetworkOperatorRelease string   `flag:"network-operator-release" config:"networkOperator.selectedRelease,networkOperator.version,networkOperator.componentVersion,networkOperator.repository,networkOperator.operatorRepository,networkOperator.helmRepoURL,docaDriver.version" resolve:"network-operator-release" scopes:"root,generate,discover" usage:"Network Operator release line to deploy (MAJOR.MINOR); selects catalog-managed component versions and repositories"`
	ImagePullSecrets       []string `flag:"image-pull-secrets" config:"networkOperator.imagePullSecrets" scopes:"root,generate,discover" usage:"Image pull secret names for Network Operator components and authenticated Helm downloads (comma-separated)"`
	// SkipNetworkOperatorHelm disables values.yaml generation, Network Operator
	// Helm installation, and Helm-specific validation. The Set companion keeps
	// an omitted flag distinct from --skip-network-operator-helm=false.
	SkipNetworkOperatorHelm    bool `flag:"skip-network-operator-helm" config:"networkOperator.skipHelmChart" scopes:"root,generate" usage:"Skip Network Operator Helm values generation, chart installation, and Helm-specific validation"`
	SkipNetworkOperatorHelmSet bool

	// Phase 2: Deployment Generation
	Fabric         string `flag:"fabric" config:"profile.fabric" scopes:"root,generate,discover" usage:"Fabric type: ethernet or infiniband"`
	DeploymentType string `flag:"deployment-type" config:"profile.deployment" scopes:"root,generate,discover" usage:"Deployment type: sriov, rdma_shared, or host_device"`
	Multirail      bool   `flag:"multirail" config:"profile.multirail" scopes:"root,generate,discover" usage:"Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)"`
	// MultirailSet is true when the user explicitly passed `--multirail`
	// (regardless of value). Without it, the bool zero value can't be
	// distinguished from "not passed", which matters once
	// `pkg/resolve.ApplyHardwareDefaults` defaults Multirail to true:
	// `ApplyOptionsToConfig` only overrides the HW default when
	// MultirailSet is true, so a user passing `--multirail=false`
	// correctly opts out. YAML presence is tracked separately by
	// `config.Profile.MultirailSet`.
	MultirailSet bool
	Routing      string `flag:"routing" config:"profile.routing" scopes:"root,generate,discover" usage:"Secondary-network routing mode: destination-based or source-based"`
	IgnoreARP    bool   `flag:"ignore-arp" config:"profile.ignoreARP" scopes:"root,generate,discover" usage:"Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails"`
	// IgnoreARPSet is true when the user explicitly passed `--ignore-arp`
	// (including `--ignore-arp=false`). This prevents the bool zero value from
	// clobbering profile.ignoreARP from the config when the flag is omitted.
	IgnoreARPSet   bool
	SpectrumX      bool   // True when --spectrum-x is set; derived from SPCXVersion != ""
	SPCXVersion    string `flag:"spectrum-x" config:"profile.spectrumX.enable,profile.spectrumX.spcxVersion" resolve:"spectrum-x" scopes:"root,generate,discover" usage:"Enable Spectrum-X by passing the SPC-X RA version"`
	MultiplaneMode string `flag:"multiplane-mode" config:"profile.spectrumX.multiplaneMode" scopes:"root,generate,discover" usage:"Spectrum-X multiplane mode: none, swplb, or hwplb"`
	NumberOfPlanes int    `flag:"number-of-planes" config:"profile.spectrumX.numberOfPlanes" scopes:"root,generate,discover" usage:"Spectrum-X plane count: 1, 2, or 4"`
	TopologyScheme string `flag:"topology-scheme" config:"profile.spectrumX.topologyType" scopes:"root,generate,discover" usage:"Spectrum-X topology scheme: 2-tier or 3-tier"`
	IPVersion      string `flag:"ip-version" config:"profile.spectrumX.ipVersion" scopes:"root,generate,discover" usage:"Spectrum-X address family: ipv4 or ipv6"`
	TopologyFile   string `flag:"topology-file" config:"profile.spectrumX.topologyFile" scopes:"root,generate,discover" usage:"Path to a Spectrum-X reference-generator or NVIDIA AIR topology JSON file"`
	// SpectrumXConfig is a path to either a full Spectrum-X profile ConfigMap
	// YAML or the raw data.profile YAML body. SpectrumXConfigMapName is required
	// only when SpectrumXConfig contains the raw profile body.
	SpectrumXConfig        string `flag:"spectrum-x-config" config:"profile.spectrumX.profile,profile.spectrumX.configMapName" resolve:"spectrum-x-config" scopes:"root,generate,discover" usage:"Path to a full Spectrum-X profile ConfigMap or raw data.profile YAML"`
	SpectrumXConfigMapName string `flag:"spectrum-x-configmap-name" config:"profile.spectrumX.configMapName" scopes:"root,generate,discover" usage:"ConfigMap name used when --spectrum-x-config contains raw data.profile YAML"`
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
	ForPreset string
	// NetworkNamespaces is the comma-separated list from --network-namespaces:
	// the namespaces the secondary-network CRs + example test DaemonSets are
	// rendered into (one copy per namespace). Empty defaults to "default".
	NetworkNamespaces   []string `flag:"network-namespaces" config:"networkNamespaces" scopes:"root,generate" usage:"Namespaces for secondary-network resources and example workloads (comma-separated)"`
	SaveDeploymentFiles string   // Directory to save generated files

	EnabledPlugins []string // Enabled plugins

	// Workload
	WorkloadManifest string `flag:"workload-manifest" config:"workload.manifest" scopes:"root,generate" usage:"Path to a custom workload manifest YAML"`

	// DOCA Driver
	EnableDocaDriver *bool `flag:"enable-doca-driver" config:"docaDriver.enable" scopes:"root,generate" usage:"Enable or disable DOCA driver deployment"`

	// ConfigInputs is populated from the tags above by pkg/configflags. It
	// records flag presence separately from Go zero values, so false, zero, and
	// empty explicit values retain normal CLI precedence.
	ConfigInputs configinput.Values

	// Phase 3: Cluster Deployment
	Deploy     bool   // Whether to deploy to cluster
	Kubeconfig string // Path to kubeconfig for discovery and deployment
	// DeployTimeout caps the *entire* deploy phase end-to-end (apply +
	// readiness wait for every manifest). A zero value means no
	// deadline — appropriate for large SR-IOV clusters where a single
	// reconciliation can outlast any reasonable per-manifest budget.
	// Plumbed into ctx via context.WithTimeout before
	// `networkoperatorplugin.ApplyManifestsFromDir` is invoked, so
	// every poll inside the deploy state machine observes the same
	// deadline.
	DeployTimeout time.Duration

	// OverwriteExisting forwards the `--overwrite-existing` flag through
	// to Phase 0 of `networkoperatorplugin.ApplyManifestsFromDir`. When
	// true and a network-operator helm release already exists in the
	// target namespace with values that differ from the freshly rendered
	// `values.yaml`, the install is promoted to `helm upgrade --install`.
	// When false (default), a value-conflict surfaces as a deployment
	// error pointing at this flag.
	OverwriteExisting bool

	// Output control
	OutputFormat string // Output format: "text" (default) or "json"
	Yes          bool   // Auto-confirm all prompts (--yes)
	Quiet        bool   // Suppress informational output (--quiet)
	DryRun       bool   // Preview without applying changes (--dry-run)
}
