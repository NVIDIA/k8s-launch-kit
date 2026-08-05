<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# AI Skills

The repository includes task-specific `SKILL.md` playbooks that help compatible AI agents operate `l8k` consistently. They describe supported commands, flags, safety boundaries, structured output, and troubleshooting workflows.

AI skills are documentation for an agent. They are not Launch Kit runtime plugins and do not change the `l8k` binary.

## Skill Catalog

| Skill | Agent task |
| --- | --- |
| `k8s-network-engineer` | Route broad NVIDIA Kubernetes networking requests to the appropriate workflow. |
| `k8s-launch-kit-shared` | Apply common install paths, output rules, error handling, and safety guidance. |
| `k8s-launch-kit-config` | Create, inspect, and edit Launch Kit configuration. |
| `k8s-launch-kit-discover` | Inventory cluster hardware and produce `cluster-config.yaml`. |
| `k8s-launch-kit-generate` | Select a profile and render manifests. |
| `k8s-launch-kit-dryrun` | Preview generated or server-side deployment changes. |
| `k8s-launch-kit-deploy` | Apply generated resources in dependency order. |
| `k8s-launch-kit-clean` | Remove Network Operator custom resources and its Helm release. |
| `k8s-launch-kit-validate` | Run deployment acceptance and interpret the validation report. |
| `k8s-launch-kit-pipeline` | Coordinate discovery, generation, and deployment as an end-to-end flow. |
| `k8s-launch-kit-troubleshoot` | Diagnose discovery, operator, SR-IOV, RDMA, and IPAM failures. |

The source playbooks are under [`skills/`](https://github.com/NVIDIA/k8s-launch-kit/tree/main/skills). Each phase skill depends on the shared skill for common behavior.

## Make Skills Available

Use the project or workspace skill mechanism provided by the AI agent. Point it at the required skill directories, preserving each directory and its bundled `references/` files. Installation and discovery locations are agent-specific.

For a complete operational assistant, expose `k8s-network-engineer` and all of its required `k8s-launch-kit-*` skills. For a narrower automation task, expose `k8s-launch-kit-shared` plus only the phase skills the agent needs.

Clone the repository before creating links:

```bash
git clone https://github.com/NVIDIA/k8s-launch-kit.git
cd k8s-launch-kit
```

Common agent layouts:

=== "Claude Code"

    ```bash
    mkdir -p ~/.claude/skills
    for skill in skills/k8s-launch-kit-* skills/k8s-network-engineer; do
      ln -sfn "$PWD/$skill" "$HOME/.claude/skills/$(basename "$skill")"
    done
    ```

    Use `<project>/.claude/skills/` instead for project-scoped skills.

=== "OpenAI Codex"

    ```bash
    mkdir -p .agents/skills
    for skill in skills/k8s-launch-kit-* skills/k8s-network-engineer; do
      ln -sfn "$PWD/$skill" ".agents/skills/$(basename "$skill")"
    done
    ```

    Use `~/.agents/skills/` for user-scoped skills. An `AGENTS.md` can still carry project-specific policy that applies alongside the skills.

=== "Cursor"

    ```bash
    mkdir -p .cursor/rules
    for skill in skills/k8s-launch-kit-* skills/k8s-network-engineer; do
      name=$(basename "$skill")
      cp "$skill/SKILL.md" ".cursor/rules/${name}.mdc"
    done
    ```

    Copy or expose any referenced files as project context when the selected rule points to `references/`.

For other agents, load the relevant `SKILL.md` files as persistent project context or expose the `skills/` tree through the agent's resource mechanism. YAML frontmatter is metadata; agents that do not parse it can still use the Markdown body.

Example request after the skills are available:

```text
Discover this cluster, review the generated profile, render the manifests,
perform a server-side dry run, and stop before deployment.
```

## Machine-Readable CLI Contract

Skills complement the CLI's structured interface. They instruct agents to inspect capabilities rather than infer flags:

```bash
l8k schema | jq .
```

For subcommands, JSON mode keeps the final result on stdout and sends human-readable logs to stderr:

```bash
l8k discover \
  --save-cluster-config ./cluster-config.yaml \
  --output json 2>/dev/null | jq .

l8k generate \
  --user-config ./cluster-config.yaml \
  --save-deployment-files ./deployment \
  --output json 2>/dev/null | jq .
```

Do not add `--yes` to subcommands. `--output json` is the portable non-interactive path for those commands.

## Deployment Boundaries

An AI agent should:

- Reuse the profile persisted by discovery unless the user requests an override.
- Generate and inspect manifests before applying them.
- Use `l8k deploy --dry-run` for a server-side preview.
- Require clear authority before changing a live cluster.
- Treat cleanup as destructive: verify the kubeconfig and resolved operator namespace before running `l8k clean`.
- Treat `--overwrite-existing` as an explicit decision after reviewing Helm value drift.
- Run `l8k validate` as the normal acceptance stage after deployment.
- Use `l8k sosreport` and focused Kubernetes inspection when acceptance fails.

The skill files provide procedure and guardrails; Kubernetes credentials and authorization remain external to the skill.

## Example Agent Tasks

Discovery and review:

```text
Discover the cluster, summarize every source group, explain any preset
deviations, and stop before generating manifests.
```

Render without applying:

```text
Generate an SR-IOV Ethernet deployment for all H200 groups, inspect the
Helm and Kubernetes diffs with a server-side dry run, and do not deploy.
```

Acceptance and triage:

```text
Run the normal validation workflow. If it does not pass, retain the test
DaemonSet, collect a sosreport, and identify the first failed stage.
```

## Maintaining Skills

Update the corresponding skill whenever a CLI workflow, flag, default, exit code, or safety requirement changes. Keep examples aligned with `l8k schema` and command help, and keep shared behavior in `k8s-launch-kit-shared` instead of duplicating it across every phase.
