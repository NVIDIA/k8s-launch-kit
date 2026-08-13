# K8s Launch Kit - CLI for configuring NVIDIA cloud-native solutions

K8s Launch Kit (l8k) is a CLI tool for deploying and managing NVIDIA cloud-native solutions on Kubernetes. The tool helps provide flexible deployment workflows for optimal network performance with SR-IOV, RDMA, and other networking technologies.

The four lifecycle commands are target-aware. The existing Network Operator
workflow is the `host` target and remains the default, so current commands do
not need to change. Each lifecycle command binds a typed, immutable request
through the target registry and the concrete Host implementation.
`--target host` is an explicit synonym. The `dpf` target name is reserved for
future DPU-plane provisioning and currently reports its phases as unavailable
instead of executing host logic. See the
[target-aware CLI architecture](docs/advanced/targets.md), the
[complete component map](docs/architecture/overview.md), and run `l8k schema`
for machine-readable target, phase, and flag ownership information.

## Documentation

Standalone documentation is published from this repository to GitHub Pages:

https://nvidia.github.io/k8s-launch-kit/

Build it locally with:

```bash
python -m pip install -r requirements-docs.txt
mkdocs build --strict
```

## Repository Automation

The `dependabot-auto-merge.yml` workflow automatically merges non-draft
Dependabot pull requests that target the current default branch after every
other pull request check has completed successfully or been skipped. Conflicting
pull requests and pull requests with unsuccessful checks are left open. A
scheduled scan revisits open Dependabot pull requests whose checks outlast the
initial workflow run.

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

For Spectrum-X, discovery combines the east-west NIC device ID with each
group's `gpuType` (or `machineType` fallback). H100/H200/B200/GB200 platforms
default to `multiplaneMode: none` and `numberOfPlanes: 1`. B300 and GB300
default to the common GA dual-plane configuration, `swplb` with 2 planes.
Platform type cannot distinguish `swplb` from `hwplb`: both are available on
B300 and GB300, while `hwplb` is an explicit opt-in. Pass
`--multiplane-mode hwplb` when required, and pass `--number-of-planes 4`
explicitly for a quad-plane B300 topology. The same defaults apply when
`--for` supplies hardware from a topology preset.

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

### Generate Deployment Files
Based on the discovered or provided configuration,
generate a complete set of YAML deployment files for the selected network profile.
Files can be saved to disk using --save-deployment-files.
The profile is defined with --fabric, --deployment-type and --multirail flags,
or via a profile section in the user-config file.

### Deploy to Cluster
Apply the generated deployment files to your Kubernetes cluster by using --deploy. This phase requires --kubeconfig and can be skipped if --deploy is not specified.

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
    --spectrum-x RA2.3 --multiplane-mode hwplb --number-of-planes 4 \
    --spectrum-x-config ./spectrum-x-profile-configmap.yaml \
    --network-operator-release 26.7 --deploy --output json --yes

  # Dry-run: preview what would be deployed
  l8k --user-config cluster-config.yaml --spectrum-x --deploy \
    --dry-run --output json

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
      --image-pull-secrets strings          Image pull secret names for Network Operator components and authenticated Helm downloads (comma-separated)
      --kubeconfig string                   Path to kubeconfig file for cluster deployment (required when using --deploy; falls back to $KUBECONFIG, then ~/.kube/config)
      --network-operator-namespace string   Override the network operator namespace from the config file
      --network-operator-release string     Network Operator release line to deploy (MAJOR.MINOR). Selects component image tags + repository from a built-in catalog and drives version-gated template sections. Supported: 26.1, 26.4, 26.7
      --node-selector string                Node selector written into the saved cluster-config (used at deploy time). Does NOT gate discovery scheduling — the daemon runs on all nodes and NIC nodes are detected via a sysfs PCI-vendor probe (default "feature.node.kubernetes.io/pci-15b3.present=true")
      --user-config string                  Use provided cluster configuration file (as base config for discovery or as full config without discovery)

