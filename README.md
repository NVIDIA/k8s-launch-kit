# K8s Launch Kit - CLI for configuring NVIDIA cloud-native solutions

K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

## Operation Phases

### Discover Cluster Configuration
Bootstrap a private NIC Configuration Daemon into the `nvidia-k8s-launch-kit`
namespace to discover your cluster's network capabilities and hardware
configuration. Discovery does **not** require a pre-installed Network Operator —
the daemon and its CRDs are created in a dedicated namespace, used to publish
`NicDevice` CRs, and torn down when discovery finishes. This phase can be
skipped if you provide your own configuration file. Discovery also resolves
missing profile settings and stores the final values in `cluster-config.yaml`.

### Select the Deployment Profile
Discovery fills missing profile values from hardware and built-in defaults.
Values already present under `profile` are preserved, while explicit CLI flags
(`--fabric`, `--deployment-type`, `--multirail`, `--routing`, `--ignore-arp`,
`--spectrum-x`) take precedence.
AI-driven profile selection now lives in the `k8s-launch-kit-*` Claude Code
skills, which wrap the deterministic CLI commands.

### Generate Deployment Files
Based on the discovered/provided configuration, generate a complete set of YAML deployment files tailored to your selected network profile.
When generation uses a file-backed config, resolved defaults and CLI overrides
are written back to that same file while its comments are preserved.

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

After building, install the binary and profile templates to `/usr/local`:

```bash
make install        # Copies binary and profiles to /usr/local
make dev-install    # Symlinks instead of copies (for development)
```

This runs `scripts/install-local.sh`, which places:
- `<prefix>/bin/l8k`
- `<prefix>/share/l8k/profiles/`

Default prefix is `/usr/local`. Override with `PREFIX=/opt/l8k make install`.

The default `l8k-config.yaml` and topology presets are embedded in the binary;
they are not copied into the installation prefix.

### Override Embedded Configuration

Use the persistent `--config-dir` flag to replace either embedded asset from
the filesystem:

```text
/etc/l8k/
├── l8k-config.yaml       # optional full replacement for the embedded default
└── presets/              # optional authoritative preset catalog
    └── <name>/topology.yaml
```

```bash
# Discover using both overrides
l8k discover --config-dir /etc/l8k --kubeconfig ~/.kube/config

# Inspect the selected preset catalog
l8k preset list --config-dir /etc/l8k
```

`--user-config <file>` has higher precedence than
`--config-dir/l8k-config.yaml`; `--config-dir` still selects the preset
catalog. If the directory provides only one asset, the other falls back to the
embedded copy. A filesystem `presets/` directory replaces the complete
embedded catalog rather than merging with it. Without `--config-dir`, the
legacy current-directory/install-prefix lookup remains supported before the
embedded fallback.

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

Discovery fills missing profile settings from the detected hardware and built-in
defaults, applies explicit CLI overrides, and saves the final profile with the
hardware inventory in cluster-config.yaml.

By default discovery advertises **one rail per NIC**. When a NIC exposes several
east-west PFs that are planes of a single physical port (e.g. Spectrum-X
multi-plane ConnectX-8/9), only the master PF is written to `cluster-config.yaml`
so the rail count reflects physical NICs, not PFs. A NIC whose VPD model name is
genuinely dual-port (e.g. "... QSFP112 **2-port** ...", "... **Dual-port** ...")
keeps a rail per port. Pass `--collapse-nic-rails=false` to restore the legacy
behaviour of one rail per PF (useful on dev setups). The model string is read
from `NicDevice.Status.modelName`; when it is empty the NIC is collapsed by
default.

Node groups whose NICs are **all north-south** (e.g. BlueField-DPU-only or
out-of-band-NIC-only nodes) are **not written** to `cluster-config.yaml` — they
produce no networking manifests and would otherwise consume an NV-IPAM subnet
slice. North-south NICs that sit alongside east-west NICs in the same group are
still recorded.

