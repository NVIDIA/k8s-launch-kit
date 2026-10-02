<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Validation

`l8k validate` checks generated manifests against the live cluster, runs
configured data-plane checks, and writes an acceptance report. Read the
[acceptance outcomes](#acceptance-outcomes) together with the test coverage:
exit `0` alone does not prove that every requested stage ran.

```bash
l8k validate \
  --deployment-files ./deployment \
  --kubeconfig "$KUBECONFIG"
```

When `--user-config` is omitted, deploy and validate prefer the bundle's
`.l8k/resolved-config.yaml` over conventional cluster config files. See the
[exact lookup order](../reference/configuration.md#deploy-and-validate-config-lookup).
The sidecar preserves the release, namespace, profile, groups, and validation
settings used during generation. Connectivity accepts this resolved metadata
or a user-owned configuration with the required explicit decisions; installed
and embedded defaults alone do not satisfy that requirement.

## Acceptance Outcomes

| Invocation | Exit and coverage behavior |
| --- | --- |
| Full Kubernetes validation | Failed static gates or gating connectivity tests return `4`. In-progress manifests can return `0` with warnings and skip connectivity. A skipped matrix (for example, fewer than two usable pods) or missing selected-family coverage does not independently fail this mode. |
| Connectivity-only validation | Requires at least one gating test for every selected family and all gating tests to pass. Skipped, empty, or incomplete coverage returns `4`. |
| OpenShift with connectivity enabled | Enforces the same per-family coverage requirement; in-progress manifests that prevent connectivity return `4`. |
| `--connectivity=false` | Runs static checks only. Success supplies no data-plane evidence. |

Missing/errored manifests, release or values/component mismatches, stray
resources, and certified-preset deviations are failure gates in full
validation. Missing or malformed test DaemonSets are input errors (exit `2`)
when connectivity is enabled, before cluster checks. `validation.checks: []`
disables all test families; it does not demonstrate connectivity.

Use `--wait <duration>` to poll in-progress manifests. Kubernetes resources
still in progress after that wait remain warnings under the current behavior;
the wait deadline alone is not a failure gate. Before accepting a deployment,
require the report to show the intended manifests ready, the required static
checks completed, and gating tests for every required data-plane family.
Review skipped stages and non-gating cross-rail observations separately.

## Worked acceptance decisions

The following examples are illustrative decisions, not records of a live
qualification run. Compare the intended worker and rail set with the report's
usable test pods and actual matrix coverage before accepting a deployment.

| Example | Process result and report | Decision |
| --- | --- | --- |
| Complete two-worker, one-rail check | Intended manifests are `READY`; required static checks complete; both workers provide usable test pods; every required selected family has completed gating tests on the intended rail with no gating failures. | Accept for those stated checks and topology, subject to the site's thresholds and exclusions. |
| Incomplete coverage | Kubernetes full validation exits `0`, but a manifest is still in progress or only one of the two intended workers has a usable test pod. Connectivity is skipped or its matrix omits the second worker. | Do not accept a deployment that requires two-worker data-plane evidence. Inspect the warning and missing endpoint, then rerun. |
| Failed gate | A release or manifest check fails, or a gating connectivity test fails; validation returns `4`. | Investigate the failing stage and preserve the report before retrying. |

Non-gating cross-rail observations, an empty selection such as
`validation.checks: []`, or a static-only success do not prove required
connectivity. `--wait` can give reconciliation more time; on Kubernetes,
exhausting that wait does not by itself change an in-progress warning into a
failure. Document any site-approved exclusions outside Launch Kit and assess
coverage against that intended scope.

## Report

By default, validation writes a self-contained HTML report at the root of
`--deployment-files` (default `./deployment`), even when generated manifests
are under its `network-operator/` subdirectory:

```text
deployment/k8s-launch-kit-validation-report.html
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

## JUnit XML

Write a JUnit report alongside text, JSON and the HTML report:

```bash
l8k validate --output json --junit-path ./reports/validation.xml > validation.jsonl
```

`--junit-path` is opt-in and independent of `--report-path`. Parent directories
are created. The file is replaced atomically after validation, including early
errors and partial results. Failure to write a requested file fails an otherwise successful command; if
validation already failed, its original exit code is preserved and the write
error is logged separately.
Preserve the exit status as well as the report. The existing JSON stream is
unchanged; XML is written only to the requested file.

Connectivity reporting requires `profile.fabric: ethernet` or `infiniband`
in the resolved cluster config. No fabric is guessed from the probe command.
Each family uses the corresponding catalog name from Network Test Discussion Notes:

| Probe | Suite name (append `-ethernet` or `-infiniband`) |
| --- | --- |
| ICMP | `K8sEastWestNetworkICMPPing` |
| RDMA ping | `K8sEastWestNetworkRDMAPing` |
| Host-memory bandwidth | `K8sEastWestNetworkIBWriteBandwidth` |
| GPUDirect DMA-BUF bandwidth | `K8sEastWestNetworkDMABufBandwidth` |

The root is `<testsuites name="l8k validation tests">`. Each family is a direct
`<testsuite>` child with `tests`, `failures`, `errors`, `skipped` and `time`
attributes. Each directional pod/rail probe is a direct `<testcase>` child,
named `FamilyName::source→destination`; endpoint names include node, rail, pod
and IP to distinguish probes. There is no extra aggregate testcase. Counters
count the child cases once. This structure is readable by ai-cloud-validation's
JUnit parser; nested `<testcase>` elements would hide the individual results.

Suite time is measured wall-clock seconds for the family stage, including route
checks. Individual probe times are omitted because batched RDMA execution does
not measure each probe separately. Unexecuted suites have time `0.000`.
`system-out` carries structured probe evidence (including expectations,
observations, endpoints, bandwidth and command output); `system-err` carries
stderr. Failures use the existing expectation-aware verdict: expected isolation
can pass, and observe-only disconnections do not independently fail validation.

Disabled families and families without executed probes have an explicit skipped
case with a reason. A missing selected-family coverage gate is recorded as a
separate `ConnectivityCoverage` failure when the existing validation policy
requires coverage, even if another check also failed. Completed probes
survive execution errors. Other checks and execution errors are recorded in
`network/validation`; in-progress manifests are skipped, not passed. Each evaluated preset group has
a `TopologyPresets::group` case: matches pass, deviations fail, and missing or
skipped presets are skipped with their reason. The report
preserves the existing mode/flavor acceptance rules described below.

## Check Stages

| Stage | What it verifies |
| --- | --- |
| Helm release | Network Operator chart appVersion and rendered user values. |
| Component versions | Version-bearing sections of `NicClusterPolicy` and `NicNodePolicy` match the selected release catalog. |
| Manifest state | Each generated CR is classified as `READY`, `IN-PROGRESS`, `ERROR`, or `MISSING`. |
| Preflight | Stray CRs and Helm value drift that can make the apply path ambiguous. |
| Connectivity | ICMP, `rping`, host-memory `ib_write_bw`, and optional GPUDirect DMA-BUF bandwidth checks between generated test DaemonSet pods. |

When another system owns the Network Operator Helm release, set
`networkOperator.skipHelmChart: true` or pass
`--skip-network-operator-helm`. The Helm release/version and values checks are
then reported as skipped. Component versions, manifest state, stray-resource
preflight, connectivity, and the HTML report still run.

In full validation, connectivity is skipped when manifests are missing,
errored, or still in progress.

Each generated example DaemonSet declares two validation containers: the DOCA
container runs `rping`, `ib_write_bw`, and DMA-BUF bandwidth, while the `netshoot` container runs
ICMP and route checks from the same pod network namespace. Validation applies
the generated DaemonSet as written; it does not inject a helper container at
runtime. Before every same-rail and cross-rail ICMP probe in `quick`, `full`,
and `strict` modes, validation runs `ip route get <dst> from <src>`. A route
that does not select the requested source interface means that rail pair is not
connected; validation does not force the packet onto that interface. When the
source rail is selected, ICMP uses `ping -I <src-ip>` so source-based policy
rules participate naturally. Routing diagnostics compare the selected device
with the profile expectation: the source interface for source-based routing,
or the destination interface for destination-based routing.

NIC configuration success requires `ConfigUpdateInProgress` with `reason: UpdateSuccessful` and `status: "False"` on every matched device; a contradictory True/Unknown status remains `IN-PROGRESS`. This applies to both deployment waiting and standalone validation, including opt-in standard-profile templates.

Manifest-state checks for `NicConfigurationTemplate` and `NicFirmwareTemplate` use the operator-populated `status.nicDevices` list as the matched device set. An empty list, a list that does not yet reflect the current node, NIC type, PCI-address, serial-number, and part-number selectors, a missing named `NicDevice`, a device spec that does not yet reflect the current template payload, or a relevant device condition with a stale `observedGeneration` remains `IN-PROGRESS`. `NicConfigurationTemplate` considers `FirmwareUpdateInProgress` relevant only when the matched device has `spec.firmware`; a stale firmware condition cannot block a configuration-only deployment. Unrelated discovered devices are used only to verify selector freshness; their configuration and firmware state is ignored.

Preflight uses the same checks as deployment: Helm chart version, generated Helm values, component versions, and stray CRs from the [enumerated networking kinds and scopes](../advanced/deployment.md#stray-resource-deletion-boundary), without an l8k ownership requirement. SR-IOV pool configs, node policies, and OVS networks labeled with `spectrumx.nvidia.com/owner-name` are controller-owned outputs of `SpectrumXRailPoolConfig`, so they are not reported as strays. Validation never remediates drift.

The Helm chart/version checks are omitted when Helm management is disabled;
component and stray-resource checks remain active.

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
the configured GPU resource. When GPUDirect is enabled, that container also
sets `LD_LIBRARY_PATH` to prefer the host-injected NVIDIA driver libraries and
exclude the CUDA compatibility-library directory. The override prevents a
bundled `libcuda` from taking precedence over the host driver and is omitted
when GPUDirect is disabled.

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

## Debug and Trace Logs

Use debug logging for structured progress without command output:

```bash
l8k validate --log-level debug
```

Debug includes static-check durations, the complete endpoint inventory,
matrix and timeout planning, source-route executions and cache hits, stage and
RDMA-batch progress, pass/fail counts, cleanup, report writes, elapsed time,
and remaining time.

Use trace when a connectivity failure needs command evidence:

```bash
l8k validate --log-level trace --keep
```

Trace adds bounded route and ICMP commands, RDMA batch commands, per-test
client stdout/stderr, and RDMA server logs. Failed RDMA server logs are
collected before the temporary files and test workload are removed. Each log
field is capped to keep command output bounded. Add `--keep` only when the
validation pods must remain available for follow-up inspection.

Disable only the connectivity stage:

```bash
l8k validate --connectivity=false
```

## Connectivity-Only Validation

When a custom operational workload replaces the generated example, follow
the [application fixture recipe](workloads.md#preserve-validation-fixtures-before-replacing-them)
to retain matching test DaemonSets for a later full validation run.

The test DaemonSet does not need to be generated by Launch Kit. Put one or
more user-provided DaemonSets in `*example*.yaml` files under a directory and
pass that directory through the existing `--deployment-files` option:

```bash
l8k validate \
  --deployment-files ./connectivity-test \
  --user-config ./cluster-config.yaml \
  --kubeconfig "$KUBECONFIG"
```

When that directory contains only example workloads, Launch Kit runs only the
connectivity stage. If it also contains `values.yaml` or any non-example YAML
manifest, Launch Kit runs the complete deployment-validation pipeline and then
connectivity. A `cluster-config.yaml` stored in the same directory is treated
as configuration, not as a deployment manifest.

Validation checks every artifact file before connecting to the cluster.
Malformed or nameless resources fail with a file and document number, even
when they are example support resources. `values.yaml` is the only Helm values
filename; `values.yml` and case variants need renaming. Desired manifests
stay fixed during `--wait` and connectivity checks, while live cluster state
continues to refresh. An explicit HTML report path still receives a partial
report when artifact loading fails.

Connectivity requires these explicit decisions even though the remaining
validation settings have defaults:

```yaml
profile:
  routing: source-based # or destination-based

validation:
  gpuDirect:
    enabled: false
```

The test workload must be an `apps/v1` DaemonSet with `metadata.name` and
`metadata.namespace`. Selected checks require the `netshoot` route/ICMP helper;
`rping`, `ib_write_bw`, and GPUDirect also require the RDMA tools container.
Referenced namespaces, network attachments, credentials, and network resources
must already exist. Launch Kit applies the DaemonSet as provided and does not
inject or generate containers.

When `validation.gpuDirect.enabled` is `true` and `ib_write_bw` is selected,
`clusterConfig` must also identify every worker group and its east-west PFs.
Each group requires `workerNodes`; each east-west PF requires a non-negative
`rail` and `connectedGPU: GPU<N>`. This topology lets Launch Kit select the
source and destination GPU independently for every tested rail.

A connectivity-only invocation succeeds only when every selected check family
produces at least one gating test and all gating tests pass. Non-gating
cross-rail observations do not satisfy this requirement. A skipped,
empty, or incomplete matrix exits with status `4` because there is no other
validation stage providing acceptance evidence.

## When Validation Does Not Pass

Keep the test DaemonSet:

```bash
l8k validate --keep
```

Wait for in-progress manifests:

```bash
l8k validate --wait 10m
```

Use the [automation guide](../integrator/automation.md#capture-results-without-losing-the-exit-status) to capture JSON output and the CLI exit status without discarding diagnostics.

Continue with [Troubleshooting](troubleshooting.md) to investigate the failed stage while preserving the report and test resources as evidence.
