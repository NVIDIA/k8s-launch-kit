<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

<p class="page-kicker">Overview</p>

# NVIDIA Kubernetes Launch Kit

NVIDIA Kubernetes Launch Kit (`l8k`) generates, deploys, and validates NVIDIA cloud-native networking manifests for Kubernetes clusters. It discovers NIC and GPU topology, selects a deployment profile, renders Network Operator and NIC Configuration Operator resources, applies them in dependency order, and verifies the result with live manifest and data-plane checks.

Use this site when you are deploying SR-IOV, RDMA shared-device, host-device, InfiniBand, or Spectrum-X networking on NVIDIA accelerated clusters.

## Find Your Path

| If you are a... | Start here |
| --- | --- |
| Operator deploying networking on a cluster | [Quick Start](user/quick-start.md) |
| Operator inventorying a cluster | [Cluster Discovery](user/discovery.md) |
| Platform engineer selecting a topology profile | [Deployment Profiles](user/profiles.md) |
| Platform engineer managing mixed hardware | [Heterogeneous Clusters](user/heterogeneous-clusters.md) |
| Spectrum-X operator | [Spectrum-X](user/spectrum-x.md) |
| CI/CD or GitOps integrator | [Automation](integrator/automation.md) |
| AI agent integrator | [AI Skills](integrator/ai-skills.md) |
| Operator confirming a deployment is ready for use | [Validation](user/validation.md) |
| Operator removing a deployment | [Cleanup](user/cleanup.md) |
| Operator investigating a failed stage | [Troubleshooting](user/troubleshooting.md) |

## Workflow

<div class="workflow-diagram" role="img" aria-label="Launch Kit workflow: discover hardware, generate manifests, deploy resources, then validate the deployment">
  <div class="workflow-step">
    <span class="workflow-step__command">Discover</span>
    <span class="workflow-step__description">Hardware inventory</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Generate</span>
    <span class="workflow-step__description">Manifests and values</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Deploy</span>
    <span class="workflow-step__description">Ordered application</span>
  </div>
  <span class="workflow-arrow" aria-hidden="true">→</span>
  <div class="workflow-step">
    <span class="workflow-step__command">Validate</span>
    <span class="workflow-step__description">Acceptance report</span>
  </div>
</div>

Each stage is independently invocable:

- `l8k discover` bootstraps a private NIC discovery daemon and writes `cluster-config.yaml`.
- `l8k generate` renders a profile-specific manifest bundle under `deployment/network-operator/`.
- `l8k deploy` installs or upgrades the Network Operator Helm chart, applies CRs in dependency order, and waits for reconciliation.
- `l8k validate` runs the deployment acceptance checks and produces the report used to green-light the deployment.
- `l8k clean` removes Network Operator custom resources and then uninstalls its Helm release.

## Links

- [GitHub repository](https://github.com/NVIDIA/k8s-launch-kit)
- [Releases](https://github.com/NVIDIA/k8s-launch-kit/releases)
- [CLI reference](reference/cli.md)
- [Configuration reference](reference/configuration.md)