Host Target Discovery Flags:
      --collapse-nic-rails           Advertise one rail per NIC: collapse a NIC's multi-plane PFs to its master PF, keeping a rail per port only for NICs whose VPD model is genuinely dual-port ("2-port"/"Dual-port"). Set to false to keep the legacy one-rail-per-PF behaviour (dev setups). (default true)
      --discover-cluster-config      Deploy a thin Network Operator profile to discover cluster capabilities
      --save-cluster-config string   Save discovered cluster configuration to the specified path (defaults to --user-config path if set, otherwise ./cluster-config.yaml)

Host Target Profile Selection Flags:
      --deployment-type string   Select the deployment type (sriov, rdma_shared, host_device)
      --fabric string            Select the fabric type to deploy (infiniband, ethernet)
      --for string               Generate for a known server preset (replaces clusterConfig from the preset). Requires --node-selector. Run 'l8k preset list' with the same --config-dir to list available names.
      --gpu-type string          Generate manifests only for source groups whose gpuType matches (case-insensitive). Mutually exclusive with --groups.
      --groups strings           Generate manifests only for the named source groups (comma-separated identifiers from cluster-config.yaml). Mutually exclusive with --gpu-type.
      --ignore-arp               Chain the tuning CNI meta-plugin to prevent ARP flux across pod rails
      --multirail                Override multirail deployment (defaults to true when absent; use --multirail=false to opt out)
      --routing string           Secondary-network routing mode: destination-based or source-based. source-based chains the automatic sbr CNI meta-plugin.
      --spectrum-x string        Enable Spectrum-X by passing the SPC-X RA version (folds in the legacy --spcx-version). Supported: [RA2.1 RA2.2 RA2.3]

Host Target Spectrum-X Flags:
      --ip-version string                  Spectrum-X IP version for guide-based allocation: ipv4 or ipv6 (requires --spectrum-x)
      --multiplane-mode string             Spectrum-X multiplane mode: none, swplb, hwplb (requires --spectrum-x)
      --number-of-planes int               Number of planes for Spectrum-X (requires --spectrum-x)
      --spectrum-x-config string           Path to full Spectrum-X profile ConfigMap YAML or raw data.profile YAML (required for SPC-X RA versions newer than RA2.2)
      --spectrum-x-configmap-name string   Spectrum-X profile ConfigMap name when --spectrum-x-config contains raw data.profile YAML
      --topology-file string               Path to spcx-gen/reference-generator or NVIDIA AIR topology JSON for Spectrum-X CIDRPool generation (requires --spectrum-x)
      --topology-scheme string             Spectrum-X topology scheme for guide-based IP allocation: 2-tier or 3-tier (requires --spectrum-x)

