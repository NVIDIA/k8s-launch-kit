<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Automation

`l8k` is designed for both interactive operators and automated systems.

For repository-provided AI-agent playbooks, see [AI Skills](ai-skills.md).

## GitOps Pattern

Choose the owners before building a pipeline: who renders, who manages the Network Operator Helm release, who applies custom resources, and who signs off the validation report. A GitOps controller is an external apply owner; its ordering, readiness, retry, and pruning policy must satisfy the generated resources' dependencies. This repository does not claim an Argo CD or Flux integration.

1. Pin the installed `l8k` version and selected Network Operator release. Review discovery output and commit the intended `cluster-config.yaml`. Archive the exact referenced Spectrum-X profile, topology, preset/template override, and custom workload files or immutable revisions. Review network addresses, target cohort, credentials, and ownership before CI renders.
2. Render offline in CI where inputs allow it. Capture output and diagnostics separately and fail the job with the original CLI status:

   ```bash
   status=0
   l8k generate --user-config ./cluster-config.yaml \
     --save-deployment-files ./deployment --output json \
     >generation.json 2>generation.log || status=$?
   if [ "$status" -ne 0 ]; then
     cat generation.log >&2
     exit "$status"
   fi
   jq . generation.json
   ```

   Pass the same pinned profile, `--config-dir`, topology, and workload options used in the approved input record. Offline generation with a preset does not qualify live hardware; compare the preset against the target cluster before apply.
