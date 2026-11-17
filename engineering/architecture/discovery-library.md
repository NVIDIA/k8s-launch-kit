<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Discovery library

This reference is for Go library consumers, not CLI operators.

## Read-only library discovery

Library consumers that only need the inventory from an already-running NIC
Configuration Daemon can call `networkoperatorplugin.DiscoverReadOnly`. The
function reads
the existing daemon pods, nodes, and `NicDevice` resources and returns a
`LaunchKitConfig` without creating, patching, or deleting Kubernetes resources.
It locates the existing daemon by its DaemonSet owner in the configured
Network Operator namespace when `WithReadOnlyNetworkOperatorNamespace` is
provided. Without that option, it checks the standard Network Operator and
Launch Kit namespaces, preferring a usable Network Operator daemon when both
installations are present.
It returns `networkoperatorplugin.ErrNotInstalled` when the discovery daemon is
not already running. With the configured-namespace option, callers need pod
read access only in that namespace; otherwise they need pod-list access in all
fallback namespaces. Both paths need cluster-scoped read access to nodes and
all-namespaces read permission for the namespaced `NicDevice` resources. Use
`discovery.Discover` when the caller is responsible for bootstrapping the
temporary daemon.

The function does not use node labels to guess whether a daemon node can
publish a `NicDevice`. If every Ready daemon node already has a device, no pod
execution is needed. If any device is missing, pass
`networkoperatorplugin.WithReadOnlyRESTConfig` with the REST config for the
cluster. The function then runs the existing read-only NIC and BlueField trust
probe in that node's daemon pod. It excludes nodes with no discoverable NIC or
only restricted BlueFields, and reports a missing device for a verified NIC
publisher. It returns inspection and permission errors instead of treating
them as absent hardware. This fallback needs `pods/exec` permission in the
daemon namespace.

Without a REST config, discovery returns an error by default. A caller that
accepts an incomplete inventory can opt in with
`networkoperatorplugin.WithReadOnlyAssumeUnverifiedExcluded()`. Ready daemon
nodes without a `NicDevice` are then excluded and a warning reports that their
publisher status is unverified. This option does not suppress errors from a
probe attempted with a REST config. At least one `NicDevice` on a Ready daemon
node is still required to construct an inventory.

For optional fabric enrichment, pass both
`networkoperatorplugin.WithReadOnlyRESTConfig(restConfig)` and
`networkoperatorplugin.WithReadOnlyFabricDiscovery()`. This explicitly permits
read-only execs in the existing daemon pods and needs `pods/exec` permission in
the selected daemon namespace. Supplying only a REST config retains the existing
publisher-verification behavior and does not enable fabric probes. The fabric
option without a REST config fails before cluster reads.

Fabric enrichment checks every selected worker and east-west PF, including sibling
PFs represented by a collapsed rail. One batched command per worker confirms
PCI-local RDMA names, enumerates all ports and reads their configured `link_layer`.
Only complete, unanimous observations set a group's `linkType`; partial, denied,
unreadable or conflicting observations leave it unknown with diagnostics. Link
state and subnet-manager presence do not gate fabric detection. An InfiniBand PF
without a netdev gets a separate IPoIB host-provisioning warning.

Normal discovery uses the same all-worker fabric check. Collection allows four
concurrent workers with a ten-second deadline per worker and no timeout retry.
Its optional worst-case budget is approximately `ceil(selected workers / 4) * 10s`
plus API/dispatch overhead, capped by the caller's deadline. Internal probe
failures leave fabric unknown; caller cancellation or deadline returns an error.
Neither optional enrichment nor partial-inventory opt-in suppresses publisher
inspection errors when REST credentials are supplied. These observations prove
coverage of a successfully enumerated sysfs snapshot, not absent kernel hardware
or deployment readiness. No host modules, node labels or Kubernetes resources are
changed by read-only discovery.

GPU product/topology enrichment is separately opt-in:
`networkoperatorplugin.WithReadOnlyGPUTopology("gpu-operator")` requires the
actual GPU Operator namespace and `WithReadOnlyRESTConfig(restConfig)`.
Default calls and REST-only calls do not acquire GPU probes. This option reads
host PCI identities in existing NIC daemon pods and tries `nvidia-smi` there,
then uses a verified same-node GPU Operator `gpu-feature-discovery` container
when necessary. It does not run the netplan workflow or modify either daemon.
Callers need pod read/exec access in both namespaces and DaemonSet read access
in the GPU Operator namespace. Missing namespace or REST credentials fails
before cluster reads. Optional failures leave unavailable GPU fields unset;
caller cancellation remains an error. Supply and consume a `pkg/ui` context
sink to receive these warnings; the library's default sink is silent.

By default, read-only discovery also fails if it cannot derive a node selector
that matches only a group's workers. Inventory-only callers can pass
`networkoperatorplugin.WithReadOnlyAllowMissingNodeSelector()` to return the
topology in that case. The affected group's `NodeSelector` is left empty and
`WorkerNodes` is authoritative; do not use that empty selector for workload
placement. This option does not relax errors for missing or unverified
`NicDevice` resources.
