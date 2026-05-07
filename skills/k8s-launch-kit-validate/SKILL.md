---
name: k8s-launch-kit-validate
version: 1.0.0
description: "Use this skill when the user wants to verify that an NVIDIA networking deployment matches the configuration that produced it. Activate for: 'is my deployment correct', 'are all the manifests applied', 'does the network operator version match', 'verify deployment', 'check cluster state against config', or any question about whether the cluster reflects what l8k generated. Wraps the `l8k validate` subcommand."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Validate

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and exit codes.

Verify that a previously generated and deployed NVIDIA networking
deployment is correctly applied and matches the selected Network
Operator release.

## What it checks

1. **Network Operator Helm release version.** Reads the chart's
   `appVersion` from any release Secret named
   `sh.helm.release.v1.<release>.v<N>` whose release name contains
   "network-operator", in the operator namespace. Compares with the
   version expected by `networkOperator.selectedRelease` in
   `cluster-config.yaml` (looked up in l8k's embedded release catalog).
2. **Manifest presence.** Every YAML manifest under
   `--deployment-files` (skipping any file with "example" in its name)
   is fetched from the cluster via `client.Get`. Each manifest is
   reported `FOUND`, `MISSING`, or `ERROR`.

Exit code is non-zero (4) on any missing manifest or version mismatch.
Both checks soft-skip when prerequisites are absent — no
`cluster-config.yaml`, no Helm release Secret, etc.

## Usage

```bash
l8k validate [--user-config <PATH>] [--deployment-files <DIR>] [--kubeconfig <PATH>]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--kubeconfig` | `$KUBECONFIG` | Path to kubeconfig with read access to the cluster |
| `--user-config` | `./cluster-config.yaml` | Cluster config YAML; used for `networkOperator.selectedRelease` and the operator namespace |
| `--deployment-files` | `./deployment` | Directory containing the manifests to verify |

## Examples

```bash
# Defaults: ./cluster-config.yaml + ./deployment, $KUBECONFIG
l8k validate

# Explicit paths
l8k validate --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig ~/.kube/config

# Agent mode (single JSON object on stdout, logs on stderr)
l8k validate --output json --yes 2>/dev/null | jq '.summary'
```

## Output

Text mode prints a short report:

```
Network Operator release
  selectedRelease: 26.4
  expected version: v26.4.0-beta.6
  deployed: network-operator (chart=26.4.0-beta.6 app=v26.4.0-beta.6 rev=3 status=deployed)
  result: MATCH

Manifests
  [FOUND] NicClusterPolicy/nic-cluster-policy in (cluster-scoped)
  [FOUND] NicNodePolicy/nicnodepolicy-h100 in (cluster-scoped)
  [MISSING] SriovNetwork/sriov-network-rail-0 in default — not found in cluster
  ...

Summary: 12 manifests, 1 missing/error; version: match
```

JSON mode (`--output json`) emits one object with `versionCheck`,
`manifests`, and `summary` fields.

## When this skill activates

Trigger phrases include: "validate my deployment", "is my cluster
correct", "are all the manifests applied", "does the chart version
match", "did the deploy succeed", or any discrepancy claim about
expected vs deployed state.

## See Also

- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — apply manifests
- [k8s-launch-kit-troubleshoot](../k8s-launch-kit-troubleshoot/SKILL.md) — investigate failures uncovered by validate
- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — global flags and exit codes
