---
name: k8s-launch-kit-shared
version: 1.0.3
description: "k8s-launch-kit (l8k) CLI: Shared patterns for binary location, global flags, output formatting, exit codes, and error handling. Read this before using any other k8s-launch-kit skill."
---

# l8k — Shared Reference

## Source Development and Installation

For source changes, read the checkout's [AGENTS.md](../../AGENTS.md). Build
and test that checkout from its repository root:

```bash
make build
./build/l8k schema
```

Global installation is optional for development. When installation is requested,
`make install` copies the binary and assets, while `make dev-install` links
assets into the source tree. Both use `scripts/install-local.sh`, can download
supporting assets and can write system paths; inspect the targets first.

## Install Paths

| Path | Contents |
|------|----------|
| `/usr/local/bin/l8k` | CLI binary (on PATH) |
| `/usr/local/share/l8k/profiles/` | Go template profiles |
| `/usr/local/share/l8k/scripts/kubectl-netop_sosreport` | Diagnostic helper |

After installation, `l8k` is available system-wide. Default configuration and
topology presets are embedded; existing filesystem overrides are preserved.

## Binary Discovery (for AI agents)

For installed CLI workflows, use `command -v l8k`. If it is unavailable,
report the missing prerequisite and use the installation guidance above when
installation is within the user's request. For source development, use
`./build/l8k` after `make build`; do not substitute an unrelated installed
binary for the checkout being tested.

## Available Commands

| Command | Description |
|---------|-------------|
| `l8k discover` | Discover cluster hardware and produce cluster-config.yaml |
| `l8k generate` | Generate Kubernetes YAML manifests from config + profile (use `--for <preset>` to skip cluster discovery for known SKUs) |
| `l8k deploy` | Apply generated manifests and install or upgrade the Network Operator Helm release |
| `l8k clean` | Delete Network Operator custom resources and uninstall its Helm release unless config or a flag retains it |
| `l8k validate` | Verify the Helm release, manifests, component versions, and connectivity |
| `l8k preset list` | List bundled topology presets (directory + machineType + gpuType) |
| `l8k preset update` | Download latest topology presets from GitHub |
| `l8k sosreport` | Collect diagnostic dump from cluster |
| `l8k schema` | List all capabilities as JSON (profiles, flags, exit codes) |
| `l8k version` | Print version information |

The root command `l8k --discover-cluster-config ...` still works for backward-compatible full-pipeline usage.

## Target selection

`discover`, `generate`, `deploy`, `validate`, and the root pipeline default to
the `host` target. Existing invocations must omit `--target` unless the user
explicitly requests it; `--target host` is an equivalent explicit form.

The `dpf` target name is reserved but its phases are not implemented in this
build. Do not attempt DPF provisioning. Use `l8k schema` and inspect
`targets[].phases` before selecting any non-host target. Host-only flags such as
`--fabric`, `--kubeconfig`, and the Network Operator/Spectrum-X flags are
rejected when explicitly combined with another target.

Internally, each lifecycle command snapshots the explicitly supplied Host
arguments, binds a phase-specific adapter through the target registry, and
runs the resulting operation exactly once with the command context. Do not
bypass this route when extending or automating a lifecycle phase. The
canonical component and data-flow diagrams live in
`docs/architecture/overview.md`; update them with any change to lifecycle,
package ownership, artifacts, or external integration boundaries.

## Global Flags

| Flag | Description |
|------|-------------|
| `--target <NAME>` | Lifecycle target: `host` (default); `dpf` is currently unavailable |
| `--kubeconfig <PATH>` | Path to kubeconfig file (optional — falls back to `$KUBECONFIG` env var) |
| `--user-config <PATH>` | Path to user-supplied l8k-config.yaml |
| `--output <FORMAT>` | Output format: `text` (default), `json` |
| `--yes` / `-y` | Auto-confirm all prompts (root command only — **not available on subcommands**; `--output json` auto-confirms) |
| `--quiet` / `-q` | Suppress informational output (errors still shown) |
| `--log-level <LEVEL>` | Enable logs at `trace`, `debug`, `info`, `warn`, or `error`. Debug shows structured progress; trace also shows bounded command output. |
| `--network-operator-namespace <NS>` | Override network operator namespace (default: `nvidia-network-operator`). **No-op for `l8k discover`** — discover always bootstraps into `nvidia-k8s-launch-kit`; the flag still applies to `l8k generate` / `l8k deploy` / `l8k clean` / `l8k validate`. |
| `--network-namespaces <NS,...>` | Comma-separated namespaces for the secondary-network CRs + example test DaemonSets; one copy rendered per namespace (shared resources like IPPools/NodePolicies are NOT duplicated). Default: `default` |
| `--node-selector <LABELS>` | Persist the deployment node selector (comma-separated, ANDed); does not restrict discovery scheduling |
| `--image-pull-secrets <NAMES>` | Image pull secret names for Network Operator components and authenticated Helm chart downloads (comma-separated) |
| `--skip-network-operator-helm` | On generate/deploy/validate and the root pipeline, skip Network Operator Helm values, installation, and Helm-specific validation while retaining custom-resource handling |

