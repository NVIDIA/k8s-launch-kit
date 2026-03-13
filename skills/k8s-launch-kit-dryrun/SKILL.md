---
name: k8s-launch-kit-dryrun
version: 1.1.0
description: "Use this skill when the user wants to preview what k8s-launch-kit (l8k) would deploy without making changes, or wants to safely validate their configuration before applying. Activate for: dry-run, preview, validation, 'what would happen if', testing configurations, schema discovery, checking generated manifests, or any cautious pre-deployment step. Also use when the user asks 'is my config valid' or 'show me what would be created' -- even without mentioning dry-run explicitly."
metadata:
  requires:
    skills: ["k8s-launch-kit-shared"]
---

# l8k: Dry Run & Validation

> **PREREQUISITE:** Read `../k8s-launch-kit-shared/SKILL.md` for install paths, global flags, and output modes.

Preview what l8k would deploy without making cluster changes, or validate a configuration.

## Usage

```bash
# Generate-only (no --deploy, no cluster changes)
l8k generate --user-config <CONFIG> --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <DIR>

# Dry-run with deploy flag (validates against live cluster but doesn't apply)
l8k generate --user-config <CONFIG> --fabric <FABRIC> --deployment-type <TYPE> \
  --save-deployment-files <DIR> --deploy --dry-run [--kubeconfig <PATH>]
```

## Modes

| Mode | Flags | Cluster Access | What It Does |
|------|-------|----------------|--------------|
| Generate-only | No `--deploy` | Not needed | Renders templates, writes YAMLs to disk |
| Dry-run | `--deploy --dry-run` | Needed | Generates + validates against cluster, doesn't apply |

## Examples

```bash
# Preview generated files (no cluster needed)
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output

# Dry-run against live cluster
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --deploy --kubeconfig ~/.kube/config --dry-run

# Agent mode: get manifest list as JSON
l8k generate --user-config cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./output \
  --deploy --dry-run --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null

# Schema discovery (list capabilities)
l8k schema
```

## JSON Output (Dry-Run)

```json
{
  "success": true,
  "phase": "generate",
  "profile": {"fabric": "ethernet", "deploymentType": "sriov"},
  "generatedFiles": ["output/group-0/nicclusterpolicy.yaml", "..."],
  "deployed": false,
  "dryRun": true
}
```

## Tips

- Generate-only is the safest starting point — inspect the YAMLs before applying.
- Use `l8k schema` to programmatically discover available profiles and flags.
- Combine `--dry-run --output json` for CI validation gates.

## See Also

- [k8s-launch-kit-shared](../k8s-launch-kit-shared/SKILL.md) — Global flags and exit codes
- [k8s-launch-kit-deploy](../k8s-launch-kit-deploy/SKILL.md) — Apply after previewing
