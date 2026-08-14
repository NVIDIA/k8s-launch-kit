<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Validation

`l8k validate` is the final acceptance stage of the deployment workflow. It verifies that generated manifests match the live cluster, runs configurable data-plane checks, and produces the report used to green-light the deployment.

```bash
l8k validate \
  --user-config ./cluster-config.yaml \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

When `--user-config` is omitted, Launch Kit checks `./cluster-config.yaml`, the parent of `--deployment-files`, and then the deployment directory. It uses that file for the selected release, operator namespace, profile, discovered groups, and validation settings.

## Check Stages

| Stage | What it verifies |
| --- | --- |
| Helm release | Network Operator chart appVersion and rendered user values. |
| Component versions | Version-bearing sections of `NicClusterPolicy` and `NicNodePolicy` match the selected release catalog. |
| Manifest state | Each generated CR is classified as `READY`, `IN-PROGRESS`, `ERROR`, or `MISSING`. |
| Preflight | Stray CRs and Helm value drift that can make the apply path ambiguous. |
| Connectivity | ICMP, `rping`, host-memory `ib_write_bw`, and optional GPUDirect DMA-BUF bandwidth checks between generated test DaemonSet pods. |

Connectivity is skipped when manifests are missing, errored, or still in progress.

Each generated example DaemonSet declares two validation containers: the DOCA
container runs `rping`, `ib_write_bw`, and DMA-BUF bandwidth, while the `netshoot` container runs
ICMP and route checks from the same pod network namespace. Validation applies
the generated DaemonSet as written; it does not inject a helper container at
runtime.

Manifest-state checks for `NicConfigurationTemplate` and `NicFirmwareTemplate` use the operator-populated `status.nicDevices` list as the matched device set. An empty list, a list that does not yet reflect the current node, NIC type, PCI-address, serial-number, and part-number selectors, a missing named `NicDevice`, a device spec that does not yet reflect the current template payload, or a relevant device condition with a stale `observedGeneration` remains `IN-PROGRESS`. `NicConfigurationTemplate` considers `FirmwareUpdateInProgress` relevant only when the matched device has `spec.firmware`; a stale firmware condition cannot block a configuration-only deployment. Unrelated discovered devices are used only to verify selector freshness; their configuration and firmware state is ignored.

Preflight uses the same checks as deployment: Helm chart version, generated Helm values, component versions, and stray l8k-managed CRs. SR-IOV pool configs, node policies, and OVS networks labeled with `spectrumx.nvidia.com/owner-name` are controller-owned outputs of `SpectrumXRailPoolConfig`, so they are not reported as strays. Validation never remediates drift.

## Validation Modes

| Mode | Coverage | Cross-rail gating |
| --- | --- | --- |
| `quick` | Same-rail all nodes plus one cross-rail canary per rail pair. | Non-gating. |
| `full` | Every source rail x destination rail pair. | Non-gating. |
| `strict` | Full matrix. | Gated by `profile.routing`. Source-based routing must pass; destination-based routing must stay isolated. |

```bash
l8k validate --validation-mode quick
l8k validate --validation-mode full
l8k validate --validation-mode strict
```

## Check Selection

Fresh discovery writes:

```yaml
validation:
  gpuDirect:
    enabled: false
    gpuResourceType: nvidia.com/gpu
  connectivity: true
  mode: strict
  checks:
    - icmp
    - rping
    - ib_write_bw
  rdma:
    rpingIterations: 5
    ibWriteSize: 65536
    ibWriteMinBandwidthGbps: 100
