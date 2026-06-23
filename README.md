# K8s Launch Kit - CLI for configuring NVIDIA cloud-native solutions

K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

## Operation Phases

### Discover Cluster Configuration
Bootstrap a private NIC Configuration Daemon into the `nvidia-k8s-launch-kit`
namespace to discover your cluster's network capabilities and hardware
configuration. Discovery does **not** require a pre-installed Network Operator —
the daemon and its CRDs are created in a dedicated namespace, used to publish
`NicDevice` CRs, and torn down when discovery finishes. This phase can be
skipped if you provide your own configuration file.

### Select the Deployment Profile
Specify the desired deployment profile via CLI flags (`--fabric`, `--deployment-type`, `--multirail`, `--spectrum-x`) or via a `profile` section in the user-config file. AI-driven profile selection now lives in the `k8s-launch-kit-*` Claude Code skills, which wrap the deterministic CLI commands.

### Generate Deployment Files
Based on the discovered/provided configuration, generate a complete set of YAML deployment files tailored to your selected network profile.

## Installation

### Quick install (from GitHub Releases)

```bash
curl -fsSL https://raw.githubusercontent.com/nvidia/k8s-launch-kit/main/scripts/install.sh | sh
```

Pin a specific version or install to a custom directory:

```bash
L8K_VERSION=v1.0.0 sh scripts/install.sh
curl -fsSL ... | sh -s -- -d ~/local
```

Uninstall:

```bash
curl -fsSL https://raw.githubusercontent.com/nvidia/k8s-launch-kit/main/scripts/install.sh | sh -s -- --uninstall
```

### Homebrew

```bash
brew tap nvidia/l8k https://github.com/nvidia/k8s-launch-kit
brew install l8k
```

### Build from source

```bash
git clone <repository-url>
cd k8s-launch-kit
make build
```

The binary will be available at `build/l8k`.

After building, install the binary, profiles, and config to `/usr/local`:

```bash
make install        # Copies binary, profiles, config to /usr/local
make dev-install    # Symlinks instead of copies (for development)
```

This runs `scripts/install-local.sh`, which places:
- `<prefix>/bin/l8k`
- `<prefix>/share/l8k/profiles/`
- `<prefix>/share/l8k/presets/`
- `<prefix>/share/l8k/l8k-config.yaml`

Default prefix is `/usr/local`. Override with `PREFIX=/opt/l8k make install`.

### Docker

```bash
make docker-build          # Build Docker image (l8k:v0.1.0 + l8k:latest)
make docker-build-local    # Build inside container, extract binary to host build/l8k
```

`docker-build-local` is useful when you don't have the Go toolchain installed — it compiles inside a container and copies the resulting binary to `build/l8k` on your host.

```bash
# Run from the Docker image
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v $(pwd):/output \
  l8k:latest discover --kubeconfig /kube/config \
    --save-cluster-config /output/cluster-config.yaml
```

## Usage

<!-- BEGIN HELP -->
<!-- This section is automatically updated by running 'make update-readme' -->

