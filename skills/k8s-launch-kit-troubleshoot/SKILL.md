---
name: k8s-launch-kit-troubleshoot
version: 1.1.0
description: "Use this skill when the user has problems with NVIDIA Network Operator on Kubernetes, or wants to analyze a sosreport diagnostic dump. Activate for: OFED driver crashes, SR-IOV pods failing, NicClusterPolicy errors, network operator pod issues, RDMA not working, NIC configuration failures, pods stuck in CrashLoopBackOff or ContainerCreating with network annotations, VF allocation issues, or when the user mentions 'troubleshoot', 'debug', 'sosreport', 'diagnose', or describes any NVIDIA networking failure -- even if they don't explicitly ask for troubleshooting."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Troubleshooting

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and exit codes.

Debug NVIDIA Network Operator issues on Kubernetes, with or without a sosreport.

## l8k Troubleshooting Commands

```bash
# Collect a diagnostic dump from the cluster
l8k sosreport [--kubeconfig <PATH>] --output-dir ./sosreport
```

`l8k sosreport` gathers cluster state, CRDs, operator logs, and per-node NIC info into a structured directory for offline analysis. Use the diagnostic commands and triage workflow below to interpret the dump.

## Diagnostic Commands

```bash
# NicClusterPolicy status (check state: ready vs notReady)
kubectl get nicclusterpolicy -o yaml

# Network operator pods
kubectl get pods -n <operator-ns> -o wide

# SR-IOV node states (VF allocation)
kubectl get sriovnetworknodestates -A -o yaml

# OFED driver pod logs
kubectl logs -n <operator-ns> -l app=mofed-<os> --tail=100

# NIC configuration daemon logs
kubectl logs -n <operator-ns> -l app=nic-configuration-daemon --tail=100

# Check for pods stuck on network resources
kubectl get pods -A -o wide | grep -E 'ContainerCreating|Init'
```

## Common Failure Patterns

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| NicClusterPolicy `state: notReady` | OFED driver pods failing | Check mofed pod logs, verify kernel/driver compatibility |
| Pods stuck in `ContainerCreating` | VFs not allocated or SR-IOV policy not applied | Check `sriovnetworknodestates`, verify device plugin pods |
| `CrashLoopBackOff` on mofed pods | Kernel module conflict | Check `thirdPartyRDMAModules`, enable `unloadThirdPartyRDMAModules` |
| No VFs on node | SriovNetworkNodePolicy not matching | Verify `nodeSelector` labels match worker nodes |
| RDMA not working | Missing RDMA device plugin or wrong resource name | Check `rdma-shared-dp` pods, verify resource annotations |
| NIC config daemon not starting | Operator namespace mismatch | Verify `--network-operator-namespace` matches actual namespace |
| IPPool not allocating | NV-IPAM subnet exhausted or misconfigured | Check `ippools` CR status, verify CIDR ranges |
| `--for requires --node-selector` | `--for` was passed without `--node-selector` | Add `--node-selector key=val,…`. The synthesized clusterConfig has no live worker-node list; the selector identifies target nodes at apply time. |
| `--for and --discover-cluster-config are mutually exclusive` | Both flags passed simultaneously | Pick one: `--for` skips discovery, `--discover-cluster-config` runs it. |
| `unknown preset "X"; available: …` | `--for X` doesn't match any directory under `presets/` | Run `l8k preset list` and re-run with one of those names. |
| `preset has no capabilities block` | Preset YAML used by `--for` is missing `capabilities.nodes.{sriov,rdma,ib}` | Add the block to the preset's `topology.yaml`. Discovery-time overlay does not require it; only `--for` does. |
| `unknown field "productType"` in YAML | Hand-authored config still uses the old key name | Rename `productType:` to `gpuType:` (the field was renamed). |

For detailed triage workflow, read `references/troubleshooting-guide.md`.

## sosreport Analysis

If the user has a pre-collected sosreport directory (from `l8k sosreport` or manual collection):

```
sosreport/
├── metadata/          # Cluster info, node list
├── crds/              # NicClusterPolicy, SriovNetworkNodePolicy, IPPool, etc.
├── operator/          # Network operator pod logs
├── nodes/             # Per-node device info
└── network/           # Interface config, routing tables
```

### Triage Checklist

1. Read `metadata/diagnostic-summary.yaml` for overview
2. Check pod health in `operator/pods.yaml`
3. Inspect CRDs in `crds/` for status fields
4. Read operator logs in `operator/logs/` for errors
5. Check per-node NIC state in `nodes/<node>/`

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Exit codes and error structure
- [k8s-launch-kit-discover](../k8s-launch-kit-discover/SKILL.md) — Re-discover to verify hardware state
- `references/troubleshooting-guide.md` — Detailed triage workflow
