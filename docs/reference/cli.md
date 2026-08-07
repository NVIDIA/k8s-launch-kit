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
| `l8k clean` | Delete Network Operator custom resources and optionally uninstall its Helm release. |
| `l8k validate` | Verify Helm release, component versions, manifest state, and connectivity. |
| `l8k preset list` | List local topology presets. |
| `l8k preset update` | Download topology presets from GitHub. |
| `l8k sosreport` | Collect diagnostic data from a cluster. |
| `l8k schema` | Print JSON capabilities for automation. |
| `l8k version` | Print version information. |

## Target selection and flag ownership

Omitting `--target` selects `host`; adding `--target host` follows the same
code path. `dpf` is a recognized target name whose phases are unavailable in
this build. Selecting it returns validation exit code `2`. This explicit
capability error prevents DPF invocations from falling through to host logic.

Host-only flags are rejected when they are explicitly supplied for another
target. Defaults are ignored by this check, including explicit-value flags
where `false` differs from omission. Run `l8k <command> --help` for target-aware
flag groups and `l8k schema` for each flag's `targets` list.

| Flag | Applies to | Target scope | Description |
| --- | --- | --- | --- |
| `--target` | discover, generate, deploy, validate, root pipeline | target-agnostic | Target name. Defaults to `host`. |
| `--kubeconfig` | discover, deploy, clean, validate | host | Path to kubeconfig. Falls back to `$KUBECONFIG` and then `~/.kube/config`. It represents the host workload cluster, not a universal multi-context input. |
| `--user-config` | discover, generate, deploy, clean, validate | host | Config file to merge, render, validate against, or use for cleanup namespace resolution. |
| `--config-dir` | all | host | Directory containing optional `l8k-config.yaml` and `presets/` overrides. |
| `--network-operator-release` | discover, generate | host | Release line such as `26.1`, `26.4`, or `26.7`. |
| `--network-operator-namespace` | generate, deploy, clean, validate | host | Override the Network Operator namespace. It is a no-op for discovery. |
| `--output json` | all | target-agnostic | Emit a single JSON result to stdout for automation. |
| `--quiet` | root pipeline | target-agnostic | Suppress informational output. |
| `--log-level` | all | target-agnostic | Enable `debug`, `info`, `warn`, or `error` logging. |
| `--log-file` | all | target-agnostic | Write logs to a file instead of `stderr`. |

The current host config and artifact contract is unchanged:
`cluster-config.yaml` remains flat and generated manifests remain under
`deployment/network-operator/`.

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
| `--multiplane-mode` | `none`, `swplb`, or `hwplb`. Defaults from GPU platform and east-west NIC: single-plane H100/H200/B200/GB200 use `none`; B300/GB300 use the GA `swplb` default. Select `hwplb` explicitly. |
| `--number-of-planes` | Plane count for Spectrum-X. Defaults to 1 for single-plane platforms and 2 for B300/GB300; pass 4 explicitly for quad-plane B300. |
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

## Clean Flags

`l8k clean` deletes every namespaced custom-resource instance in the resolved
Network Operator namespace, then deletes the known cluster-scoped Network
Operator CRs. It waits for their finalizers before uninstalling the
`network-operator` Helm release. It preserves the namespace, CRDs, unrelated
Secrets, generated files, and resources outside the namespace. Helm release
metadata and chart-managed resources are removed with the release.

Namespace resolution uses the first available source: an explicit
`--network-operator-namespace`, `networkOperator.namespace` from
`--user-config`, `./cluster-config.yaml`, or an explicit
`--config-dir/l8k-config.yaml`, then `nvidia-network-operator`. Custom
installation namespaces must be explicit in a flag or config; untrusted
in-cluster objects do not select a destructive cleanup target.

| Flag | Description |
| --- | --- |
| `--keep-helm-chart` | Delete custom resources but leave the Network Operator Helm release and chart-managed resources installed. |
| `--kubeconfig` | Cluster to clean. Falls back to `$KUBECONFIG` and then `~/.kube/config`. |
| `--user-config` | Optional config used only to read `networkOperator.namespace`. |
| `--network-operator-namespace` | Explicit cleanup namespace; takes precedence over config and the standard default. |

Cleanup is destructive and asks for confirmation in text mode. JSON mode is
non-interactive and auto-confirms, so use `--output json` only after verifying
the kubeconfig and resolved namespace. See [Cleanup](../user/cleanup.md) for the
full deletion boundary.

## Validate Flags

| Flag | Description |
| --- | --- |
| `--connectivity` | Enable or disable data-plane connectivity checks. |
| `--validation-mode` | `quick`, `full`, or `strict`. |
| `--validation-checks` | Comma-separated list of `icmp`, `rping`, and `ib_write_bw`. Enabled GPUDirect DMA-BUF validation follows the `ib_write_bw` selection. |
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