```

K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

### Discover Cluster Configuration
Deploy a minimal Network Operator profile to automatically discover your cluster's
network capabilities and hardware configuration by using --discover-cluster-config.
This phase can be skipped if you provide your own configuration file by using --user-config.
This phase requires --kubeconfig to be specified.

### Generate Deployment Files
Based on the discovered or provided configuration,
generate a complete set of YAML deployment files for the selected network profile.
Files can be saved to disk using --save-deployment-files.
The profile is defined with --fabric, --deployment-type and --multirail flags,
or via a profile section in the user-config file.

### Deploy to Cluster
Apply the generated deployment files to your Kubernetes cluster by using --deploy. This phase requires --kubeconfig and can be skipped if --deploy is not specified.

The deploy step installs (or upgrades) the `nvidia/network-operator` Helm chart in-process before applying the post-install CRs. The chart version and Helm repository URL are taken from the embedded release catalog and can be selected via `--network-operator-release <MAJOR.MINOR>`. Each profile renders a per-profile `values.yaml` next to the CR manifests; `l8k deploy` reads that file and runs the install. When a release already exists with different values, deploy fails fast — pass `--overwrite-existing` to promote to `helm upgrade --install`.

### AI Agent / Automation Support
Use --output json for structured machine-readable output (single JSON object to stdout).
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
    --spectrum-x RA2.2 --multiplane-mode hwplb --number-of-planes 4 --network-operator-release 26.4 --deploy --output json --yes

  # Dry-run: preview what would be deployed
  l8k --user-config cluster-config.yaml --spectrum-x --deploy \
    --dry-run --output json

  # Get tool capabilities as JSON (for AI agents)
  l8k schema

Available Commands:
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

Common Flags:
      --enabled-plugins string              Comma-separated list of plugins to enable (default "network-operator")
      --image-pull-secrets strings          Image pull secret names for NicClusterPolicy (comma-separated)
      --kubeconfig string                   Path to kubeconfig file for cluster deployment (required when using --deploy; falls back to $KUBECONFIG, then ~/.kube/config)
      --network-operator-namespace string   Override the network operator namespace from the config file
      --network-operator-release string     Network Operator release line to deploy (MAJOR.MINOR). Selects component image tags + repository from a built-in catalog and drives version-gated template sections. Supported: 25.10, 26.1, 26.4
      --node-selector string                Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe (default "feature.node.kubernetes.io/pci-15b3.present=true")
      --user-config string                  Use provided cluster configuration file (as base config for discovery or as full config without discovery)

Discovery Flags:
      --discover-cluster-config      Deploy a thin Network Operator profile to discover cluster capabilities
      --save-cluster-config string   Save discovered cluster configuration to the specified path (defaults to --user-config path if set, otherwise ./cluster-config.yaml)

Profile Selection Flags:
      --deployment-type string   Select the deployment type (sriov, rdma_shared, host_device)
      --fabric string            Select the fabric type to deploy (infiniband, ethernet)
      --for string               Generate for a known server preset (replaces clusterConfig from the preset). Requires --node-selector. Available: PowerEdge-R760xa-H100-NVL, PowerEdge-XE7745-RTX-PRO-4500, PowerEdge-XE9680-H200, ThinkSystem-SR650-V4-RTX-PRO-6000, ThinkSystem-SR675-V3-H200-NVL, ThinkSystem-SR675-V3-RTX-PRO-6000, ThinkSystem-SR680a-V3-H200, UCSC-885A-M8-H22-H200
      --gpu-type string          Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.
      --groups strings           Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.
      --multirail                Enable multirail deployment
      --spectrum-x string        Enable Spectrum-X by passing the SPC-X RA version (folds in the legacy --spcx-version). Supported: [RA2.1 RA2.2]

Spectrum-X Flags:
      --multiplane-mode string   Spectrum-X multiplane mode: swplb, hwplb, uniplane (requires --spectrum-x)
      --number-of-planes int     Number of planes for Spectrum-X (requires --spectrum-x)

Generation Output Flags:
      --enable-doca-driver             Enable DOCA driver deployment (overrides config file docaDriver.enable)
      --pod-namespace string           Namespace for pods and network resources (overrides config podNamespace, default: 'default')
      --save-deployment-files string   Save generated deployment files to the specified directory (default "./deployment")
      --workload-manifest string       Path to a custom workload manifest YAML (replaces the profile's default example workload)

Deploy Flags:
      --deploy                    Deploy the generated files to the Kubernetes cluster
      --deploy-timeout duration   Maximum end-to-end wall-clock budget for the deploy phase (e.g. 45m, 2h). 0 (the default) means no deadline; the deploy polls until every manifest reaches a terminal state.
      --dry-run                   Preview what would be deployed without applying changes to the cluster

Output & Logging Flags:
  -h, --help               help for l8k
      --log-file string    Write logs to file instead of stderr
      --log-level string   Enable logging at specified level (debug, info, warn, error)
      --output string      Output format: text (default, human-readable) or json (structured, for automation and AI agents) (default "text")
  -q, --quiet              Suppress informational output (errors still shown)
  -y, --yes                Auto-confirm all prompts without interactive input

Use "l8k [command] --help" for more information about a command.
```

<!-- END HELP -->

> **Note:** The help text above is auto-generated. Run `make update-readme` after CLI changes to refresh it.

## Usage Examples

### Subcommand Workflow (Recommended)

Discover cluster hardware:

```bash
l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./cluster-config.yaml
```

Generate deployment manifests:

```bash
l8k generate --user-config ./cluster-config.yaml \
    --fabric ethernet --deployment-type sriov --multirail \
    --save-deployment-files ./deployments
```

Apply the generated manifests to the cluster:

```bash
l8k deploy --deployment-files ./deployments --kubeconfig ~/.kube/config
```

`l8k deploy` reads YAML from `--deployment-files` (default `./deployment`)
and applies it in four phases: NicClusterPolicy first (await ready), per-group
NicNodePolicy (await each), all remaining CRs in one batch (controllers
reconcile concurrently), then verify every manifest reached a terminal state.
Example workload manifests (`*example*`) are **not** applied by `l8k deploy` —
they're fixtures consumed by `l8k validate --connectivity` or
`l8k deploy --test-connectivity` for the data-plane phase. It auto-prefers
`<dir>/network-operator/` (the layout `l8k generate` produces) and falls back
to `<dir>` itself. `--dry-run` does a server-side dry run. `--deploy-timeout`
caps the whole apply+reconcile phase end-to-end (e.g. `--deploy-timeout 90m`);
without it, deploy polls indefinitely — right for SR-IOV on large clusters
where reconciliation can take an hour. `--test-connectivity` chains the
connectivity matrix straight after a successful apply.