Host Target Generation Output Flags:
      --enable-doca-driver             Enable DOCA driver deployment (overrides config file docaDriver.enable)
      --network-namespaces strings     Comma-separated namespaces for the secondary-network CRs and example test DaemonSets. One independent copy is rendered per namespace (shared resources like IPPools and NodePolicies are NOT duplicated). Overrides config networkNamespaces; default: 'default'.
      --save-deployment-files string   Save generated deployment files to the specified directory (default "./deployment")
      --workload-manifest string       Path to a custom workload manifest YAML (replaces the profile's default example workload)

Target-Agnostic Execution Flags:
      --deploy                    Deploy the generated files to the Kubernetes cluster
      --deploy-timeout duration   Maximum end-to-end wall-clock budget for the deploy phase (e.g. 45m, 2h). 0 (the default) means no deadline; the deploy polls until every manifest reaches a terminal state.
      --dry-run                   Preview what would be deployed without applying changes to the cluster

Target-Agnostic Output & Logging Flags:
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
they're fixtures consumed by `l8k validate --connectivity` for the data-plane
phase. It auto-prefers
`<dir>/network-operator/` (the layout `l8k generate` produces) and falls back
to `<dir>` itself. `--dry-run` does a server-side dry run. `--deploy-timeout`
caps the whole apply+reconcile phase end-to-end (e.g. `--deploy-timeout 90m`);
without it, deploy polls indefinitely — right for SR-IOV on large clusters
where reconciliation can take an hour.

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
and NicFirmwareTemplate validation scoped to the operator-populated
`status.nicDevices`, current template-payload and device-generation checks,
matched-set checks for node, NIC type, PCI, serial-number, and part-number
selectors, condition-Reason classification, and firmware-condition gating
only for devices carrying `spec.firmware`,
NicClusterPolicy appliedStates breakdown, etc.); and (3) a data-plane
connectivity matrix —
apply the example DaemonSet, wait for it to roll out completely (`numberReady ==
desiredNumberScheduled > 0` — a single ContainerCreating-stuck pod fails),
and run the configured checks (`icmp`, `rping`, and/or `ib_write_bw`) with
source-bound rail identity. Every profile renders the DaemonSet with a DOCA
container for RDMA checks and a declared `netshoot` container for ICMP; validate
applies that manifest without injecting containers at runtime. ICMP uses
`ping -I <src-iface>`, `rping` uses
`-I <src-ip>`, and `ib_write_bw` uses `--bind_source_ip <src-ip>`; every test
also records a source-qualified `ip route get <dst> from <src>` lookup from the
netshoot container. When `validation.gpuDirect.enabled` is true, a separate
DMA-BUF bandwidth stage repeats the selected `ib_write_bw` matrix with
`--use_cuda=<endpoint-index> --use_cuda_dmabuf`. Source and destination CUDA
indices are resolved independently from each node's per-PF `connectedGPU`
topology; missing or ambiguous mappings fail explicitly instead of falling
back to GPU 0. The
matrix is on by default (`--connectivity=false` to skip) and cleans up the
test DaemonSet unless `--keep` is set. Fresh `l8k discover` output includes
the default validation block:

```yaml
validation:
  gpuDirect:
    enabled: false
    gpuResourceType: nvidia.com/gpu
  connectivity: true
  mode: strict
  checks:
    - icmp
    - rping
    - ib_write_bw
  rdma:
    rpingIterations: 5
    ibWriteSize: 65536
    ibWriteMinBandwidthGbps: 100
```

Discovery always writes `gpuDirect.enabled`: it is `true` only when every
worker in every discovered group can satisfy the topology-derived
`gpuResourceType` request for its render bucket, and `false` otherwise. Users
can edit the result before generation. When enabled, generated validation DaemonSets use
the release-specific full-runtime DOCA image, propagate
`networkOperator.imagePullSecrets`, and request the GPU prefix required by the
largest discovered `GPU<N>` index in the primary DOCA container. The netshoot
container never requests GPUs. The named pull Secret must exist in every
validation workload namespace.

Validation modes control cross-rail coverage and gating:

- `quick` runs all same-rail node pairs plus one non-gating cross-rail canary
  for every source-rail/destination-rail mapping.
- `full` runs every source rail × every destination rail × every ordered pod
  pair; cross-rail results are reported but do not decide pass/fail.
- `strict` is the default. It runs the full matrix and gates cross-rail by the
  generated profile routing mode: `profile.routing: source-based` requires
  cross-rail success, while `destination-based` requires cross-rail isolation.

`l8k validate` can override that config for a single run:
`--validation-mode quick|full|strict`, `--validation-checks icmp`,
`--validation-checks rping`, `--validation-checks ib_write_bw`,
`--validation-checks ""`, `--rdma-rping-iterations <N>`,
`--rdma-ib-write-size <BYTES>`, and
`--rdma-ib-write-min-bandwidth-gbps <GBPS>` (`0` disables bandwidth gating).

