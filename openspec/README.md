<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Launch Kit extension contracts

These specs define the integration decisions and observable behavior to preserve
when extending Launch Kit. They are organized by extension boundary, so a new
flag or Kind fits an existing contract instead of needing its own permanent spec.

## Status and scope

The requirements govern new or changed integrations. **Baseline and limits** in
each spec records existing behavior and known exceptions; adopting these documents
does not fix those exceptions. When changing a path with a listed gap, explicitly
resolve the relevant requirement or document the retained exception and its impact
in the PR. Do not silently claim conformance or expand a scoped change into an
unrequested migration.

The initial baseline was reviewed against main `2fda1c951db00af3382b9562a6cfc76e744c1541`.
Implementation/test links are navigation and existing evidence starting points,
not a claim that every scenario already has a passing test. New extensions must
supply evidence for their affected requirements. OpenSpec validation checks document
structure; it does not execute scenarios, prove code conformance or qualify a cluster.

## Contract index

| Contract | Read when adding or changing | Owns |
| --- | --- | --- |
| [Configuration parameters](specs/configuration-parameters/spec.md) | Field, flag, default, normalization | Value meaning, presence, precedence and consumer wiring |
| [Discovery inventory](specs/discovery-inventory/spec.md) | Probe, capability, observation source | Eligibility, provenance, aggregation and lifetime |
| [Workflow execution](specs/workflow-execution/spec.md) | Phase, command, adapter or public entry point | Dispatch, dependencies, context and partial failure |
| [Platform support](specs/platform-support/spec.md) | Target, flavor or supported combination | Capabilities, prerequisites, adaptations and qualification |
| [Profile planning](specs/profile-planning/spec.md) | Profile, template, grouping or identity | Eligibility, exact selection, multiplicity and references |
| [Artifact handoff](specs/artifact-handoff/spec.md) | Format, renderer, bundle consumer or metadata | Desired snapshots, structural validation and config handoff |
| [Resource kinds](specs/resource-kinds/spec.md) | GVK, version or readiness strategy | API integration, evidence freshness and state classification |
| [Managed effects](specs/managed-effects/spec.md) | Apply, adoption, temporary resource or deletion | Ownership, mutation scope, preservation and teardown |
| [Connectivity checks](specs/connectivity-checks/spec.md) | Protocol, endpoint, metric or route check | Measurement claims, planning, execution and per-check evidence |
| [Outcomes and evidence](specs/outcomes-evidence/spec.md) | Acceptance gate, report or output | Completeness, aggregate verdicts, framing and retained artifacts |

## Using the contracts in a change

1. Select the relevant contracts from the index; follow their boundary links only
   where the change crosses another integration point.
2. Record the extension decisions in existing types, metadata, implementation,
   documentation and a short PR explanation. A new registry or manifest is not required.
3. Trace applicable scenarios to meaningful tests or other verification. Include
   failure paths and prohibited effects where relevant. Identify unverified behavior.
4. Update a spec when a shared promise, integration decision or baseline exception
   changes. Routine additions that satisfy an unchanged contract need only their
   implementation, tests and affected user documentation/skills updated.
5. Follow [AGENTS.md](../AGENTS.md) and [CONTRIBUTING.md](../CONTRIBUTING.md) for
   development and delivery. These contracts do not grant authority for live operations.

For example, adding a configuration flag starts with **configuration parameters**:
define its YAML/type/presence, wire each advertised command, test precedence and
consumer propagation, then update configuration/CLI docs and the relevant skill.
Consult **artifact handoff** if its serialization changes, and **platform support**
if it changes flavor policy. Do not edit all ten specs or create a permanent spec
for that individual flag.

Adding a Kind usually crosses **profile planning**, **resource kinds** and
**managed effects**: declare render/API scopes, dependencies, readiness evidence
and cleanup policy, then exercise generation through bundle parsing and relevant
lifecycle tests. A test proving rendering alone cannot prove readiness or teardown.

## OpenSpec tooling

The repository uses the `spec-driven` schema with durable specs under `specs/`.
No generated agent commands, global installation or Node dependency in the Go
build is required. From the repository root, optional structural validation with
the version used to introduce these specs is:

```sh
npx --yes --package @fission-ai/openspec@1.13.2 openspec validate --all --strict --no-interactive
```

This fetches the pinned CLI into npm's cache if necessary. Use a compatible Node
runtime as required by that package. The specs remain readable without the CLI.
Run strict MkDocs and relevant implementation checks separately as described in
the development guide. Structural validation is a local documentation check;
this PR does not add a hosted OpenSpec gate.

A change proposal under `openspec/changes/` is useful when modifying a shared
promise across several boundaries. It is optional for routine conforming changes;
these contracts introduce no mandatory proposal/approval stage or per-flag ceremony.
Keep exact flag/default/version inventories in their existing mechanical sources,
operating procedures in docs/skills, and development rules in AGENTS.md.