```

`gpuDirect.enabled` is never omitted. Discovery sets it to `true` only when
every worker in every discovered group can satisfy its render bucket's
topology-derived `gpuResourceType` request; otherwise it writes `false`. You may
change the value before generation. GPUDirect runs as a distinct result family whenever
it is enabled and `ib_write_bw` is selected.

The generated validation DaemonSet selects its full-runtime DOCA image from
the Network Operator release catalog and copies
`networkOperator.imagePullSecrets` into the Pod spec. Create those Secrets in
every network namespace used by validation. Only the DOCA container requests
the configured GPU resource.

For each test, Launch Kit maps the source rail and destination rail to their
own `connectedGPU` value from discovery or the selected topology preset. It
then passes `--use_cuda=<source-index> --use_cuda_dmabuf` to the client and
`--use_cuda=<destination-index> --use_cuda_dmabuf` to the server. Missing or
ambiguous mappings fail; GPU 0 is never assumed. Text, JSON, and HTML output
keep DMA-BUF results separate and include indices, PCI addresses when known,
bandwidth, threshold, and errors.

Override per run:

```bash
l8k validate \
  --validation-checks icmp,rping \
  --connectivity-timeout 10m \
  --rdma-rping-iterations 20 \
  --rdma-ib-write-size 65536 \
  --rdma-ib-write-min-bandwidth-gbps 100
```

## Connectivity Timeout

By default, `--connectivity-timeout=0` selects an automatic timeout. Launch Kit
first applies and discovers the generated validation workload under a bounded
setup allowance. Once the matrix is known, it calculates the total budget from
the selected tests, their individual command limits, ordered pod-pair batches,
cleanup allowances, and a safety margin. The log reports the calculated total
before connectivity test execution starts:

```text
Connectivity timeout automatically calculated from 144 planned tests: 2h10m24s total budget
```

Set a positive duration to replace the automatic budget with an explicit hard
deadline for connectivity workload setup and execution:

```bash
l8k validate --connectivity-timeout 45m
```

The explicit deadline is useful for fitting validation into an external
maintenance or CI window. Test DaemonSet and RDMA-process cleanup use short,
independent best-effort contexts so cleanup is still attempted after either an
automatic or user-supplied deadline expires.

Disable only the connectivity stage:

```bash
l8k validate --connectivity=false
```

## Report

By default, validation writes a self-contained HTML report beside the generated manifests:

```text
deployment/network-operator/k8s-launch-kit-validation-report.html
```

Override or disable it:

```bash
l8k validate --report-path ./reports/validation.html
l8k validate --report-path=-
```

The report includes release checks, component checks, manifest state, live YAML dropdowns, topology/preset comparison, connectivity matrices, and warnings.

| Report section | Content |
| --- | --- |
| Environment | Launch Kit version, timestamp, API server, operator namespace, and kubeconfig context. |
| Profile | Fabric, deployment type, multirail, routing, and Spectrum-X settings. |
| Node groups | Machine/GPU identity, labels, capabilities, worker nodes, PF inventory, and paired actual/expected preset topology. |
| Release and preflight | Selected and deployed chart, values, component versions, and stray-resource results. |
| Manifest state | State, reason, kind-specific details, and live YAML with `managedFields` removed. |
| Connectivity | Same-rail and cross-rail ICMP/RDMA matrices with bandwidth observations. |
| Warnings | Skipped stages, in-progress resources, and other non-success conditions. |

The report is one HTML file with inline styling and no external runtime dependency, so it can be opened offline or attached to an approved diagnostic record.

## Acceptance Outcomes

- Exit `0`: all gating checks passed.
- Exit `0` with warnings: no error/missing resources, but at least one resource is still in progress and `--wait` was not used. Connectivity is skipped.
- Exit `4`: a manifest is missing or errored; release, values, or component checks mismatch; stray resources exist; certified preset topology differs; or a gating connectivity check fails.

Use `--wait <duration>` to turn an in-progress snapshot into a bounded acceptance wait.

## When Validation Does Not Pass

Keep the test DaemonSet:

```bash
l8k validate --keep
```

Wait for in-progress manifests:

```bash
l8k validate --wait 10m
```

Use JSON output for automation:

```bash
l8k validate --output json 2>/dev/null | jq .
```

Continue with [Troubleshooting](troubleshooting.md) to investigate the failed stage while preserving the report and test resources as evidence.