A self-contained HTML report lands at `<deployment-files>/k8s-launch-kit-validation-report.html`
by default (override with `--report-path`, disable with `--report-path=-`).
The report has: header (l8k version, kubeconfig context, API-server version),
profile, **Node groups** (per-`clusterConfig[]` entry with east-west / north-
south PF tables — PCI, deviceID, rail, netdev, RDMA device, PSID, part #,
NUMA, connected GPU), cluster nodes, Network Operator release, **Manifest
state** (with expandable Details + **Live YAML** dropdowns per row),
connectivity matrix (per-rail src×dst grids + cross-rail results), and a
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

In heterogeneous clusters, discovery produces multiple node groups. Use `--groups` to generate manifests for a single group:

```bash
l8k generate --user-config ./config.yaml \
    --fabric infiniband --deployment-type sriov --multirail \
    --groups group-0 \
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

### Remove a Network Operator Deployment

`l8k clean` removes a Network Operator installation from the selected cluster:

```bash
l8k clean --kubeconfig ~/.kube/config
```

The command resolves the operator namespace from an explicit
`--network-operator-namespace`, `networkOperator.namespace` in
`--user-config`, `./cluster-config.yaml`, or an explicit `--config-dir`, then
the `nvidia-network-operator` default. Custom installation namespaces must be
supplied by flag or config; in-cluster objects never select this destructive
target. The command deletes every namespaced custom resource in that namespace,
removes the known cluster-scoped Network Operator CRs, waits for their
finalizers, and uninstalls the `network-operator` Helm release last. The
namespace, CRDs, unrelated Secrets, files on disk, and custom resources outside
the namespace are preserved; Helm metadata and chart-managed resources are
removed with the release.

Pass `--keep-helm-chart` to delete the custom resources while leaving the Helm
release installed. Cleanup is destructive and confirms the resolved target in
text mode. See [the cleanup guide](docs/user/cleanup.md) for the full deletion
boundary and automation output.

### Troubleshooting Network Operator Issues

Collect a diagnostic dump from the cluster:

```bash
l8k sosreport --kubeconfig ~/.kube/config --output-dir ./sosreport
```

The sosreport contains NicClusterPolicy, pod logs, node info, CRDs, and other diagnostic data. For interactive AI-assisted analysis, use the bundled Claude Code skills under `skills/k8s-launch-kit-troubleshoot/` — they wrap the deterministic commands (`l8k sosreport`, `kubectl`) and let the agent driving the skill do the reasoning.

### AI Agent / Automation Usage

l8k supports structured output for AI agents and CI/CD pipelines. Use `--output json` to get machine-readable output and auto-confirm subcommand prompts; `--yes` is available on the root pipeline. Use `--dry-run` to preview deployment changes safely. Because `l8k clean` has no dry-run mode, verify its kubeconfig and resolved namespace before using JSON mode.

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

Use `--network-operator-release <MAJOR.MINOR>` (or `networkOperator.selectedRelease` in the config file) to pick a Network Operator release line by name instead of hand-editing image tags. Supported releases live in an embedded catalog ([pkg/networkoperatorplugin/releases/releases.yaml](pkg/networkoperatorplugin/releases/releases.yaml)); each entry maps a release key to image tags and repositories for the operator, DOCA driver, full-runtime validation workload, and—where the release deploys it—the independently versioned xPlane service. Selecting a release populates `networkOperator.{version,componentVersion,repository}` and `docaDriver.version` from the catalog, while validation workloads and Spectrum-X profiles resolve their image coordinates from the same entry. Explicit values in `l8k-config.yaml` are overridden when a release is set.

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

There are three **Spectrum-X** profiles, picked by the value of `--spectrum-x`:

- **`spectrum-x`** — RA2.3 on `26.7+`. Uses the v1alpha2 `SpectrumXRailPoolConfig` with `railTopology[]` and deploys the Spectrum-X profile through a ConfigMap consumed by NIC Configuration Operator. Selected for `--spectrum-x RA2.3`.
- **`spectrum-x-ra2.2`** — RA2.2 on `26.4` only. Uses the v1alpha2 `SpectrumXRailPoolConfig` with `railTopology[]` to consolidate rail wiring. Selected for `--spectrum-x RA2.2`.
- **`spectrum-x-ra2.1`** — RA2.1 on `26.1` only (pinned via `min`/`maxNetworkOperatorRelease: "26.1"`). Renders the full SR-IOV operator chain: per-group `SriovNetworkPoolConfig` + per-rail `SriovNetworkNodePolicy` + `OVSNetwork` + nv-ipam `CIDRPool` + a v1alpha1 glue `SpectrumXRailPoolConfig`. Selected for `--spectrum-x RA2.1`.

The v1alpha2 RA2.2 and RA2.3 manifests omit the removed `spec.withBCM`
field. Current `SpectrumXRailPoolConfig` CRDs reject that field during strict
decoding.

When Spectrum-X is enabled and no release is already set, profile resolution
selects its compatible default release (`RA2.1` → `26.1`, `RA2.2` → `26.4`,
`RA2.3` → `26.7`).
An explicit CLI or config value is preserved and the pair is validated:
`--spectrum-x RA2.1 --network-operator-release 26.4` errors with a specific
"RA2.1 requires --network-operator-release in [26.1]" message rather than a
generic "no applicable profile found".

RA2.3 requires a Spectrum-X profile ConfigMap input. Pass either a full
ConfigMap YAML:

```bash
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.3 --network-operator-release 26.7 \
  --spectrum-x-config ./spectrum-x-profile-configmap.yaml
