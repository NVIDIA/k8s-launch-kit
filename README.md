# NVIDIA Kubernetes Launch Kit

Kubernetes Launch Kit (`l8k`) helps platform teams deploy NVIDIA networking on Kubernetes and OpenShift clusters. It discovers NIC and GPU topology, generates reviewable Network Operator resources, deploys them in dependency order, and validates their state and data-plane connectivity. It supports SR-IOV, RDMA shared-device, host-device, InfiniBand, and Spectrum-X profiles; [Plan your deployment](docs/user/profiles.md) explains applicability and prerequisites.
Standard SR-IOV, RDMA-shared and host-device profiles can opt into NIC tuning with `--deploy-nic-configuration-template`. See [NIC configuration](docs/reference/configuration.md#nic-configuration-and-naming) for fabric-specific settings and operator ownership.


The current lifecycle uses the `host` target by default. The reserved `dpf` target does not have available lifecycle phases; see [target extension](docs/advanced/targets.md) for its implementation status.

## Documentation

The [Launch Kit documentation](https://nvidia.github.io/k8s-launch-kit/) is the operational guide. Start with [planning](docs/user/profiles.md), [installation](docs/user/installation.md), and the [SR-IOV Ethernet walkthrough](docs/user/quick-start.md). OpenShift and Spectrum-X have [separate](docs/user/openshift.md) [guides](docs/user/spectrum-x.md). The [CLI](docs/reference/cli.md) and [configuration](docs/reference/configuration.md) references cover exact inputs.

## Installation

<span id="quick-install-from-github-releases"></span>

Install a published binary using the [installation methods and version guidance](docs/user/installation.md), then verify it:

```bash
l8k version
l8k schema
```

## Operation Phases

### Discover Cluster Configuration

`l8k discover` bootstraps a temporary privileged daemon on eligible Ready nodes, collects hardware inventory, and saves `cluster-config.yaml`. A normal CLI `--node-selector` value is saved for deployment-time selection; it does not limit where the discovery daemon runs. Discovery does not walk kernel-module holder graphs. Both DOCA driver unload controls default to true; review dependent storage and RDMA workloads before deployment. See [discovery scope and driver safety](docs/user/discovery.md#driver-module-safety).

### Select the Deployment Profile

The profile describes fabric, workload device exposure, multirail and optional Spectrum-X behavior. Use [planning and profile choice](docs/user/profiles.md) before overriding a discovered profile.

### Generate Deployment Files

`l8k generate` leaves the input YAML unchanged and writes manifests plus `.l8k/resolved-config.yaml` under the deployment directory. Review both before applying; keep the complete directory for later deploy and validation. See [generation](docs/advanced/generation.md).

## Usage

<span id="subcommand-workflow-recommended"></span>
<span id="complete-workflow-root-command"></span>

This example shows the stage order for a Kubernetes SR-IOV Ethernet deployment. Choose the Network Operator release and site inputs using [planning](docs/user/profiles.md) before starting:

```bash
l8k discover --kubeconfig "$KUBECONFIG" \
  --network-operator-release 26.4 \
  --save-cluster-config ./cluster-config.yaml
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./deployment
```

Review the discovered workers, NICs, addressing, driver settings, maintenance limits, and generated manifests. The [first deployment walkthrough](docs/user/quick-start.md) shows the review and preview steps before `l8k deploy`, followed by [acceptance](docs/user/validation.md#acceptance-outcomes).

### Generated command help

<details>
<summary>Root command help (generated with <code>make update-readme</code>)</summary>

<!-- BEGIN HELP -->
<!-- This section is automatically updated by running 'make update-readme' -->

```

K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

### Discover Cluster Configuration
Bootstrap a private NIC Configuration Daemon to discover your cluster's
network capabilities and hardware configuration with --discover-cluster-config.
This phase can be skipped if you provide your own configuration file by using --user-config.
This phase resolves credentials from --kubeconfig, $KUBECONFIG, or ~/.kube/config.
Fresh discovery fills profile settings from the detected hardware and built-in
defaults. With --user-config, discovery replaces only clusterConfig and preserves
all other settings. Explicit CLI overrides apply in both modes.

### Generate Deployment Files
Based on the discovered or provided configuration,
generate a complete set of YAML deployment files for the selected network profile.
Files can be saved to disk using --save-deployment-files.
The profile is defined with --fabric, --deployment-type and --multirail flags,
or via a profile section in the user-config file.

### Deploy to Cluster
Apply the generated deployment files to your Kubernetes cluster by using --deploy. Credentials resolve from --kubeconfig, $KUBECONFIG, or ~/.kube/config. Skip this phase by omitting --deploy.

### AI Agent / Automation Support
Use --output json for a structured result from this root pipeline.
Subcommands have their own output contracts; validate emits a JSON stream.
Use `l8k validate --junit-path ./reports/validation.xml` to also produce
[JUnit results](docs/user/validation.md#junit-xml), with a suite per connectivity
family and a testcase per directional pod/rail probe.
Use --yes to auto-confirm prompts, --quiet to suppress informational output, and --dry-run to preview deployments.
Use 'l8k schema' to discover tool capabilities programmatically.

Usage:
  l8k [flags]
  l8k [command]

Examples:
  # Discover cluster and generate SR-IOV ethernet deployment
  l8k --kubeconfig ~/.kube/config --discover-cluster-config \
    --fabric ethernet --deployment-type sriov --save-deployment-files ./output

  # Generate from saved config (no cluster access needed)
  l8k --user-config cluster-config.yaml --fabric ethernet \
    --deployment-type sriov --save-deployment-files ./output

  # Discover + deploy Spectrum-X with JSON output for automation
  l8k --kubeconfig ~/.kube/config --discover-cluster-config \
    --spectrum-x RA2.3 --multiplane-mode hwplb --number-of-planes 4 \
    --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
    --network-operator-release 26.7 --deploy --output json --yes

  # Dry-run: preview what would be deployed
  l8k --user-config cluster-config.yaml --fabric ethernet \
    --deployment-type sriov --deploy --dry-run --output json

  # Get tool capabilities as JSON (for AI agents)
  l8k schema

Available Commands:
  clean       Remove a Network Operator deployment from a Kubernetes cluster
  completion  Generate the autocompletion script for the specified shell
  deploy      Apply previously generated manifests to a Kubernetes cluster
  discover    Discover cluster network hardware capabilities
  generate    Generate deployment manifests for a network profile
  help        Help about any command
  preset      Manage predefined cluster configuration presets
  schema      Print tool capabilities as JSON (for AI agents and automation)
  sosreport   Collect diagnostic sosreport from a Kubernetes cluster
  validate    Verify a deployment matches the selected Network Operator release
  version     Print the version number

Target Selection Flags:
      --target string   Deployment target: host (default) or dpf (reserved; phases are unavailable until the DPF driver is added) (default "host")

Host Target Common Flags:
      --config-dir string                   Directory containing optional l8k-config.yaml and presets/ overrides
      --enabled-plugins string              Comma-separated list of plugins to enable (default "network-operator")
      --flavor string                       Cluster flavor: k8s or ocp (overrides config)
      --image-pull-secrets strings          Image pull secret names for Network Operator components and authenticated Helm downloads (comma-separated)
      --kubeconfig string                   Path to kubeconfig file for cluster deployment (required when using --deploy; falls back to $KUBECONFIG, then ~/.kube/config)
      --network-operator-namespace string   Override the Network Operator namespace from the config file
      --network-operator-release string     Network Operator release line to deploy (MAJOR.MINOR); selects catalog-managed component versions and repositories
      --node-selector string                Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and discoverable NICs are detected via sysfs; restricted BlueFields are excluded (default "feature.node.kubernetes.io/pci-15b3.present=true")
      --skip-network-operator-helm          Skip Network Operator Helm values generation, chart installation, and Helm-specific validation
      --user-config string                  Use provided cluster configuration file (as base config for discovery or as full config without discovery)

Host Target Discovery Flags:
      --collapse-nic-rails           Advertise one rail per NIC: collapse a NIC's multi-plane PFs to its master PF, keeping a rail per port only for NICs whose VPD model is genuinely dual-port ("2-port"/"Dual-port"). Set to false to keep the legacy one-rail-per-PF behaviour (dev setups). (default true)
      --discover-cluster-config      Bootstrap a private NIC Configuration Daemon to discover cluster capabilities
      --save-cluster-config string   Save discovered cluster configuration to the specified path (defaults to --user-config path if set, otherwise ./cluster-config.yaml)

Host Target Profile Selection Flags:
      --deployment-type string   Deployment type: sriov, rdma_shared, or host_device
      --fabric string            Fabric type: ethernet or infiniband
      --for string               Generate for a known server preset (replaces clusterConfig from the preset). Requires --node-selector. Run 'l8k preset list' with the same --config-dir to list available names.
      --gpu-type string          Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.
      --groups strings           Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.
      --ignore-arp               Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails
      --multirail                Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)
      --routing string           Secondary-network routing mode: destination-based or source-based
      --spectrum-x string        Enable Spectrum-X by passing the SPC-X RA version

Host Target Spectrum-X Flags:
      --ip-version string                  Spectrum-X address family: ipv4 or ipv6
      --multiplane-mode string             Spectrum-X multiplane mode: none, swplb, or hwplb
      --number-of-planes int               Spectrum-X plane count: 1, 2, or 4
      --spectrum-x-config string           Path to a full Spectrum-X profile ConfigMap or raw data.profile YAML
      --spectrum-x-configmap-name string   ConfigMap name used when --spectrum-x-config contains raw data.profile YAML
      --topology-file string               Path to a Spectrum-X reference-generator or NVIDIA AIR topology JSON file
      --topology-scheme string             Spectrum-X topology scheme: 2-tier or 3-tier

Host Target Generation Output Flags:
      --enable-doca-driver             Enable or disable DOCA driver deployment
      --network-namespaces strings     Namespaces for secondary-network resources and example workloads (comma-separated)
      --save-deployment-files string   Save generated deployment files to the specified directory (default "./deployment")
      --workload-manifest string       Path to a custom workload manifest YAML

Target-Agnostic Execution Flags:
      --deploy                    Deploy the generated files to the Kubernetes cluster
      --deploy-timeout duration   Maximum end-to-end wall-clock budget for the deploy phase (e.g. 45m, 2h). 0 (the default) means no deadline; the deploy polls until every manifest reaches a terminal state.
      --dry-run                   Preview what would be deployed without applying changes to the cluster

Target-Agnostic Output & Logging Flags:
  -h, --help               help for l8k
      --log-file string    Write logs to file instead of stderr
      --log-level string   Enable logging at specified level (trace, debug, info, warn, error)
      --output string      Output format: text (default, human-readable) or json (structured, for automation and AI agents) (default "text")
  -q, --quiet              Suppress informational output (errors still shown)
  -y, --yes                Auto-confirm all prompts without interactive input

Use "l8k [command] --help" for more information about a command.
```

<!-- END HELP -->

</details>

## Usage Examples

<span id="generate-deployment-files-without-cluster-access-for"></span>
<span id="remove-a-network-operator-deployment"></span>
<span id="troubleshooting-network-operator-issues"></span>

Follow the [first deployment](docs/user/quick-start.md), [OpenShift](docs/user/openshift.md), or [Spectrum-X](docs/user/spectrum-x.md) walkthrough. For [preset-only generation](docs/user/presets.md), [mixed hardware](docs/user/heterogeneous-clusters.md), [automation](docs/integrator/automation.md), and [removal](docs/user/cleanup.md), use their task guides.

## Configuration file

<span id="ofed-dependent-module-handling"></span>
<span id="nv-ipam-subnet-configuration"></span>
<span id="custom-workload-manifest"></span>

The [configuration reference](docs/reference/configuration.md) owns fields, defaults, precedence, and path lookup. The [maintenance guide](docs/user/maintenance.md) explains upgrade limits and change procedures.

## Docker container

Use the [container installation example](docs/user/installation.md#container). Its kubeconfig and working-directory mounts must match the host environment.

## OpenShift

Use the [OpenShift deployment guide](docs/user/openshift.md) for externally installed operators, flavor-specific resources, validation permissions, and qualification limits.

## Development

Read [AGENTS.md](AGENTS.md) and [CONTRIBUTING.md](CONTRIBUTING.md) before editing. The [architecture guide](docs/architecture/overview.md) covers the component map and Go discovery-library entry point. [Documentation publishing](docs/reference/docs-publishing.md) gives the build and consistency checks. The [extension contracts](openspec/README.md) cover shared behavior and conformance scenarios for new configuration, probes, profiles, resource kinds, and checks.

### Library API

See the [Go discovery-library integration](docs/architecture/overview.md#go-discovery-library) for the Helm-free public entry point, release and preset options, and logging/concurrency considerations.

## Repository Automation

Contributor automation and review workflow live in [AGENTS.md](AGENTS.md) and [CONTRIBUTING.md](CONTRIBUTING.md).
