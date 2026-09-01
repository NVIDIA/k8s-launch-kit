---
name: k8s-launch-kit-validate
version: 1.0.6
description: "Use this skill when the user wants to verify that an NVIDIA networking deployment matches the configuration that produced it. Activate for: 'is my deployment correct', 'are all the manifests applied', 'does the network operator version match', 'verify deployment', 'check cluster state against config', or any question about whether the cluster reflects what l8k generated. Wraps the `l8k validate` subcommand."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Validate

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and exit codes.

This workflow uses the default `host` target; `--target host` is equivalent.
The CLI snapshots validation arguments and runs the standalone Host validation
and report service through the target registry; checks and report semantics
are unchanged.

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
2. **Manifest state.** Every YAML manifest under
   `--deployment-files` (skipping example workloads and Helm `values.yaml`)
   is fetched from the cluster and classified by the per-Kind resource-state
   registry. `NicConfigurationTemplate` and `NicFirmwareTemplate` wait for
   the operator-populated `status.nicDevices` list to reflect current node,
   NIC type, PCI-address, serial-number, and part-number selectors and validate
   only the named devices. Their propagated template
   payload and condition `observedGeneration` must also be current; unrelated
   discovered NIC configuration state is ignored. A configuration template
   checks `FirmwareUpdateInProgress` only for a matched device with
   `spec.firmware`, so a stale firmware condition does not block a deployment
   with no `NicFirmwareTemplate`. Each manifest is reported
   `READY`, `IN-PROGRESS`, `ERROR`, or `MISSING`.
3. **Connectivity matrix.** By default, `l8k validate` applies the generated
   example DaemonSet, waits for ready pods, and runs source-bound `icmp`,
   `rping`, and `ib_write_bw` tests. When `validation.gpuDirect.enabled` is
   true and `ib_write_bw` is selected, a distinct DMA-BUF bandwidth family
   uses the connected GPU index for each source and destination rail. Profile templates declare both validation
   containers: the release-specific full-runtime DOCA image for the RDMA checks and `netshoot` for ICMP and route
   checks. Validate applies the generated DaemonSet without runtime container
   injection. The default mode is `strict`.

Validation also reports deploy-preflight drift without remediating it.
`SriovNetworkPoolConfig`, `SriovNetworkNodePolicy`, and `OVSNetwork` objects
labeled with `spectrumx.nvidia.com/owner-name` are excluded from stray results
because the Spectrum-X operator owns them as children of
`SpectrumXRailPoolConfig`.

Exit code is non-zero (4) on any missing manifest, version mismatch, or
gating connectivity failure. Version checks soft-skip when prerequisites are
absent — no `cluster-config.yaml`, no Helm release Secret, etc.

If `networkOperator.skipHelmChart: true` is set, or the user passes
`--skip-network-operator-helm`, both Helm release/version and values checks are
reported as skipped. Component-version, manifest, stray-resource, connectivity,
and report stages still run.

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
| `--skip-network-operator-helm` | `networkOperator.skipHelmChart` (`false`) | Skip Helm release/version and values validation only |
| `--validation-mode` | `validation.mode` (`strict`) | Connectivity mode: `quick`, `full`, or `strict` |
| `--validation-checks` | `validation.checks` (`icmp,rping,ib_write_bw`) | Comma-separated connectivity checks; `""` disables all |
| `--connectivity-timeout` | automatic (`0`) | Total connectivity budget is calculated from the generated matrix plan; set a positive duration for an explicit hard setup and execution deadline |
| `--rdma-rping-iterations` | `validation.rdma.rpingIterations` | rping client iteration count |
| `--rdma-ib-write-size` | `validation.rdma.ibWriteSize` | ib_write_bw message size |
| `--rdma-ib-write-min-bandwidth-gbps` | `validation.rdma.ibWriteMinBandwidthGbps` | Minimum peak Gbps; `0` disables bandwidth gating |
| `--log-level` | disabled | `debug` for structured progress and timing; `trace` also includes bounded command output |

## Connectivity Modes

- `quick`: all same-rail node pairs plus one non-gating cross-rail canary per
  source-rail/destination-rail mapping.
- `full`: every source rail × every destination rail × every ordered pod pair;
  cross-rail results are reported but do not gate pass/fail.
- `strict`: full matrix. Cross-rail gates by `profile.routing`: `source-based`
  must succeed, `destination-based` must stay isolated.

All checks are source-bound. ICMP always uses `ping -I <src-iface>` for
same-rail and cross-rail probes in every validation mode; it never binds only
the source IP. `rping` uses `-I <src-ip>`, and `ib_write_bw` uses
`--bind_source_ip <src-ip>`. GPUDirect
adds `--use_cuda=<endpoint-index> --use_cuda_dmabuf` independently on the
client and server. Treat missing or ambiguous `connectedGPU` topology as a
failure; never substitute GPU 0. Pull Secrets come from
`networkOperator.imagePullSecrets` and must exist in every validation namespace.

With the default `--connectivity-timeout=0`, validate logs one total automatic
budget after planning the matrix. The calculation reflects the enabled test
families, per-command limits, ordered pod-pair batches, bounded workload setup,
cleanup, and a safety margin. A positive flag value replaces that calculation
with a user-supplied hard deadline.

## Examples

```bash
# Defaults: ./cluster-config.yaml + ./deployment, $KUBECONFIG
l8k validate

# Explicit paths
l8k validate --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig ~/.kube/config

# Agent mode (single JSON object on stdout, logs on stderr)
l8k validate --output json 2>/dev/null | jq '.summary'

# Diagnose stage or batch progress without raw command output
l8k validate --log-level debug

# Capture bounded route, ICMP, RDMA client, and RDMA server evidence
l8k validate --log-level trace --keep
```

Debug logs show the endpoint inventory, plan, source-route cache statistics,
static checks, stages, RDMA batches, cleanup, report writes, elapsed time, and
remaining timeout. Trace adds bounded commands and per-test stdout/stderr.
Failed RDMA server logs are collected before the temporary files and test
workload are removed. Add `--keep` only when follow-up pod inspection is needed.

## Output

Text mode prints a short report:

```
Network Operator release
  selectedRelease: 26.4
  expected version: v26.4.0-beta.6
  deployed: network-operator (chart=26.4.0-beta.6 app=v26.4.0-beta.6 rev=3 status=deployed)
  result: MATCH

Manifests
  [READY      ] NicClusterPolicy/nic-cluster-policy in (cluster-scoped)
  [IN-PROGRESS] NicConfigurationTemplate/spectrum-x-config in network-operator — waiting for nic-configuration-operator to populate status.nicDevices with matched devices
  [MISSING    ] SriovNetwork/sriov-network-rail-0 in default — not found in cluster
  ...

Summary: 1/3 ready, 1 in-progress, 0 error, 1 missing; version: match; topology mismatches: 0 group(s)
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
