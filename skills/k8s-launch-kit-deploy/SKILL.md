---
name: k8s-launch-kit-deploy
version: 1.1.0
description: "Use this skill when the user wants to deploy generated NVIDIA networking manifests to a Kubernetes cluster using k8s-launch-kit (l8k). Activate for: applying manifests, deploying to cluster, the --deploy flag, applying generated files, or any mention of pushing l8k output to a live cluster. Even if the user just says 'apply these' or 'push to cluster' after generating manifests, use this skill."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Deploy

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Apply generated NVIDIA networking manifests to a Kubernetes cluster.

## Usage

```bash
l8k generate --user-config <CONFIG> --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <DIR> --deploy [--kubeconfig <PATH>]
```

## Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--deploy` | Yes | Enable deployment to cluster |
| `--kubeconfig` | — | Path to kubeconfig with cluster-admin access (optional — falls back to `$KUBECONFIG`) |
| `--dry-run` | — | Preview what would be applied without making changes |

All profile selection flags from `k8s-launch-kit-generate` apply.

## Examples

```bash
# Generate + deploy SR-IOV Ethernet
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --deploy --kubeconfig ~/.kube/config

# Discover + generate + deploy Spectrum-X (full pipeline via root command)
l8k --discover-cluster-config \
  --kubeconfig ~/.kube/config \
  --spectrum-x --spcx-version RA2.1 \
  --save-deployment-files ./output \
  --deploy

# Agent mode
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --deploy --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null
```

## Resource Apply Order

l8k applies resources in dependency order:

1. NicClusterPolicy (OFED, NIC config, secondary network)
2. IPPool (NV-IPAM address allocation)
3. SriovNetworkNodePolicy / HostDeviceNetwork / MacvlanNetwork
4. SriovNetwork / IPoIBNetwork
5. NicInterfaceNameTemplate (if needed)
6. Test pod (optional)

## Post-Deploy Verification

```bash
kubectl get nicclusterpolicy -o yaml          # Check policy state
kubectl get pods -n <operator-ns>             # Verify all pods Running
kubectl get sriovnetworknodestates -A          # Check SR-IOV VF allocation
```

> [!CAUTION]
> This is a **write** command — confirm with the user before executing on production clusters.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and exit codes
- [k8s-launch-kit-dryrun](../k8s-launch-kit-dryrun/SKILL.md) — Preview before deploying
- [k8s-launch-kit-troubleshoot](../k8s-launch-kit-troubleshoot/SKILL.md) — Debug deployment failures
