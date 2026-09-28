<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Automation

`l8k` is designed for both interactive operators and automated systems.

For repository-provided AI-agent playbooks, see [AI Skills](ai-skills.md).

## JSON Mode

Use JSON mode when a pipeline or agent needs structured output:

```bash
l8k discover --output json 2>/dev/null | jq .
l8k generate --output json 2>/dev/null | jq .
l8k validate --output json 2>/dev/null | jq .
```

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
jq -s . validation.jsonl >validation-results.json
# Read reportPath from whichever object carries it.
jq -r 'select(has("reportPath")) | .reportPath' validation.jsonl
exit "$status"
```

An empty stream or absence of a `reportPath` is possible on an early failure.
The manifest object's `summary.success` covers that summary, not later
connectivity or every final acceptance gate. Inspect the HTML report and
[coverage outcomes](../user/validation.md#acceptance-outcomes) as well as the
exit status. For display-only `l8k … | jq …` examples, Bash automation must
enable `set -o pipefail` to propagate a failing `l8k` status.

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

## GitOps Pattern

1. Run discovery on a representative cluster and commit the reviewed `cluster-config.yaml`.
2. Generate manifests in CI:

   ```bash
   l8k generate \
     --user-config cluster-config.yaml \
     --save-deployment-files ./deployment \
     --output json 2>/dev/null
   ```

3. Retain the complete `deployment/` directory as one versioned CI artifact,
   including `.l8k/resolved-config.yaml`. Publish only the intended Kubernetes
   resources to the GitOps controller; exclude `*example*.yaml`, treat
   `values.yaml` as Helm input, and never apply the `.l8k` metadata envelope.
4. After the controller applies the resources, retrieve that same complete
   artifact and run `l8k validate --deployment-files ./deployment`. Omit
   `--user-config` to use the effective configuration recorded during rendering.
   Confirm completed acceptance coverage, not only an exit code.

Copying only `network-operator/` loses generation-time CLI overrides and exact
resolved configuration. If another system owns Helm, set
`networkOperator.skipHelmChart: true` before generation so l8k skips its
Helm-specific acceptance checks.

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
  --output json 2>/dev/null
```

Use `--config-dir` to test a custom preset catalog in CI.

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
