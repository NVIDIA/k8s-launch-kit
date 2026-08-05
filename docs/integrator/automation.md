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

In JSON mode:

- `stdout` contains one final JSON object.
- Human-readable logs go to `stderr`.
- Prompts are auto-confirmed.
- Timestamped progress messages are collected under `messages`.

Do not combine `--yes` with subcommands unless the specific command accepts it. `--output json` is the portable automation path.

## Structured Results

A successful command can include its phase, resolved profile, generated file list, deploy status, dry-run status, and collected messages:

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

3. Diff or publish `deployment/network-operator/` to the GitOps repository.
4. Run `l8k validate` after the GitOps controller applies the bundle.

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