3. Review the complete `deployment/` output, especially `.l8k/resolved-config.yaml`, resource identities, Helm values, selectors, addresses, driver/maintenance settings, and resources that would disappear. Retain the **whole** directory as one immutable artifact. Publishing only `network-operator/` loses generation-time overrides and exact effective configuration.
4. Create a separate apply set for the external controller. `values.yaml` is Helm input for the Helm owner; `.l8k/` is metadata for Launch Kit; `*example*.yaml` files are validation fixtures. Apply only approved operational Kubernetes resources in dependency order. If Helm is externally owned, set `networkOperator.skipHelmChart: true` before generation. Avoid a controller prune policy that could delete other cohorts or manually owned resources; compare the complete desired/live inventory.
5. After the external controller reports reconciliation, retrieve the **same complete artifact** and run `l8k validate --deployment-files ./deployment --wait 10m`. Omit `--user-config` so validation uses the saved effective configuration. Keep the HTML report and apply the [acceptance outcomes](../user/validation.md#acceptance-outcomes); process success alone can leave incomplete connectivity coverage.

The reproduction record should contain the CLI version/build, selected operator release, source configuration, every referenced profile/topology/workload file, preset or template revision/digest, all render options, complete generated bundle and sidecar, apply owner/revision, and validation report. Mutable local paths alone cannot reproduce a render. The site follows development `main`; use the documentation at the installed release's tag or commit when operating an older binary.

## JSON Mode

Use JSON mode when a pipeline or agent needs structured output. Capture the command and check its status before parsing, as in the [GitOps recipe](#gitops-pattern) and [validation capture](#capture-results-without-losing-the-exit-status).

Output contracts differ by command:

| Command | Successful stdout with `--output json` |
| --- | --- |
| Root pipeline, `discover`, `generate` | One `JSONResult` envelope, including collected `messages`. |
| `clean` | One result with a `cleanup` summary. |
| Standalone `deploy` | No finalized success JSON envelope. Use the process exit status; progress is sent to stderr. |
| `validate` | A sequence of JSON objects: manifest/version summary when full validation runs, connectivity when run, and `reportPath` when the HTML report is written. Do not parse the stream as one document. |
| `schema` | One capability object; this command always emits JSON. |
| `version` | One version/build object. |
| `preset list`, `preset update`, `sosreport` | Text output; these commands do not provide a JSON success contract despite accepting the inherited flag. |

Lifecycle JSON mode sends human-readable progress to stderr and auto-confirms
prompts. Structured error objects may be emitted on failures; early Cobra flag
parse errors can instead write text to stderr. Validation can emit partial
results before a failure. Always check the process exit status.

`--yes` is root-only. Use `--output json` for non-interactive lifecycle
subcommands, with particular care for destructive cleanup and deployment.

### Capture Results Without Losing The Exit Status

Capture the command first, then parse its output. This works without relying
on shell pipeline options and preserves diagnostic stderr:

```bash
status=0
l8k validate --deployment-files ./deployment --output json \
  >validation.jsonl 2>validation.log || status=$?
if [ "$status" -ne 0 ]; then
  cat validation.log >&2
  exit "$status"
fi
jq -s . validation.jsonl >validation-results.json
# Read reportPath from whichever object carries it.
jq -r 'select(has("reportPath")) | .reportPath' validation.jsonl
```

An empty stream or absence of a `reportPath` is possible on an early failure.
The manifest object's `summary.success` covers that summary, not later
connectivity or every final acceptance gate. Inspect the HTML report and
[coverage outcomes](../user/validation.md#acceptance-outcomes) as well as the
exit status. Interactive display pipelines can use `set -o pipefail` in Bash. CI examples above preserve the command status explicitly and keep diagnostic stderr.

## Structured Results

A successful generation or root pipeline result can include its phase,
resolved profile, generated files, deploy/dry-run status, and messages:

```json
{
  "success": true,
  "phase": "generate",
  "profile": {
    "fabric": "ethernet",
    "deployment": "sriov"
  },
  "generatedFiles": [
    "deployment/network-operator/values.yaml"
  ],
  "deployed": false,
  "messages": []
}
```

A structured failure includes a stable category and retry guidance:

```json
{
  "success": false,
  "error": {
    "code": "CLUSTER_ERROR",
    "message": "failed to connect to cluster",
    "category": "cluster",
    "transient": true,
    "suggestion": "Check kubeconfig and API server connectivity"
  },
  "deployed": false,
  "messages": []
}
```

Use `error.transient` to decide whether retry is appropriate. Treat `suggestion` as operator guidance, not a command to execute without review. The process exit code remains authoritative even when a JSON object is emitted.

Cleanup returns a command-specific summary:

```json
{
  "success": true,
  "phase": "clean",
  "deployed": false,
  "cleanup": {
    "namespace": "nvidia-network-operator",
    "customResourcesDeleted": 12,
    "helmReleaseRemoved": true,
    "keepHelmChart": false
  },
  "messages": []
}
```

`cleanup.keepHelmChart` reports the effective retention policy. It is `true`
when either `networkOperator.skipHelmChart` in the resolved config or the
explicit `--keep-helm-chart` flag retains the release.

`l8k clean --output json` auto-confirms an irreversible cluster mutation and
has no dry-run mode. Automation must pin the intended kubeconfig and should
pass `--network-operator-namespace` explicitly after independently checking
the target.

## Capability Discovery

```bash
l8k schema | jq .
```

The schema includes command descriptions, supported fabrics and deployment types, supported Network Operator release lines, exit codes, and automation-relevant flags.

## Exit Codes

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | General error |
| `2` | Validation error, bad flags, or invalid config |
| `3` | Cluster error |
| `4` | Deployment or validation failure |
| `5` | Partial success |

Validation uses exit `4` when an acceptance gate fails, including release or manifest drift, preset topology deviation, and gating connectivity failures.

## Logging

Keep JSON on `stdout` and direct diagnostic logging separately:

```bash
l8k validate \
  --output json \
  --log-level debug \
  --log-file ./l8k-debug.log \
  >validation.json
```

Without `--log-file`, logs use `stderr`. Do not merge `stderr` into `stdout` in a parser-facing pipeline.

## Offline Preset Pattern

For known SKUs, avoid cluster access during render:

```bash
l8k generate \
  --for ThinkSystem-SR680a-V3-H200 \
  --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
  --fabric ethernet \
  --deployment-type sriov \
  --network-operator-release 26.4 \
  --save-deployment-files ./deployment \
  --output json >generation.json 2>generation.log &&
  jq . generation.json
```

The `&&` preserves a failed generation status and avoids parsing a partial
result. Retain `generation.log` on failure; use the fuller [capture pattern](#gitops-pattern)
in CI. Use `--config-dir` to test a custom preset catalog in CI.

## Release Selection

Pin the target Network Operator line in the config or CLI:

```yaml
networkOperator:
  selectedRelease: "26.4"
```

```bash
l8k generate --network-operator-release 26.4
```

The selected release fills image tags, component versions, the DOCA driver version, Helm repository URL, and profile gates.
