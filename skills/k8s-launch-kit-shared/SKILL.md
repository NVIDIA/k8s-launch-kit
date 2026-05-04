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
| `l8k preset list` | List bundled topology presets (directory + machineType + gpuType) |
| `l8k preset update` | Download latest topology presets from GitHub |
| `l8k sosreport` | Collect diagnostic dump from cluster |
| `l8k schema` | List all capabilities as JSON (profiles, flags, exit codes) |
| `l8k version` | Print version information |

The root command `l8k --discover-cluster-config ...` still works for backward-compatible full-pipeline usage.

## Global Flags

| Flag | Description |
|------|-------------|
| `--kubeconfig <PATH>` | Path to kubeconfig file (optional — falls back to `$KUBECONFIG` env var) |
| `--user-config <PATH>` | Path to user-supplied l8k-config.yaml |
| `--output <FORMAT>` | Output format: `text` (default), `json` |
| `--yes` / `-y` | Auto-confirm all prompts (root command only — **not available on subcommands**; `--output json` auto-confirms) |
| `--quiet` / `-q` | Suppress informational output (errors still shown) |
| `--network-operator-namespace <NS>` | Override network operator namespace (default: `nvidia-network-operator`) |
| `--pod-namespace <NS>` | Namespace for pods and network resources (default: `default`) |
| `--node-selector <LABELS>` | Restrict to nodes matching labels (comma-separated, ANDed) |
| `--image-pull-secrets <NAMES>` | Image pull secret names for NicClusterPolicy (comma-separated) |

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

Both `nvidia-network-operator` and `network-operator` are common default namespaces. If l8k fails with "no pods found", the error message will suggest using `--network-operator-namespace` to specify the correct namespace.

When the user does not specify a namespace and discovery fails:
1. Check the error message for the namespace hint
2. Retry with `--network-operator-namespace <correct-namespace>`
