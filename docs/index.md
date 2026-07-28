<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA Kubernetes Launch Kit

NVIDIA Kubernetes Launch Kit (`l8k`) generates, deploys, and validates NVIDIA cloud-native networking manifests for Kubernetes clusters. It discovers NIC and GPU topology, selects a deployment profile, renders Network Operator and NIC Configuration Operator resources, applies them in dependency order, and verifies the result with live manifest and data-plane checks.

Use this site when you are deploying SR-IOV, RDMA shared-device, host-device, InfiniBand, or Spectrum-X networking on NVIDIA accelerated clusters.

## Find Your Path

| If you are a... | Start here |
| --- | --- |
| Operator deploying networking on a cluster | [Quick Start](user/quick-start.md) |
| Platform engineer selecting a topology profile | [Deployment Profiles](user/profiles.md) |
| Spectrum-X operator | [Spectrum-X](user/spectrum-x.md) |
| CI/CD or GitOps integrator | [Automation](integrator/automation.md) |
| User debugging an applied deployment | [Validation](user/validation.md) |
| Maintainer updating the docs site | [Documentation Publishing](reference/docs-publishing.md) |

## Workflow

```text
[ Discover ] ---> [ Generate ] ---> [ Deploy ] ---> [ Validate ]
 hardware          manifests         apply           live state
 inventory         + values          in order        + data plane
```

Each stage is independently invocable:

- `l8k discover` bootstraps a private NIC discovery daemon and writes `cluster-config.yaml`.
- `l8k generate` renders a profile-specific manifest bundle under `deployment/network-operator/`.
- `l8k deploy` installs or upgrades the Network Operator Helm chart, applies CRs in dependency order, and waits for reconciliation.
- `l8k validate` checks Helm version and values, component versions, manifest state, preflight drift, and configurable ICMP/RDMA connectivity.

## What The Current Docs Cover

These standalone docs include features added across recent release lines:

- Self-contained discovery without a pre-installed Network Operator.
- Hardware-derived machine/GPU labels, group filtering, and heterogeneous cluster merging.
- One-rail-per-NIC discovery, north-south-only group filtering, and exact-match topology presets.
- Network Operator release catalog selection for `25.10`, `26.1`, `26.4`, and `26.7`.
- NicNodePolicy rendering, maintenance concurrency, and Network Operator Helm install through `l8k deploy`.
- Multi-namespace secondary network fan-out, source-based routing, ARP tuning, NV-IPAM exclusions, and custom workload manifests.
- Spectrum-X RA2.1, RA2.2, and RA2.3 flows, including ConfigMap-backed profiles, topology-driven CIDRPools, and optional DRA ResourceClaimTemplates.
- Configurable validation modes, ICMP/RDMA check selection, bandwidth gates, and self-contained HTML reports.
- Machine-readable JSON output and the `l8k schema` command for automation and AI agents.

## Links

- [GitHub repository](https://github.com/NVIDIA/k8s-launch-kit)
- [Releases](https://github.com/NVIDIA/k8s-launch-kit/releases)
- [CLI reference](reference/cli.md)
- [Configuration reference](reference/configuration.md)