Verify the deployment end-to-end:

```bash
l8k validate --user-config ./cluster-config.yaml \
    --deployment-files ./deployments \
    --kubeconfig ~/.kube/config
```

`l8k validate` runs three checks back-to-back: (1) the Network Operator Helm
chart's appVersion matches the version expected by
`networkOperator.selectedRelease` in `cluster-config.yaml`; (2) every YAML
manifest under `--deployment-files` is classified against the live cluster
as `READY` / `IN-PROGRESS` / `ERROR` / `MISSING` via the per-Kind validator
registry (with SR-IOV silent-failure detection, NicConfigurationTemplate
condition-Reason classification, NicClusterPolicy appliedStates breakdown,
etc.); and (3) a data-plane connectivity matrix — apply the example
DaemonSet, wait for it to roll out completely (`numberReady ==
desiredNumberScheduled > 0` — a single ContainerCreating-stuck pod fails),
and run `ping -c N -I <srcIP> <dstIP>` across every rail and pod pair plus
a per-pair cross-rail canary. The matrix is on by default
(`--connectivity=false` to skip), runs concurrent pings capped at 16, and
cleans up the test DaemonSet unless `--keep` is set.

A self-contained HTML report lands at `<deployment-files>/k8s-launch-kit-validation-report.html`
by default (override with `--report-path`, disable with `--report-path=-`).
The report has: header (l8k version, kubeconfig context, API-server version),
profile, **Node groups** (per-`clusterConfig[]` entry with east-west / north-
south PF tables — PCI, deviceID, rail, netdev, RDMA device, PSID, part #,
NUMA, connected GPU), cluster nodes, Network Operator release, **Manifest
state** (with expandable Details + **Live YAML** dropdowns per row),
connectivity matrix (per-rail src×dst grids + cross-rail canary), and a
warnings rollup. Styled after the NVIDIA AICR documentation light theme;
no JS, no external assets.

Exits 4 on any missing/error manifest, version mismatch, or connectivity
failure. `IN-PROGRESS` exits 0 with a warning so CI can re-run later (or
pass `--wait <duration>` to block).

Collect a diagnostic dump:

```bash
l8k sosreport --kubeconfig ~/.kube/config
```

### Complete Workflow (Root Command)

The root command still supports all flags for backward compatibility and running the full pipeline in one shot:

```bash
l8k --discover-cluster-config --save-cluster-config ./cluster-config.yaml \
    --fabric ethernet --deployment-type sriov --multirail \
    --save-deployment-files ./deployments \
    --deploy --kubeconfig ~/.kube/config
```

### Discover Cluster Configuration

Using the subcommand:

```bash
l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./my-cluster-config.yaml
```

Filter discovery to specific nodes using a label selector:

```bash
l8k discover --kubeconfig ~/.kube/config \
    --save-cluster-config ./my-cluster-config.yaml \
    --node-selector "feature.node.kubernetes.io/pci-15b3.present=true"
```

Or using the root command (backward compatible):

```bash
l8k --discover-cluster-config --save-cluster-config ./my-cluster-config.yaml \
    --kubeconfig ~/.kube/config
```

### Discovery with User-Provided Base Config

Use your own config file (with custom network operator version, subnets, etc.) as the base for discovery. Without `--save-cluster-config`, the file is rewritten in place with discovery results:

```bash
l8k discover --user-config ./my-config.yaml \
    --kubeconfig ~/.kube/config
```

Save discovery results to a separate file instead:

```bash
l8k discover --user-config ./my-config.yaml \
    --save-cluster-config ./discovered-config.yaml \
    --kubeconfig ~/.kube/config
```

### Use Existing Configuration

Generate and deploy with pre-existing config:

```bash
l8k generate --user-config ./existing-config.yaml \
    --fabric ethernet --deployment-type sriov --multirail \
    --save-deployment-files ./deployments \
    --deploy --kubeconfig ~/.kube/config
```

### Generate Deployment Files

```bash
l8k generate --user-config ./config.yaml \
    --fabric ethernet --deployment-type sriov --multirail \
    --save-deployment-files ./deployments
```

### Generate Deployment Files for a Specific Node Group

