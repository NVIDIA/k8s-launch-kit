# Sosreport Directory Structure Reference

This document describes every directory and file type found in an l8k sosreport.
Use this as a map when navigating diagnostic dumps.

## Full Directory Tree

```
sosreport-dir/
├── metadata/
│   ├── cluster-version.txt          — Kubernetes server version (major, minor, gitVersion)
│   ├── cluster-version.yaml         — Full version info as YAML (for programmatic use)
│   ├── namespaces.txt               — List of all namespaces in the cluster
│   ├── collection-info.txt          — Timestamp, l8k version, collection parameters
│   └── api-resources.txt            — All registered API resources (kubectl api-resources output)
│
├── crds/
│   ├── definitions/                 — CRD schemas (CustomResourceDefinition YAML)
│   │   ├── nicclusterpolicies.mellanox.com.yaml
│   │   ├── hostdevicenetworks.mellanox.com.yaml
│   │   ├── ipoibnetworks.mellanox.com.yaml
│   │   ├── macvlannetworks.mellanox.com.yaml
│   │   ├── sriovnetworks.sriovnetwork.openshift.io.yaml
│   │   ├── sriovnetworknodepolicies.sriovnetwork.openshift.io.yaml
│   │   ├── sriovnetworknodestates.sriovnetwork.openshift.io.yaml
│   │   ├── sriovnetworkpoolconfigs.sriovnetwork.openshift.io.yaml
│   │   ├── sriovoperatorconfigs.sriovnetwork.openshift.io.yaml
│   │   ├── network-attachment-definitions.k8s.cni.cncf.io.yaml
│   │   ├── nodefeatures.nfd.k8s-sigs.io.yaml
│   │   ├── nodefeaturerules.nfd.k8s-sigs.io.yaml
│   │   ├── nodemaintenances.maintenance.nvidia.com.yaml
│   │   └── maintenanceoperatorconfigs.maintenance.nvidia.com.yaml
│   │
│   └── instances/                   — Deployed custom resource objects
│       ├── hostdevicenetworks/
│       │   └── all.yaml             — All HostDeviceNetwork CRs
│       ├── network-attachment-definitions/
│       │   └── all.yaml             — All NetworkAttachmentDefinitions across namespaces
│       ├── nodefeatures/
│       │   └── all.yaml             — NodeFeature CRs (PCI device info, NUMA, firmware)
│       ├── sriovnetworknodestates/
│       │   └── all.yaml             — SR-IOV node states (VF allocation, sync status)
│       ├── sriovnetworkpoolconfigs/
│       │   └── all.yaml             — SR-IOV pool configurations
│       └── sriovoperatorconfigs/
│           └── all.yaml             — SR-IOV operator configuration
│
├── operator/
│   ├── namespace.yaml               — Operator namespace spec and labels
│   ├── configmaps.yaml              — ConfigMaps in the operator namespace
│   ├── events.yaml                  — Kubernetes events in the operator namespace
│   ├── secrets-metadata.txt         — Secret names and types (values redacted)
│   │
│   ├── components/
│   │   ├── network-operator/        — Main Network Operator controller
│   │   │   ├── deployment.yaml      — Deployment spec (replicas, image, env vars)
│   │   │   └── pods/
│   │   │       ├── <pod-name>.yaml  — Pod spec (status, conditions, restart count)
│   │   │       └── <pod-name>.log   — Container logs (reconciliation, errors)
│   │   │
│   │   ├── ofed-driver/             — OFED/DOCA driver DaemonSet pods
│   │   │   └── pods/
│   │   │       ├── <pod-name>.yaml  — Pod spec with init containers and mounts
│   │   │       └── <pod-name>.log   — Driver loading logs, module conflicts
│   │   │
│   │   ├── sriov-network-config-daemon/  — SR-IOV config daemon pods
│   │   │   └── pods/
│   │   │       ├── <pod-name>.yaml
│   │   │       └── <pod-name>.log   — VF creation, device plugin registration
│   │   │
│   │   ├── nv-ipam-node/            — NV-IPAM node agent pods
│   │   │   └── pods/
│   │   │       ├── <pod-name>.yaml
│   │   │       └── <pod-name>.log   — IP allocation, pool sync
│   │   │
│   │   └── nic-configuration-operator/  — NIC configuration operator pods
│   │       └── pods/
│   │           ├── <pod-name>.yaml
│   │           └── <pod-name>.log   — NIC renaming, firmware config
│   │
│   ├── rbac/
│   │   ├── roles.yaml               — Roles in operator namespace
│   │   ├── rolebindings.yaml        — RoleBindings in operator namespace
│   │   └── serviceaccounts.yaml     — ServiceAccounts in operator namespace
│   │
│   └── webhooks/
│       ├── validatingwebhookconfigurations.yaml
│       └── mutatingwebhookconfigurations.yaml
│
├── nodes/
│   ├── all-nodes.yaml               — Full node specs (capacity, allocatable, conditions)
│   ├── node-labels.txt              — All node labels (useful for selector debugging)
│   ├── node-resources.txt           — Allocatable resources per node (device plugin resources)
│   └── nodes-summary.txt            — Condensed node info (name, status, roles, version)
│
├── network/
│   └── services.yaml                — Services in operator namespace
│
├── collection-errors.log            — Any errors encountered during sosreport collection
├── report.html                      — HTML-formatted diagnostic report (if generated)
└── diagnostic-summary.txt           — START HERE: high-level cluster health overview
```

## How to Read Each Section

### diagnostic-summary.txt

Always start here. Contains:
- Cluster version and node count
- Component health summary (which pods are running, crashed, or missing)
- Detected issues and warnings
- Quick statistics (VF counts, resource allocations, event counts)

### metadata/

Context for the collection. Use `cluster-version.txt` to verify the Kubernetes version
matches expected values. Use `api-resources.txt` to confirm required CRDs are registered.

### crds/definitions/

The CRD schemas installed in the cluster. Useful for verifying that the correct Network
Operator version is installed (CRD versions change between releases).

### crds/instances/

The actual deployed custom resources. Key files to check:
- **NicClusterPolicy** -- the main configuration object. Check `.status.conditions` for
  readiness and error messages.
- **SriovNetworkNodeState** -- per-node SR-IOV state. Check `.status.syncStatus` and
  `.status.interfaces` for VF allocation.
- **NetworkAttachmentDefinitions** -- verify they exist in the expected namespace with
  the correct config.

### operator/components/

Per-component pod specs and logs. For each component:
1. Check the pod `.yaml` for `status.phase`, `status.conditions`, and
   `status.containerStatuses[].restartCount`
2. Read the `.log` file for errors, searching for `error`, `failed`, `panic`, `fatal`

### nodes/

Node-level information. Key checks:
- `node-labels.txt` -- verify `feature.node.kubernetes.io/pci-15b3.present=true` exists
- `node-resources.txt` -- verify device plugin resources appear in allocatable
  (e.g., `nvidia.com/sriov_resource: 8`)
- `all-nodes.yaml` -- check node conditions for `Ready` status

### operator/events.yaml

Kubernetes events in the operator namespace. Filter for `type: Warning` to find
issues. Common event reasons: `FailedCreate`, `BackOff`, `Unhealthy`, `SyncError`.
