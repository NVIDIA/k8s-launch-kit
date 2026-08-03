<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# CLI Reference

Run `l8k <command> --help` for the authoritative flag list. Run `l8k schema` for machine-readable capabilities.

## Commands

| Command | Purpose |
| --- | --- |
| `l8k discover` | Discover cluster network hardware and write `cluster-config.yaml`. |
| `l8k generate` | Generate deployment manifests for a selected profile. |
| `l8k deploy` | Apply previously generated manifests to a cluster. |
| `l8k validate` | Verify Helm release, component versions, manifest state, and connectivity. |
| `l8k preset list` | List local topology presets. |
| `l8k preset update` | Download topology presets from GitHub. |
| `l8k sosreport` | Collect diagnostic data from a cluster. |
| `l8k schema` | Print JSON capabilities for automation. |
| `l8k version` | Print version information. |

## Common Flags

| Flag | Applies to | Description |
| --- | --- | --- |
| `--kubeconfig` | discover, deploy, validate | Path to kubeconfig. Falls back to `$KUBECONFIG` and then `~/.kube/config`. |
| `--user-config` | discover, generate, deploy, validate | Config file to merge, render, or validate against. |
| `--config-dir` | all | Directory containing optional `l8k-config.yaml` and `presets/` overrides. |
| `--network-operator-release` | discover, generate | Release line such as `26.1`, `26.4`, or `26.7`. |
| `--network-operator-namespace` | generate, deploy, validate | Override the Network Operator namespace. It is a no-op for discovery. |
| `--output json` | all | Emit a single JSON result to stdout for automation. |
| `--quiet` | all | Suppress informational output. |
| `--log-level` | all | Enable `debug`, `info`, `warn`, or `error` logging. |
| `--log-file` | all | Write logs to a file instead of `stderr`. |

## Discover Flags

| Flag | Description |
| --- | --- |
| `--save-cluster-config` | Output path for the discovered configuration. Defaults to the `--user-config` path or `./cluster-config.yaml`. |
| `--node-selector` | Selector persisted for generated resources. It does not filter discovery scheduling. |
| `--keep-namespace` | Keep the temporary `nvidia-k8s-launch-kit` namespace and daemon workload for inspection. |
| `--collapse-nic-rails` | Collapse eligible multi-port NICs into one rail. Enabled by default; known dual-port models retain a rail per port. |
| `--image-pull-secrets` | Secret names used to pull the discovery daemon, propagated into generated policies and Helm values, and reused for authenticated Helm chart downloads when the registry host matches. |
| `--enabled-plugins` | Comma-separated plugins. The supported deployment plugin is `network-operator`. |

Discovery also accepts the profile and Spectrum-X flags below. Explicit flags override values from `--user-config` and discovered defaults.

## Profile Flags

| Flag | Description |
| --- | --- |
| `--fabric` | `ethernet` or `infiniband`. |
| `--deployment-type` | `sriov`, `rdma_shared`, or `host_device`. |
| `--multirail` | Override multirail deployment. Explicit `--multirail=false` is preserved. |
| `--routing` | `destination-based` or `source-based`. |
| `--ignore-arp` | Add tuning CNI sysctls to avoid ARP flux across pod rails. |
| `--groups` | Render only named source groups. Mutually exclusive with `--gpu-type`. |
| `--gpu-type` | Render all source groups whose GPU type matches. |
| `--for` | Generate from a topology preset. Requires `--node-selector`. |

`--groups`, `--gpu-type`, and `--for` apply to generation. The remaining profile flags apply to discovery and generation.

## Spectrum-X Flags

| Flag | Description |
| --- | --- |
| `--spectrum-x` | Enable Spectrum-X and select RA version, such as `RA2.3`. |
| `--multiplane-mode` | `none`, `swplb`, or `hwplb`. |
| `--number-of-planes` | Plane count for Spectrum-X. |
| `--topology-scheme` | `2-tier` or `3-tier` for topology-driven CIDRPool allocation. |
| `--ip-version` | `ipv4` for per-node `/31` allocation or `ipv6` for per-node `/64` allocation. |
| `--topology-file` | Path to spcx-gen/reference-generator or contract-compliant NVIDIA AIR topology JSON. The format is detected from its structure. |
| `--spectrum-x-config` | Full ConfigMap YAML or raw `data.profile` YAML. Required for RA2.3. |
| `--spectrum-x-configmap-name` | ConfigMap name when `--spectrum-x-config` is raw profile YAML. |

## Generate Flags

| Flag | Description |
| --- | --- |
| `--save-deployment-files` | Output directory for generated manifests. |
| `--network-namespaces` | Namespaces that receive secondary-network resources and example workloads. |
| `--workload-manifest` | Replace the profile's example workload with a Pod or workload-controller manifest. |
| `--enable-doca-driver` | Override `docaDriver.enable` and include the DOCA driver deployment. |
| `--image-pull-secrets` | Secret names propagated into generated Network Operator policies and Helm values. Matching credentials already present in the operator namespace authenticate the Helm chart download. |
| `--deploy` | Deploy immediately after generation. |
| `--kubeconfig` | Kubeconfig used with `--deploy`. |
| `--dry-run` | Preview the deploy stage used with `--deploy`. |
| `--overwrite-existing` | Allow convergence when deploy preflight finds Helm or managed-resource drift. |

## Deploy Flags

| Flag | Description |
| --- | --- |
| `--deployment-files` | Directory containing generated manifests. Defaults to `./deployment`. |
| `--dry-run` | Use server-side dry run. |
| `--deploy-timeout` | End-to-end deploy timeout. `0` means unbounded. |
| `--overwrite-existing` | Allow Helm upgrade when an existing Network Operator release has different values. |

## Validate Flags

| Flag | Description |
| --- | --- |
| `--connectivity` | Enable or disable data-plane connectivity checks. |
| `--validation-mode` | `quick`, `full`, or `strict`. |
| `--validation-checks` | Comma-separated list of `icmp`, `rping`, and `ib_write_bw`. |
| `--connectivity-timeout` | Wall-clock budget for connectivity workload rollout and test execution. |
| `--rdma-rping-iterations` | Override `validation.rdma.rpingIterations`. |
| `--rdma-ib-write-size` | Override `validation.rdma.ibWriteSize`. |
| `--rdma-ib-write-min-bandwidth-gbps` | Minimum `ib_write_bw` peak bandwidth. |
| `--report-path` | HTML report path. Use `-` to disable. |
| `--keep` | Keep the test DaemonSet after validation. |
| `--wait` | Wait for in-progress manifests to reach a terminal state. |

## Preset Flags

| Command and flag | Description |
| --- | --- |
| `preset list --config-dir` | List presets from a custom configuration directory instead of the embedded catalog. |
| `preset update --dir` | Destination directory for downloaded presets. |
| `preset update --repo` | Source GitHub repository. Defaults to `nvidia/k8s-launch-kit`. |
| `preset update --branch` | Source branch. Defaults to `main`. |

Set `GITHUB_TOKEN` for authenticated GitHub API requests when updating presets.

## Sosreport Flags

| Flag | Description |
| --- | --- |
| `--kubeconfig` | Cluster kubeconfig, with the same environment and home-directory fallback as other cluster commands. |
| `--output-dir` | Diagnostic output directory. Defaults to `./sosreport`. |