In heterogeneous clusters, discovery produces multiple node groups. Use `--group` to generate manifests for a single group:

```bash
l8k generate --user-config ./config.yaml \
    --fabric infiniband --deployment-type sriov --multirail \
    --group group-0 \
    --save-deployment-files ./deployments
```

### Generate Deployment Files Without Cluster Access (`--for`)

When you have a known server SKU, use `--for <preset-name>` to skip cluster discovery and synthesize the `clusterConfig` from a topology preset. List available presets with `l8k preset list`. The `--node-selector` flag is required since the synthesized clusterConfig has no live worker-node list:

```bash
# List available presets (each shows machineType + gpuType)
l8k preset list

# Generate from a known SKU (no kubeconfig needed)
l8k generate --user-config ./config.yaml \
    --for ThinkSystem-SR680a-V3 \
    --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./deployments
```

The preset YAML must declare a `capabilities.nodes.{sriov,rdma,ib}` block to be usable with `--for`; presets shipped with l8k already have one. See [docs/presets.rst](docs/presets.rst) for the full preset format and how to add new ones.

### Troubleshooting Network Operator Issues

Collect a diagnostic dump from the cluster:

```bash
l8k sosreport --kubeconfig ~/.kube/config --output-dir ./sosreport
```

The sosreport contains NicClusterPolicy, pod logs, node info, CRDs, and other diagnostic data. For interactive AI-assisted analysis, use the bundled Claude Code skills under `skills/k8s-launch-kit-troubleshoot/` — they wrap the deterministic commands (`l8k sosreport`, `kubectl`) and let the agent driving the skill do the reasoning.

### AI Agent / Automation Usage

l8k supports structured output for AI agents and CI/CD pipelines. Use `--output json` to get machine-readable output, `--yes` to skip interactive prompts, and `--dry-run` to preview changes safely.

#### Structured JSON Output

```bash
# Get structured output for programmatic consumption
l8k generate --user-config ./config.yaml \
    --fabric ethernet --deployment-type sriov --multirail \
    --save-deployment-files ./deployments \
    --output json --yes 2>/dev/null | jq .
```

Example JSON output:
```json
{
  "success": true,
  "phase": "generate",
  "profile": {
    "fabric": "ethernet",
    "deployment": "sriov",
    "multirail": "true"
  },
  "generatedFiles": [
    "./deployments/network-operator/nic-cluster-policy.yaml",
    "./deployments/network-operator/sriov-network-node-policy.yaml"
  ],
  "deployed": false,
  "messages": [
    {"level": "info", "message": "Generating files for profile: SR-IOV Ethernet RDMA", "timestamp": "..."}
  ]
}
```

#### Dry-Run Preview

Preview what would be deployed without making changes:

```bash
l8k generate --user-config ./config.yaml --spectrum-x --deploy \
    --dry-run --output json --kubeconfig ~/.kube/config
```

#### Schema Discovery

AI agents can programmatically discover l8k's capabilities:

```bash
l8k schema
```

This outputs a JSON description of available phases, fabrics, deployment types, flags, exit codes, and output formats.

#### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | General error |
| 2 | Validation error (bad flags, invalid config) |
| 3 | Cluster error (API unreachable, discovery failed) |
| 4 | Deployment error (apply failed) |
| 5 | Partial success (discovery ok but deploy failed) |

In JSON mode, errors include structured fields (`code`, `category`, `transient`, `suggestion`) to help agents decide whether to retry or fix input.

## Configuration file

During cluster discovery stage, Kubernetes Launch Kit creates a configuration file, which it later uses to generate deployment manifests from the templates. This config file can be edited by the user to customize their deployment configuration. The user can provide the custom config file to the tool using the `--user-config` cli flag — either as a standalone config (skipping discovery) or as a base config combined with `l8k discover` / `--discover-cluster-config` (discovery takes network operator parameters from the file and adds discovered cluster config).

The tool resolves configuration and profile paths in order: local directory first (`./l8k-config.yaml`, `./profiles`), then installed location (`/usr/local/share/l8k/`), then binary-relative.

### Network Operator release selection

Use `--network-operator-release <MAJOR.MINOR>` (or `networkOperator.selectedRelease` in the config file) to pick a Network Operator release line by name instead of hand-editing image tags. Supported releases live in an embedded catalog ([pkg/networkoperatorplugin/releases.yaml](pkg/networkoperatorplugin/releases.yaml)); each entry maps a release key to image tags + repository for the operator and DOCA driver. Selecting a release populates `networkOperator.{version,componentVersion,repository}` and `docaDriver.version` from the catalog — explicit values in `l8k-config.yaml` are overridden when a release is set.