The saved `cluster-config.yaml` also **keeps the documentation comments** from
the config you started from (the default `l8k-config.yaml` or your
`--user-config`), so the field reference travels with the generated file.

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
      --config-dir string                   Directory containing optional l8k-config.yaml and presets/ overrides
      --enabled-plugins string              Comma-separated list of plugins to enable (default "network-operator")
      --image-pull-secrets strings          Image pull secret names for NicClusterPolicy (comma-separated)
      --kubeconfig string                   Path to kubeconfig file for cluster deployment (required when using --deploy; falls back to $KUBECONFIG, then ~/.kube/config)
      --network-operator-namespace string   Override the network operator namespace from the config file
      --network-operator-release string     Network Operator release line to deploy (MAJOR.MINOR). Selects component image tags + repository from a built-in catalog and drives version-gated template sections. Supported: 25.10, 26.1, 26.4
      --node-selector string                Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe (default "feature.node.kubernetes.io/pci-15b3.present=true")
      --user-config string                  Use provided cluster configuration file (as base config for discovery or as full config without discovery)

Discovery Flags:
      --collapse-nic-rails           Advertise one rail per NIC: collapse a NIC's multi-plane PFs to its master PF, keeping a rail per port only for NICs whose VPD model is genuinely dual-port ("2-port"/"Dual-port"). Set to false to keep the legacy one-rail-per-PF behaviour (dev setups). (default true)
      --discover-cluster-config      Deploy a thin Network Operator profile to discover cluster capabilities
      --save-cluster-config string   Save discovered cluster configuration to the specified path (defaults to --user-config path if set, otherwise ./cluster-config.yaml)

Profile Selection Flags:
      --deployment-type string   Select the deployment type (sriov, rdma_shared, host_device)
      --fabric string            Select the fabric type to deploy (infiniband, ethernet)
      --for string               Generate for a known server preset (replaces clusterConfig from the preset). Requires --node-selector. Run 'l8k preset list' with the same --config-dir to list available names.
      --gpu-type string          Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.
      --groups strings           Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.
      --multirail                Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)
      --spectrum-x string        Enable Spectrum-X by passing the SPC-X RA version (folds in the legacy --spcx-version). Supported: [RA2.1 RA2.2]

Spectrum-X Flags:
      --multiplane-mode string   Spectrum-X multiplane mode: none, swplb, hwplb, uniplane (requires --spectrum-x)
      --number-of-planes int     Number of planes for Spectrum-X (requires --spectrum-x)

Generation Output Flags:
      --enable-doca-driver             Enable DOCA driver deployment (overrides config file docaDriver.enable)
      --network-namespaces strings     Comma-separated namespaces for the secondary-network CRs and example test DaemonSets; one copy is rendered per namespace (shared resources like IPPools/NodePolicies are NOT duplicated). Overrides config networkNamespaces; default: 'default'.
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
    --save-deployment-files ./deployments
```

The generated config already contains the resolved profile. Pass profile flags
to `generate` only when you want to override the saved values.

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

When `--user-config` and a filesystem default are both absent, discovery uses
the default configuration embedded in the binary. Pass `--config-dir` to use
an external default and/or preset catalog instead.

The saved file contains the final `profile` section. Resolution order is:

1. Hardware and built-in defaults fill missing fields.
2. Existing values from `--user-config` are preserved.
3. Explicit `discover` CLI flags override both and are written back.

For example, force a shared-RDMA InfiniBand profile and explicitly opt out of
multirail:

```bash
l8k discover --kubeconfig ~/.kube/config \
    --fabric infiniband --deployment-type rdma_shared \
    --multirail=false \
    --save-cluster-config ./my-cluster-config.yaml
