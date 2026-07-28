<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start

This walkthrough discovers a cluster, generates SR-IOV Ethernet manifests, deploys them, and validates the result. It assumes `l8k` is installed and `$KUBECONFIG` points at the target cluster.

## 1. Discover

```bash
l8k discover \
  --kubeconfig "$KUBECONFIG" \
  --network-operator-release 26.4 \
  --save-cluster-config ./cluster-config.yaml
```

Discovery bootstraps a private NIC Configuration Daemon in `nvidia-k8s-launch-kit`, creates the CRDs it needs, reads `NicDevice` state, and tears the namespace down when finished. A pre-installed Network Operator is not required.

See [Cluster Discovery](discovery.md) for profile precedence, grouping, labels, rail collapsing, preset matching, and bootstrap troubleshooting.

The saved `cluster-config.yaml` includes:

- The discovered NIC and GPU topology.
- Resolved profile settings.
- Machine and GPU labels used for later group selection.
- Comments copied from the source config so the generated file remains editable.

## 2. Generate

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./deployment
```

Generation writes the Network Operator bundle to `deployment/network-operator/`. When the config came from a file, `l8k` writes resolved defaults and explicit CLI overrides back to the same file while preserving comments.

## 3. Deploy

```bash
l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

Deploy runs in phases:

1. Install or verify the `nvidia/network-operator` Helm chart from the embedded release catalog.
2. Apply `NicClusterPolicy` and wait for readiness.
3. Apply per-group `NicNodePolicy` resources and wait for readiness.
4. Apply the remaining CRs and verify each one reaches a terminal state.

Preview the server-side apply without persisting resources:

```bash
l8k deploy \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG" \
  --dry-run
```

## 4. Validate

```bash
l8k validate \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

Validation is the final acceptance stage of the normal workflow. It checks Helm release metadata, rendered values, component versions, manifest state, preflight drift, and data-plane connectivity. A successful run gives the deployment a green light and writes the supporting HTML report to:

```text
deployment/network-operator/k8s-launch-kit-validation-report.html
```

## Common Variants

Generate and deploy in one step:

```bash
l8k generate \
  --user-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./deployment \
  --deploy \
  --kubeconfig "$KUBECONFIG"
```

Run in automation mode:

```bash
l8k discover --output json 2>/dev/null | jq .
l8k generate --output json 2>/dev/null | jq .
l8k validate --output json 2>/dev/null | jq .
```

Use a known hardware preset without cluster discovery:

```bash
l8k generate \
  --for ThinkSystem-SR680a-V3-H200 \
  --node-selector "feature.node.kubernetes.io/pci-15b3.present=true" \
  --fabric ethernet \
  --deployment-type sriov \
  --save-deployment-files ./deployment
```