```bash
# Pick a release on the CLI
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --network-operator-release 26.4 \
  --save-deployment-files ./output

# Equivalent via config file
# networkOperator:
#   selectedRelease: "26.4"

# Discover supported releases
l8k schema | jq '.supportedNetworkOperatorReleases'
```

The release identifier is also used to gate version-specific template sections. **NicNodePolicy** is rendered only for `26.4+`; under older releases the OFED driver and the appropriate device plugin (`rdmaSharedDevicePlugin` for ipoib/macvlan, `sriovDevicePlugin` for host-device) are emitted in `NicClusterPolicy` instead, matching the legacy 26.1 model.

There are two **Spectrum-X** profiles, picked by the value of `--spectrum-x`:

- **`spectrum-x`** — RA2.2 on `26.4+`. Uses the v1alpha2 `SpectrumXRailPoolConfig` with `railTopology[]` to consolidate rail wiring. Selected for `--spectrum-x RA2.2`.
- **`spectrum-x-ra2.1`** — RA2.1 on `26.1` only (pinned via `min`/`maxNetworkOperatorRelease: "26.1"`). Renders the full SR-IOV operator chain: per-group `SriovNetworkPoolConfig` + per-rail `SriovNetworkNodePolicy` + `OVSNetwork` + nv-ipam `CIDRPool` + a v1alpha1 glue `SpectrumXRailPoolConfig`. Selected for `--spectrum-x RA2.1`.

`--network-operator-release` must be passed explicitly with `--spectrum-x` — the release line is consequential (it picks the CRD shape and the SR-IOV operator behaviour), so we don't silently fill it in. The pair is then validated: `--spectrum-x RA2.1 --network-operator-release 26.4` errors out with a specific "RA2.1 requires --network-operator-release in [26.1]" message rather than a generic "no applicable profile found".

When neither the flag nor `selectedRelease` is set, behavior is unchanged: explicit values in the config file flow through and templates render the newest gates (treated as "latest").

Adding a new release is a YAML-only change in `releases.yaml` — patch bumps update an existing entry in place; new minor lines add a new top-level key.

### DOCA Driver

The `docaDriver` section controls the OFED driver deployment in the NicClusterPolicy. Set `enable: true` to include the `ofedDriver` section in generated manifests, or `enable: false` to omit it. This can also be overridden via the `--enable-doca-driver` CLI flag.

#### OFED-Dependent Module Handling

When the DOCA/OFED driver loads on a node, it replaces the inbox MLX kernel modules (`mlx5_core`, `mlx5_ib`, `ib_core`, etc.) with its own versions. If other kernel modules depend on the inbox MLX modules, they will block the inbox modules from being unloaded, causing the DOCA driver to fail to load.

During cluster discovery, the tool execs into `nic-configuration-daemon` pods and builds a full reverse dependency graph from `/sys/module/*/holders/` for all loaded modules, then BFS-traverses from each of the following MLX/OFED kernel modules to find all transitive non-MOFED dependents:

`mlx5_core`, `mlx5_ib`, `ib_umad`, `ib_uverbs`, `ib_ipoib`, `rdma_cm`, `rdma_ucm`, `ib_core`, `ib_cm`

Discovered modules are classified into three categories:

1. **mlx5-prefixed modules** (e.g. `mlx5_vdpa`, `mlx5_netdev`) — NVIDIA's own modules, silently filtered out.
2. **Known storage-over-RDMA modules** (`ib_isert`, `nvme_rdma`, `nvmet_rdma`, `rpcrdma`, `xprtrdma`, `ib_srpt`) — saved per-group as `storageModules`. Discovery **automatically enables** `docaDriver.unloadStorageModules: true` when any are found. The generated NicClusterPolicy renders `UNLOAD_STORAGE_MODULES: "true"`.
3. **Third-party RDMA modules** (everything else, e.g. `qedr`, `bnxt_re`, `rdma_rxe`) — saved per-group as `thirdPartyRDMAModules`. Discovery **automatically enables** `docaDriver.unloadThirdPartyRDMAModules: true` when any are found. The generated NicClusterPolicy renders `UNLOAD_THIRD_PARTY_RDMA_MODULES: "true"`. The driver container has 15 known third-party modules hardcoded.

Both flags are auto-enabled during discovery so the DOCA driver can unload blocking modules. A warning is emitted after discovery and generation reminding you to verify that no running workloads depend on these modules. When multiple node groups are merged, both module lists are aggregated as unions.

