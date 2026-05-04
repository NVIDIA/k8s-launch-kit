---
name: k8s-launch-kit-pipeline
version: 1.1.0
description: "Use this skill when the user wants to run the full k8s-launch-kit (l8k) pipeline end-to-end: discover cluster hardware, select a profile, generate manifests, and deploy them all in one command. Also activate for CI/CD integration, automation pipelines, 'one-liner', 'complete workflow', or end-to-end NVIDIA networking deployment."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Full Pipeline

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Run discovery + generation + deployment as a single command.

## Usage

The root command chains all phases in one invocation:

```bash
l8k --discover-cluster-config [--kubeconfig <PATH>] \
  --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <DIR> --deploy
```

Or use subcommands for a two-step approach:

```bash
l8k discover --save-cluster-config ./cluster-config.yaml && \
l8k generate --user-config ./cluster-config.yaml \
  --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <DIR> --deploy
```

## Examples

```bash
# Full pipeline: discover + SR-IOV Ethernet + deploy (root command)
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy

# Full pipeline: Spectrum-X
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --spectrum-x RA2.2 --multiplane-mode hwplb --number-of-planes 4 \
  --save-deployment-files ./output --deploy

# Non-default operator namespace
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --network-operator-namespace network-operator \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy

# Agent / CI mode
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy \
  --output json --yes 2>/dev/null

# Pipeline with dry-run (validate everything, apply nothing)
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy --dry-run

# Subcommand alternative: discover then generate+deploy separately
l8k discover --kubeconfig ~/.kube/config \
  --save-cluster-config ./cluster-config.yaml && \
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy

# Skip discovery entirely with --for (known SKU)
l8k generate --user-config ./cluster-config.yaml \
  --for ThinkSystem-SR680a-V3 \
  --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output --deploy \
  --kubeconfig ~/.kube/config
```

## Common Variations

| Use Case | Command |
|----------|---------|
| Discovery only | `l8k discover --save-cluster-config <PATH>` |
| Generate only | `l8k generate --user-config <CONFIG> --fabric ... --save-deployment-files <DIR>` |
| Generate + deploy | `l8k generate ... --deploy` |
| Full pipeline (root) | `l8k --discover-cluster-config ... --deploy` |
| Full pipeline (subcommands) | `l8k discover ... && l8k generate ... --deploy` |
| Full pipeline dry-run | `l8k --discover-cluster-config ... --deploy --dry-run` |

Note: The root command's strength is chaining all phases — it runs discover, generate, and deploy in a single invocation. Use subcommands when you need intermediate inspection or different flags per phase.

## Phase Order

1. **Discover** — Deploy minimal NicClusterPolicy, read NodeFeature CRDs, group nodes
2. **Generate** — Match profile, render templates, write YAMLs
3. **Deploy** — Apply resources in dependency order

If any phase fails, subsequent phases are skipped. The JSON output includes which phase failed.

> [!CAUTION]
> The full pipeline includes deployment — confirm with the user before running on production. Use `--dry-run` to preview first.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and exit codes
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Discovery details
- [k8s-launch-kit-generate](../k8s-launch-kit-generate/SKILL.md) — Profile selection details
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Deploy details