```

or pass only the raw YAML that belongs under `data.profile` and provide the
ConfigMap name:

```bash
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.3 --network-operator-release 26.7 \
  --spectrum-x-config ./profile.yaml \
  --spectrum-x-configmap-name site-ra23-profile
```

l8k stores the raw Spectrum-X profile text in
`profile.spectrumX.profile`, renders the deployed ConfigMap into the Network
Operator namespace with the
`network.nvidia.com/operator.nic-configuration.spectrum-x-profile` label, and
sets `NicConfigurationTemplate.spec.template.spectrumXOptimized.version` to
the rendered ConfigMap name.

For the RA2.2 and RA2.3 Spectrum-X profiles, `profile.spectrumX.useDRA` defaults to `false`.
When set to `true`, l8k enables the SR-IOV operator `dynamicResourceAllocation`
feature gate, sets `SpectrumXRailPoolConfig.spec.draEnabled: true`, emits
`ResourceClaimTemplate` manifests, and renders the example workload with DRA
claims instead of `nvidia.com/rail_*` device-plugin resource requests.

Spectrum-X CIDRPools are generated from spcx-gen/reference-generator or contract-compliant
NVIDIA AIR topology JSON plus the resolved l8k discovery data. l8k detects the
format from the JSON structure. AIR support relies on the documented node and
interface naming contract; see [Spectrum-X topology-driven CIDRPools](docs/user/spectrum-x.md#topology-driven-cidrpools).
Pass the topology scheme and file on the CLI, or set the same values under
`profile.spectrumX`:

```bash
l8k generate --user-config cluster-config.yaml \
  --spectrum-x RA2.3 \
  --topology-scheme 2-tier \
  --ip-version ipv6 \
  --topology-file ./topology.json