After discovery, the config will contain the discovered modules and auto-enabled flags:
```yaml
docaDriver:
  enable: true
  version: doca3.3.0-26.01-1.0.0.0-0
  unloadStorageModules: true            # auto-enabled by discovery
  enableNFSRDMA: false
  unloadThirdPartyRDMAModules: true     # auto-enabled by discovery

clusterConfig:
- identifier: group-0
  thirdPartyRDMAModules:
  - rdma_rxe
  storageModules:
  - nvme_rdma
  - ib_isert
```

The generated NicClusterPolicy `ofedDriver` section will include:
```yaml
env:
  - name: UNLOAD_STORAGE_MODULES
    value: "true"
  - name: UNLOAD_THIRD_PARTY_RDMA_MODULES
    value: "true"
```

To disable automatic unloading, set the flags back to `false` in your config after discovery.

### NV-IPAM Subnet Configuration

The `nvIpam` section supports two modes for subnet configuration:

**Option 1: Manual subnet list** — List each subnet explicitly. This takes precedence if the list is non-empty:
```yaml
nvIpam:
  poolName: nv-ipam-pool
  subnets:
  - subnet: 192.168.2.0/24
    gateway: 192.168.2.1
  - subnet: 192.168.3.0/24
    gateway: 192.168.3.1
```

**Option 2: Auto-generate subnets** — When the `subnets` list is empty but `startingSubnet`, `mask`, and `offset` are all set, subnets are automatically generated. Each cluster config group gets its own unique, non-overlapping subnet slice. The gateway for each subnet is the first usable address (network + 1).
```yaml
nvIpam:
  poolName: nv-ipam-pool
  startingSubnet: "192.168.2.0"
  mask: 24
  offset: 1
```

With the auto-generation example above, a cluster with 2 groups (4 east-west PFs each) would receive:
- Group 0: 192.168.2.0/24, 192.168.3.0/24, 192.168.4.0/24, 192.168.5.0/24
- Group 1: 192.168.6.0/24, 192.168.7.0/24, 192.168.8.0/24, 192.168.9.0/24

The `offset` parameter controls how many subnet blocks to skip between consecutive subnets (offset=1 is contiguous, offset=2 skips every other).

Example of the configuration file discovered from the cluster:

