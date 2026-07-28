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

## Check Stages

| Stage | What it verifies |
| --- | --- |
| Helm release | Network Operator chart appVersion and rendered user values. |
| Component versions | Version-bearing sections of `NicClusterPolicy` and `NicNodePolicy` match the selected release catalog. |
| Manifest state | Each generated CR is classified as `READY`, `IN-PROGRESS`, `ERROR`, or `MISSING`. |
| Preflight | Stray CRs and Helm value drift that can make the apply path ambiguous. |
| Connectivity | ICMP, `rping`, and `ib_write_bw` checks between generated test DaemonSet pods. |

Connectivity is skipped when manifests are missing, errored, or still in progress.

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

Override per run:

```bash
l8k validate \
  --validation-checks icmp,rping \
  --rdma-rping-iterations 20 \
  --rdma-ib-write-size 65536 \
  --rdma-ib-write-min-bandwidth-gbps 100
```

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