```

`profile.spectrumX.hostFirstOctet` is config-only. When omitted, l8k uses `172`
for 2-tier IPv4 allocation and `10` for 3-tier IPv4 allocation. It has no effect
on IPv6 allocation. IPv6 uses the standard `fd02:00PP:RRDD:SSHH::peer` layout,
with a `/64` per node, host candidate `::1`, leaf gateway `::2`, and a `/40`
CIDRPool per rail or rail-plane. Generated IPv6 routes use `/32` for a single
plane and `/24` for dual- or quad-plane deployments.

CIDRPool allocation requires exact, case-sensitive equality between selected
`clusterConfig.workerNodes` values and topology host endpoint `node` values.
Generation errors summarize both name sets and identify missing rail/plane
coverage, including likely case or short-name/FQDN mismatches. See the
[Spectrum-X troubleshooting guidance](docs/user/spectrum-x.md#troubleshooting-cidrpool-allocation-errors).

For non-Spectrum-X profiles, leaving both the flag and `selectedRelease` empty
continues to render the newest gates (treated as "latest").

Every catalog entry is synchronized nightly from
`Mellanox/network-operator`'s `v<MAJOR.MINOR>.x` branch. When the release
branch for the highest catalog key has not been created yet, the synchronizer
uses `master` and verifies that its Network Operator version still belongs to
that release line. When values change, the workflow verifies the synchronized
catalog, opens or refreshes its PR, and squash-merges it automatically. The
release stage checks out that merge commit and verifies the catalog is still
current before publishing anything. Run the same update locally with
`GITHUB_TOKEN=<token> make sync-network-operator-releases` (authentication
avoids GitHub's low anonymous API rate limit).

After a catalog update reaches `main`, the workflow compares the previous and
new highest catalog versions. If the new version is newer and the exact tag
exists in `Mellanox/network-operator`, it publishes a matching k8s-launch-kit
GitHub Release with generated release notes. Publishing the release creates
the tag on the catalog-update commit, then dispatches the existing GoReleaser
and release-image workflows for that tag. No tag or release is created before
the catalog update reaches `main`.

Adding a new release remains a YAML-only catalog change: add its top-level
entry under `releases:`. It automatically becomes part of the nightly sync and,
while it is the highest key, can use `master` until its release branch exists.

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
  version: doca3.3.0-26.01-1.0.0.0-6
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
  # NicConfigurationTemplate nicType and pciAddresses are derived from the
  # east-west PF inventory of each source group.
  overlay: none
  singlePlane:
    netdevPrefix: "eth_r%rail_id%"
    rdmaPrefix: "roce_r%rail_id%"
  hwplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%"
  swplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%_p%plane_id%"
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  routing: destination-based
  ignoreARP: false
  spectrumX:
    enable: true
    spcxVersion: RA2.3
    multiplaneMode: hwplb
    numberOfPlanes: 4
    topologyType: 2-tier
    ipVersion: ipv4
    topologyFile: ./topology.json
    configMapName: site-ra23-profile
    profile: |
      useSoftwareCCAlgorithm: true
    useDRA: false                       # Set true to generate Spectrum-X DRA ResourceClaimTemplates
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

North-south PFs are included in the saved cluster configuration for visibility, but are **automatically filtered out** during template rendering so that only east-west PFs appear in the generated manifests. Spectrum-X renders one `NicConfigurationTemplate` per source hardware group and selects the intersection of that group's east-west NIC type and PCI addresses. This prevents a north-south DPU from receiving Spectrum-X settings when it reports the same device ID as an east-west SuperNIC. Each east-west PF is assigned a sequential rail number (rail-0, rail-1, rail-2, ...) used for naming resources like SriovNetworkNodePolicy and IPPool entries.

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

When both values are available, discovery derives the group `identifier` from
`<machineType>-<gpuType>`. Generated identifiers are limited to 40 bytes; long
values keep a readable prefix plus an 8-character deterministic hash. This
keeps resource names and label values that append the identifier below their
Kubernetes size limits without making similar hardware identities collide.

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

Spectrum-X interface prefixes default according to the selected multiplane mode:

| Mode | Network device prefix | RDMA device prefix |
| --- | --- | --- |
| `hwplb` | `eth_r%rail_id%_p%plane_id%` | `roce_r%rail_id%` |
| `swplb` | `eth_r%rail_id%_p%plane_id%` | `roce_r%rail_id%_p%plane_id%` |
| `none` | `eth_r%rail_id%` | `roce_r%rail_id%` |

Set the prefixes in the corresponding `spectrumX.singlePlane`, `spectrumX.hwplb`, or `spectrumX.swplb` block to override that mode. The `none` mode uses `singlePlane`.

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