```yaml
networkOperator:
  version: v26.1.0
  componentVersion: network-operator-v26.1.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
  imagePullSecrets: []
docaDriver:
  enable: true
  version: doca3.2.0-25.10-1.2.8.0-2
  unloadStorageModules: false
  enableNFSRDMA: false
  unloadThirdPartyRDMAModules: false
nvIpam:
  poolName: nv-ipam-pool
  subnets:
  - subnet: 192.168.2.0/24
    gateway: 192.168.2.1
  - subnet: 192.168.3.0/24
    gateway: 192.168.3.1
  - subnet: 192.168.4.0/24
    gateway: 192.168.4.1
  - subnet: 192.168.5.0/24
    gateway: 192.168.5.1
  - subnet: 192.168.6.0/24
    gateway: 192.168.6.1
  - subnet: 192.168.7.0/24
    gateway: 192.168.7.1
  - subnet: 192.168.8.0/24
    gateway: 192.168.8.1
  - subnet: 192.168.9.0/24
    gateway: 192.168.9.1
  - subnet: 192.168.10.0/24
    gateway: 192.168.10.1
  - subnet: 192.168.11.0/24
    gateway: 192.168.11.1
  - subnet: 192.168.12.0/24
    gateway: 192.168.12.1
  - subnet: 192.168.13.0/24
    gateway: 192.168.13.1
  - subnet: 192.168.14.0/24
    gateway: 192.168.14.1
  - subnet: 192.168.15.0/24
    gateway: 192.168.15.1
  - subnet: 192.168.16.0/24
    gateway: 192.168.16.1
  - subnet: 192.168.17.0/24
    gateway: 192.168.17.1
  - subnet: 192.168.18.0/24
    gateway: 192.168.18.1
  - subnet: 192.168.19.0/24
sriov:
  ethernetMtu: 9000
  infinibandMtu: 4000
  numVfs: 8
  priority: 90
  resourceName: sriov_resource
  networkName: sriov-network
hostdev:
  resourceName: hostdev-resource
  networkName: hostdev-network
rdmaShared:
  resourceName: rdma_shared_resource
  hcaMax: 63
ipoib:
  networkName: ipoib-network
macvlan:
  networkName: macvlan-network
nicConfigurationOperator:
  deployNicInterfaceNameTemplate: true  # Enable NIC rename when needed (see NIC Interface Name Templates section)
  rdmaPrefix: "rdma_r%rail%"           # RDMA device name template (%rail% substituted per rail)
  netdevPrefix: "eth_r%rail%"          # Network interface name template (%rail% substituted per rail)
spectrumX:
  nicType: "1023"
  overlay: none
  rdmaPrefix: roce_p%plane%_r%rail%    # Spectrum-X uses its own prefixes (with %plane%)
  netdevPrefix: eth_p%plane%_r%rail%
clusterConfig:
- identifier: group-0
  capabilities:
    nodes:
      sriov: true
      rdma: true
      ib: true
  pfs:
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:19:00.0"
    networkInterface: ""
    traffic: east-west
    rail: 0
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:2a:00.0
    networkInterface: ""
    traffic: east-west
    rail: 1
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:3b:00.0
    networkInterface: ""
    traffic: east-west
    rail: 2
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:4c:00.0
    networkInterface: ""
    traffic: east-west
    rail: 3
  - deviceID: 101f
    rdmaDevice: ""
    pciAddress: 0000:5a:00.0
    networkInterface: ""
    traffic: east-west
    rail: 4
  - deviceID: 101f
    rdmaDevice: ""
    pciAddress: 0000:5a:00.1
    networkInterface: ""
    traffic: east-west
    rail: 5
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:9b:00.0
    networkInterface: ""
    traffic: east-west
    rail: 6
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:ab:00.0
    networkInterface: ""
    traffic: east-west
    rail: 7
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:c1:00.0
    networkInterface: ""
    traffic: east-west
    rail: 8
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:cb:00.0
    networkInterface: ""
    traffic: east-west
    rail: 9
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:d8:00.0
    networkInterface: ""
    traffic: east-west
    rail: 10
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:d8:00.1
    networkInterface: ""
    traffic: east-west
    rail: 11
  workerNodes:
  - pdx-g22r13-2894-lh2-w01
  - pdx-g24r13-2894-lh2-w02
  nodeSelector:
    nvidia.com/gpu.machine: ThinkSystem-SR680a-V3
- identifier: group-1
  capabilities:
    nodes:
      sriov: true
      rdma: true
      ib: true
  pfs:
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:1a:00.0
    networkInterface: ""
    traffic: east-west
    rail: 0
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:3c:00.0
    networkInterface: ""
    traffic: east-west
    rail: 1
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:4d:00.0
    networkInterface: ""
    traffic: east-west
    rail: 2
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:5e:00.0
    networkInterface: ""
    traffic: east-west
    rail: 3
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:9c:00.0
    networkInterface: ""
    traffic: east-west
    rail: 4
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:9d:00.0
    networkInterface: ""
    traffic: east-west
    rail: 5
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:9d:00.1
    networkInterface: ""
    traffic: east-west
    rail: 6
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:bc:00.0
    networkInterface: ""
    traffic: east-west
    rail: 7
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:cc:00.0
    networkInterface: ""
    traffic: east-west
    rail: 8
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:dc:00.0
    networkInterface: ""
    traffic: east-west
    rail: 9
  workerNodes:
  - pdx-g22r23-2894-dh2-w03
  - pdx-g24r23-2894-dh2-w04
  nodeSelector:
    nvidia.com/gpu.machine: PowerEdge-XE9680
- identifier: group-2
  capabilities:
    nodes:
      sriov: true
      rdma: true
      ib: true
  pfs:
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:09:00.0"
    networkInterface: ""
    traffic: east-west
    rail: 0
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:23:00.0"
    networkInterface: ""
    traffic: east-west
    rail: 1
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:35:00.0"
    networkInterface: ""
    traffic: east-west
    rail: 2
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:35:00.1"
    networkInterface: ""
    traffic: east-west
    rail: 3
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: "0000:53:00.0"
    networkInterface: ""
    traffic: east-west
    rail: 4
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:69:00.0
    networkInterface: ""
    traffic: east-west
    rail: 5
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:8f:00.0
    networkInterface: ""
    traffic: east-west
    rail: 6
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:9c:00.0
    networkInterface: ""
    traffic: east-west
    rail: 7
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:cd:00.0
    networkInterface: ""
    traffic: east-west
    rail: 8
  - deviceID: a2dc
    rdmaDevice: ""
    pciAddress: 0000:f1:00.0
    networkInterface: ""
    traffic: east-west
    rail: 9
  workerNodes:
  - pdx-g22r31-2894-ch2-w05
  - pdx-g24r31-2894-ch2-w06
  nodeSelector:
    nvidia.com/gpu.machine: UCSC-885A-M8-H22
```

### North-South Traffic Detection

During cluster discovery, the tool automatically identifies BlueField DPU devices (as opposed to SuperNICs or ConnectX NICs) by matching each device's `partNumber` against a known list of DPU product codes in [pkg/networkoperatorplugin/ns-product-ids](pkg/networkoperatorplugin/ns-product-ids). Devices matching a DPU product code are classified as **north-south** traffic (management/external), while all other devices are classified as **east-west** traffic (GPU interconnect).