```

For routed multi-rail IPv4/RoCE deployments, `--routing source-based` chains
the automatic `sbr` CNI meta-plugin on generated non-Spectrum-X secondary
networks. This creates source-selected routing tables so traffic sourced from a
rail IP exits through that rail's interface and gateway.

Use `--ignore-arp` when pod rails can observe ARP for each other, or when the
fabric/VLAN isolation is not enough to prove that a rail IP can only be claimed
by its own VF. Linux normally treats IPv4 addresses as local to the network
namespace, so one pod interface can answer ARP for an address configured on
another interface. In routed multi-rail RoCE this can teach a remote peer that a
rail-0 IP lives behind a rail-3 VF MAC, sending traffic to the wrong HCA. The
flag chains the `tuning` CNI meta-plugin before `sbr` and sets `arp_ignore=1`,
`arp_announce=2`, and `rp_filter=0` at both `all` and `IFNAME` scopes.

These settings apply to SR-IOV, SR-IOV IB, host-device, Macvlan RDMA-shared,
and IPoIB RDMA-shared profiles. They do not apply to Spectrum-X profiles.

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

Use your own config file (with custom network operator version, subnets, or a
partial profile) as the base for discovery. Missing profile values are resolved;
existing values are retained unless a CLI flag overrides them. Without
`--save-cluster-config`, the file is rewritten in place with the final results:

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

After successful profile resolution, `generate` rewrites the source config with
the final defaults and explicit CLI overrides before rendering manifests. The
embedded config used by `--for` when no file is selected is not written.

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

# Generate from a known SKU using the embedded default config (no kubeconfig needed)
l8k generate \
    --for ThinkSystem-SR680a-V3-H200 \
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

During cluster discovery, Kubernetes Launch Kit creates a configuration file
that contains both hardware inventory and the resolved deployment profile. The
profile precedence is hardware/built-in defaults, then existing YAML values,
then explicit CLI flags. The config can be edited and supplied through
`--user-config` either as a standalone generation input or as a base for a
later discovery refresh. Refreshing replaces `clusterConfig`, fills only
missing profile fields, applies CLI overrides, and writes the final profile
back to YAML.

The tool resolves configuration and profile paths in order: local directory first (`./l8k-config.yaml`, `./profiles`), then installed location (`/usr/local/share/l8k/`), then binary-relative.

### Network Operator release selection

Use `--network-operator-release <MAJOR.MINOR>` (or `networkOperator.selectedRelease` in the config file) to pick a Network Operator release line by name instead of hand-editing image tags. Supported releases live in an embedded catalog ([pkg/networkoperatorplugin/releases/releases.yaml](pkg/networkoperatorplugin/releases/releases.yaml)); each entry maps a release key to image tags + repository for the operator and DOCA driver. Selecting a release populates `networkOperator.{version,componentVersion,repository}` and `docaDriver.version` from the catalog — explicit values in `l8k-config.yaml` are overridden when a release is set.

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

When Spectrum-X is enabled and no release is already set, profile resolution
selects its compatible default release (`RA2.1` → `26.1`, `RA2.2` → `26.4`).
An explicit CLI or config value is preserved and the pair is validated:
`--spectrum-x RA2.1 --network-operator-release 26.4` errors with a specific
"RA2.1 requires --network-operator-release in [26.1]" message rather than a
generic "no applicable profile found".

For non-Spectrum-X profiles, leaving both the flag and `selectedRelease` empty
continues to render the newest gates (treated as "latest").

Adding a new release is a YAML-only change in `releases.yaml` — patch bumps update an existing entry in place; new minor lines add a new top-level key.

### Maintenance and node upgrade concurrency

The top-level `maintenance` section controls how many nodes can be disrupted at
once by SR-IOV configuration and DOCA/OFED upgrades. The defaults allow four
concurrent operations instead of the operators' single-node defaults:

```yaml
maintenance:
  maxParallelOperations: 4
  maxUnavailable: 4
  maxNodeMaintenanceTimeSeconds: 3600
  maxParallelUpgrades: 4
