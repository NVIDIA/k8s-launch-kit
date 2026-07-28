<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Automation

`l8k` is designed for both interactive operators and automated systems.

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

Do not combine `--yes` with subcommands unless the specific command accepts it. `--output json` is the portable automation path.

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