North-south PFs are included in the saved cluster configuration for visibility, but are **automatically filtered out** during template rendering so that only east-west PFs appear in the generated manifests. Each east-west PF is assigned a sequential rail number (rail-0, rail-1, rail-2, ...) used for naming resources like SriovNetworkNodePolicy and IPPool entries.

Example of mixed traffic types in the config:
```yaml
clusterConfig:
- identifier: group-0
  pfs:
  - deviceID: a2dc
    pciAddress: "0000:19:00.0"
    traffic: east-west       # SuperNIC — included in manifests
    rail: 0
  - deviceID: a2dc
    pciAddress: "0000:2a:00.0"
    traffic: east-west
    rail: 1
  - deviceID: a2dc
    pciAddress: "0000:3b:00.0"
    traffic: north-south     # BlueField DPU — excluded from manifests
```

### Machine and GPU Product Type

During discovery, each node group's `machineType` and `gpuType` are populated from GPU operator node labels (`nvidia.com/gpu.machine` and `nvidia.com/gpu.product`). When these labels are absent — for example, when the GPU operator is not deployed — the tool falls back to probing hardware directly from a `nic-configuration-daemon` pod on one of the group's nodes:

- **Machine type**: read from `/sys/class/dmi/id/product_name`
- **GPU product type**: parsed from `nvidia-smi -q` output (the first `Product Name` field)

Values are sanitized to match the GPU operator label format (spaces replaced with dashes). If either probe fails (e.g., `nvidia-smi` not installed, DMI not readable), the corresponding field is left empty and discovery continues without error.

Example of discovered hardware types in the config:
```yaml
clusterConfig:
- identifier: group-0
  machineType: ThinkSystem-SR680a-V3
  gpuType: NVIDIA-H100-NVL
  workerNodes:
  - node-1
  - node-2
```

### NIC Interface Name Templates

The `nicConfigurationOperator.deployNicInterfaceNameTemplate` setting controls whether a `NicInterfaceNameTemplate` CR is deployed to rename NIC interfaces to predictable, rail-based names (e.g., `eth_r0`, `eth_r1`). When set to `true`, the tool treats it as "enable when needed" rather than "always enable". The NicInterfaceNameTemplate CR and associated `nicConfigurationOperator` section in NicClusterPolicy are only deployed when one of the following conditions is met:

1. **Merged groups with PCI address conflicts** — When multiple node groups share the same GPU product type and are merged into a single group, but the same PCI address appears at different rail positions across groups. In this case PCI addresses alone cannot identify the correct rail, so interface name templates are used instead.

2. **rdma_shared deployment with empty network interface names** — When the deployment type is `rdma_shared` (macvlan-rdma-shared or ipoib-rdma-shared profiles) and PFs have empty `networkInterface` fields. The `rdmaSharedDevicePlugin` uses `ifNames` selectors that require interface names, so NicInterfaceNameTemplate must be enabled to provide them. This typically happens when discovery finds multiple nodes per group and omits device names for safety.

When neither condition holds, name templates are disabled and the device plugin uses PCI addresses directly, avoiding the overhead of deploying the NIC configuration operator.

### Custom Workload Manifest

By default, l8k generates example workload DaemonSets (file pattern: `*-example-daemonset.yaml`) for each profile. To use your own workload manifest instead, specify it in the config or via CLI flag:

```yaml
workload:
  manifest: /path/to/my-workload.yaml
```

Or via CLI:
```bash
l8k generate --user-config ./config.yaml \
    --workload-manifest /path/to/my-workload.yaml \
    --fabric ethernet --deployment-type sriov \
    --save-deployment-files ./deployments
```

## Docker container

You can run the l8k tool as a docker container:

```bash
docker run -v ~/remote-cluster/:/remote-cluster -v /tmp:/output --net=host nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 --discover-cluster-config --kubeconfig /remote-cluster/kubeconf.yaml --save-cluster-config /output/config.yaml --log-level debug  --save-deployment-files /output --fabric infiniband --deployment-type rdma_shared --multirail
```

Don't forget to enable `--net=host` and mount the necessary directories for input and output files with `-v`.

## Development

### Building

```bash
make build        # Build for current platform
make build-all    # Build for all platforms
make clean        # Clean build artifacts
```

### Testing

```bash
make test         # Run tests
make coverage     # Run tests with coverage
```

### Linting

```bash
make lint         # Run linter
make lint-check   # Install and run linter
```

### Docker

```bash
make docker-build # Build Docker image
make docker-run   # Run Docker container
```
