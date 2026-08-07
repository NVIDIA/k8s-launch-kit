---
name: k8s-launch-kit-shared
version: 1.0.0
description: "k8s-launch-kit (l8k) CLI: Shared patterns for binary location, global flags, output formatting, exit codes, and error handling. Read this before using any other k8s-launch-kit skill."
---

# l8k — Shared Reference

## Installation

```bash
# Build and install (production — copies binary + profiles)
make build && sudo scripts/install.sh

# Development (symlinks into source tree)
make build && scripts/install.sh --dev-env
```

## Install Paths

| Path | Contents |
|------|----------|
| `/usr/local/bin/l8k` | CLI binary (on PATH) |
| `/usr/local/share/l8k/profiles/` | Go template profiles |
| `/usr/local/share/l8k/presets/` | Topology presets (per `(machineType, gpuType)` directories) |
| `/usr/local/share/l8k/l8k-config.yaml` | Default config |

After installation, `l8k` is available system-wide.

## Binary Discovery (for AI agents)

**CRITICAL — follow these steps exactly:**
1. Run `which l8k` in a single Bash call. If it returns a path, use it. Done.
2. If `which l8k` fails (exit code 1), tell the user l8k is not installed. Point them to: `make build && sudo scripts/install.sh`. **Stop — do not proceed.**
3. **NEVER** do any of the following to find l8k:
   - `find` or `ls` commands
   - Spawn a subagent to locate the binary
   - Check multiple paths or directories
   - Explore the repo contents

## Available Commands

| Command | Description |
|---------|-------------|
| `l8k discover` | Discover cluster hardware and produce cluster-config.yaml |
| `l8k generate` | Generate Kubernetes YAML manifests from config + profile (use `--for <preset>` to skip cluster discovery for known SKUs) |
| `l8k deploy` | Apply generated manifests and install or upgrade the Network Operator Helm release |
| `l8k clean` | Delete Network Operator custom resources and optionally uninstall its Helm release |
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

## Global Flags

| Flag | Description |
|------|-------------|
| `--target <NAME>` | Lifecycle target: `host` (default); `dpf` is currently unavailable |
| `--kubeconfig <PATH>` | Path to kubeconfig file (optional — falls back to `$KUBECONFIG` env var) |
| `--user-config <PATH>` | Path to user-supplied l8k-config.yaml |
| `--output <FORMAT>` | Output format: `text` (default), `json` |
| `--yes` / `-y` | Auto-confirm all prompts (root command only — **not available on subcommands**; `--output json` auto-confirms) |
| `--quiet` / `-q` | Suppress informational output (errors still shown) |
| `--network-operator-namespace <NS>` | Override network operator namespace (default: `nvidia-network-operator`). **No-op for `l8k discover`** — discover always bootstraps into `nvidia-k8s-launch-kit`; the flag still applies to `l8k generate` / `l8k deploy` / `l8k clean` / `l8k validate`. |
| `--network-namespaces <NS,...>` | Comma-separated namespaces for the secondary-network CRs + example test DaemonSets; one copy rendered per namespace (shared resources like IPPools/NodePolicies are NOT duplicated). Default: `default` |
| `--node-selector <LABELS>` | Restrict to nodes matching labels (comma-separated, ANDed) |
| `--image-pull-secrets <NAMES>` | Image pull secret names for Network Operator components and authenticated Helm chart downloads (comma-separated) |

`l8k discover` and `l8k generate` both accept the profile flags `--fabric`,
`--deployment-type`, `--multirail`, `--spectrum-x`, `--multiplane-mode`, and
`--number-of-planes`. Discovery persists the resolved values; later generation
reuses them unless another explicit CLI override is supplied.

## Agent / JSON Mode

**RULE: AI agents MUST always use `--output json 2>/dev/null` when calling any l8k subcommand.** Never use text mode — it produces unstructured output with spinners and ANSI codes that is hard to parse.

**Do NOT use `--yes` with subcommands** — it only exists on the root command and will cause "unknown flag" errors. `--output json` already auto-confirms all prompts (no interactive input needed).

```bash
# Correct
l8k discover --output json 2>/dev/null
l8k generate --output json 2>/dev/null

# WRONG — --yes is not a valid flag on subcommands
l8k discover --output json --yes 2>/dev/null
```

- **stdout**: Exactly one JSON object (`JSONResult`) at completion
- **stderr**: Human-readable log lines
- Pipe with `jq` for downstream processing: `l8k discover ... --output json 2>/dev/null | jq .success`

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

- Confirm with the user before executing `--deploy` on a production cluster
- Prefer `--dry-run` for destructive operations
- Never expose kubeconfig contents in output

## Network Operator Namespace Resolution

Applies to `l8k generate` / `l8k deploy` / `l8k validate` only. `l8k discover`
manages its own private namespace (`nvidia-k8s-launch-kit`) and ignores this flag.

Both `nvidia-network-operator` and `network-operator` are common default namespaces
for an existing Network Operator install. If `l8k generate` / `l8k deploy` /
`l8k validate` can't find Network Operator resources, retry with
`--network-operator-namespace <correct-namespace>`.