The persistent `networkOperator.skipHelmChart` setting also makes `l8k clean`
retain the externally owned Helm release while deleting Network Operator custom
resources. Clean does not route the skip flag; use config for the ownership
policy or `--keep-helm-chart` for an explicit retention-only override.

`l8k discover` and `l8k generate` both accept the profile flags `--fabric`,
`--deployment-type`, `--multirail`, `--spectrum-x`, `--multiplane-mode`, and
`--number-of-planes`. Discovery persists the resolved values; later generation
reuses them unless another explicit CLI override is supplied.

For automation, `l8k schema` exposes `configPaths` on config-backed flags. The
metadata comes from the shared option bindings used during resolution, so use
it instead of maintaining a separate CLI-to-YAML mapping.

## Agent / JSON Mode

Use `--output json` for scripted assertions. Preserve stderr for diagnosis and
check the process exit status before parsing output. Read the command's actual
result shape; do not assume every command emits exactly one `JSONResult`.
Text/help/schema inspection is appropriate when testing those interfaces.

**Do not use `--yes` with subcommands.** It exists only on the root command.
JSON mode is non-interactive, so confirm that the operation is within the
user's existing authorization before running it.

```bash
l8k generate --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment \
  --output json >generate.json 2>generate.log
```

Inspect the exit status, then parse `generate.json`; retain `generate.log` if
there is a failure. Avoid pipelines that hide the CLI's failure status.

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | General/unknown error |
| `2` | Validation error — bad flags, missing required arguments, or invalid config |
| `3` | Cluster error — kubeconfig invalid, API unreachable, missing CRDs, no NVIDIA NICs |
| `4` | Deployment error — apply failed |
| `5` | Partial success — discovery ok but deploy failed |

## Structured Error Output (JSON mode)

```json
{
  "success": false,
  "phase": "discover",
  "deployed": false,
  "error": {
    "code": "CLUSTER_ERROR",
    "message": "cluster discovery failed",
    "category": "cluster",
    "transient": true,
    "suggestion": "Check that kubeconfig is valid and the cluster is reachable"
  },
  "messages": [...]
}
```

Error categories: `validation`, `cluster`, `deployment`. The `transient` field
hints whether retrying might help.

## Schema Discovery

```bash
# List all l8k capabilities as JSON
l8k schema
```

Use `l8k schema` to programmatically discover available profiles, fabrics,
deployment types, flags, exit codes, and output formats.

## Security Rules

- Verify the target/context and stay within the user's existing authorization
  for live changes. A skill does not grant deployment or cleanup authority.
- Use deployment dry-run to preview changes where supported. Discovery and
  connectivity checks create cluster resources; root pipeline `--dry-run`
  does not make every phase read-only. `clean` has no dry-run mode.
- Never expose credentials or kubeconfig contents in output.

## Maintaining This Guidance

When behavior changes, update the affected documentation sections, phase skills
and bundled references in the same PR. Follow the
[documentation requirements](../../AGENTS.md#required-documentation-updates),
check examples against schema/help, and keep shared rules here.

## Network Operator Namespace Resolution

Applies to `l8k generate` / `l8k deploy` / `l8k validate` only. `l8k discover`
manages its own private namespace (`nvidia-k8s-launch-kit`) and ignores this flag.

Both `nvidia-network-operator` and `network-operator` are common default namespaces
for an existing Network Operator install. If `l8k generate` / `l8k deploy` /
`l8k validate` can't find Network Operator resources, retry with
`--network-operator-namespace <correct-namespace>`.

## OpenShift

Use `--flavor ocp` or `flavor: ocp` for OpenShift Host workflows. The default is `k8s`. See `docs/user/openshift.md` for external operator prerequisites and namespace defaults.
