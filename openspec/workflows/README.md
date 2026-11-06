<!--
Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Procedural documentation sources

`dpf-discovery.json` owns the user steps and flow transitions for initial DPF
inventory, address-candidate lookup, refresh, and Sentry BF4 generation,
deployment and resource/functional verification. Its generated page is
`docs/user/dpf-workflows.md`; edit the JSON, not the generated Markdown.

This is a **repository-owned documentation extension**, not a native executable
OpenSpec workflow format. It does not add a runtime state machine or a discovery
plugin. Python's standard library is used only during documentation generation
and tests; the Go CLI and remote collection do not depend on Python. Commands
inside step bodies are displayed as examples and are never executed by this tool.

## Regenerate and verify

From the repository root:

```sh
python3 scripts/generate-workflow-docs.py
python3 -m unittest discover -s scripts -p 'test_generate_workflow_docs.py' -v
python3 scripts/generate-workflow-docs.py --check
```

The Docs CI job runs the tests and `--check` before strict MkDocs. The existing
`check-docs.py` CLI example checks still run separately in code CI. The generator
is deterministic, omits timestamps, and rejects invalid metadata before writing.
`--check` reports missing/stale output with exit 1 and leaves output untouched.
`--source` and `--output` can select explicit paths for local testing.

## Format version 1

The source is one JSON object with `formatVersion: 1`, `title`, `introduction`,
and a nonempty `workflows` array. An optional `$comment` carries source licensing.
Each workflow has a unique `id`, `title`, `introduction`, and nonempty `steps`.
Each step has an `id` unique within that workflow, `title`, Markdown `body`, and
`transitions`. Each transition has a destination step `to` and a nonempty `label`.
An empty transition list ends the procedure. Retry transitions may point backward.
IDs use lowercase letters, digits and hyphens and start with a letter. The first
listed step is the starting step; list steps in the order users should read them.

The generator validates field shapes, duplicate workflow/step IDs, duplicate JSON
fields, and unknown or duplicate transition destinations. It renders the same
transitions as both Mermaid edges and next-step links. The source remains a human
procedure: no conditions are evaluated, no infrastructure is inspected, and graph
structure does not prove the commands implement the contract. Review the source,
rendered prose, examples and diagram together against the actual CLI behavior.

## Maintaining a workflow

1. Update applicable [OpenSpec contracts](../README.md) when the shared promise
   changes, and implement/test the CLI behavior.
2. Edit the procedure's inputs, actions, failure explanations and transitions in
   the JSON. Keep required site inputs separate from defaults and observations.
3. Regenerate and run generator tests, drift detection, CLI example checks and
   strict MkDocs. Include the source and generated page in the same change.
4. Review runtime/hardware evidence separately. Inventory success, deployment
   readiness and hardware qualification are distinct outcomes.

The generator currently targets this one DPF procedure document. Extending it to
other guides is a future scoped documentation change, not a workflow execution API.