```

For Network Operator 26.1 and newer, l8k enables Maintenance Operator requestor
mode in the generated Helm values. OFED upgrades use
`operator.maintenanceOperator.useRequestor`; SR-IOV additionally requires both
the Network Operator drain requestor and the SR-IOV external drainer. In this
mode, `maxParallelOperations` and `maxUnavailable` are global Maintenance
Operator limits. For older releases, `maxParallelUpgrades` controls OFED and
`SriovNetworkPoolConfig.spec.maxUnavailable` controls the SR-IOV internal
drainer.

Changing to requestor mode modifies Helm values and operator Deployment
environment variables. Upgrade an existing release with
`--overwrite-existing`; applying only the generated custom resources cannot
enable requestor mode. See [Maintenance and upgrade concurrency](docs/maintenance.rst)
for value restrictions, zero-value behavior, and release-specific details.

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

**IP exclusions** — l8k can populate the IPPool `spec.exclusions` so addresses
reserved for infrastructure (gateways, EVPN endpoints) are never handed to pods.
Two mechanisms combine:

- `reserveFirstIPs` / `reserveLastIPs` — a global, mask-agnostic pattern applied
  to **every** subnet (including each auto-generated per-rail subnet). They
  reserve the first N host addresses (from the network address upward) and the
  last N (down from the broadcast address).
- per-subnet `exclusions` — optional explicit `startIP`/`endIP` ranges on a
  manually-listed subnet, for anything the reserve pattern doesn't cover.

The computed reserve ranges are prepended to any explicit `exclusions`. The
gateway is not excluded automatically — it is covered by the low reserve block.

```yaml
nvIpam:
  poolName: nv-ipam-pool
  startingSubnet: "192.168.0.0"
  mask: 24
  offset: 1
  reserveFirstIPs: 10   # reserve .0–.9 on every /24
  reserveLastIPs: 6     # reserve .250–.255 on every /24
  # Optional explicit ranges on a manual subnet, merged on top of the reserve pattern:
  # subnets:
  # - subnet: 192.168.0.0/24
  #   gateway: 192.168.0.1
  #   exclusions:
  #   - {startIP: 192.168.0.2, endIP: 192.168.0.3}
```

For a `/24` this reserves `.0–.9` and `.250–.255`, leaving a usable range of
`.10–.249`. The rendered IPPool then carries:

```yaml
spec:
  subnet: 192.168.0.0/24
  gateway: 192.168.0.1
  exclusions:
  - startIP: 192.168.0.0
    endIP: 192.168.0.9
  - startIP: 192.168.0.250
    endIP: 192.168.0.255
```

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
maintenance:
  maxParallelOperations: 4
  maxUnavailable: 4
  maxNodeMaintenanceTimeSeconds: 3600
  maxParallelUpgrades: 4
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
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  routing: destination-based
  ignoreARP: false
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

During cluster discovery, the tool automatically identifies BlueField DPU devices (as opposed to SuperNICs or ConnectX NICs) by matching each device's `partNumber` against a known list of DPU product codes in [pkg/networkoperatorplugin/discovery/ns-product-ids](pkg/networkoperatorplugin/discovery/ns-product-ids). Devices matching a DPU product code are classified as **north-south** traffic (management/external), while all other devices are classified as **east-west** traffic (GPU interconnect).

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

### Library API

l8k exposes a helm-free Go library for in-process cluster discovery at
`github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery`. The
package is the public entry point for embedders (e.g. NVIDIA AICR's
network-topology snapshotter): it walks the cluster, bootstraps the
nic-configuration daemon, populates a `*config.LaunchKitConfig` with the
discovered hardware topology, and writes the
`nvidia.kubernetes-launch-kit.machine` / `.gpu` node labels.

```go
import (
    "path/filepath"

    l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
    l8kdisc   "github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery"
)

cfg, err := l8kconfig.DefaultLaunchKitConfig()  // baked-in NO release + repos
if err != nil { return err }

cfg, err = l8kdisc.Discover(ctx, kubeClient, restConfig, cfg,
    l8kdisc.WithLogger(logr.FromSlogHandler(slog.Default().Handler())),
    // l8kdisc.WithRelease("26.1"),  // optional override
    // l8kdisc.WithPresetsDir(filepath.Join(configDir, "presets")),
)
```

To replace the embedded default as well, load the filesystem copy before
calling discovery:

```go
cfg, err := l8kconfig.LoadFullConfig(
    filepath.Join(configDir, "l8k-config.yaml"), logger,
)
```

`WithPresetsDir` selects an authoritative catalog for that call and does not
mutate process-global preset state, so concurrent embedders can use different
directories safely.

The discovery package is deliberately Helm-free — importing it does NOT
pull `helm.sh/helm/v3` or its transitive deps into the consumer's binary.
The deploy/validate paths (network-operator install/upgrade, manifest
state checks) live in the parent `pkg/networkoperatorplugin` package and
are reachable from there for callers that need them. The release catalog
that powers `WithRelease` is exposed separately at
`pkg/networkoperatorplugin/releases` (`LookupRelease`, `SupportedReleases`).

### Docker

```bash
make docker-build # Build Docker image
make docker-run   # Run Docker container
```
