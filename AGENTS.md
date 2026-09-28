<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Launch Kit development guide

Kubernetes Launch Kit (`l8k`) discovers hardware, resolves configuration, renders networking
artifacts, deploys them, and validates the result. This file is the repository entrypoint for
development agents. Read [CONTRIBUTING.md](CONTRIBUTING.md) and any instructions scoped to the files
you change.

## Working approach

1. Confirm the requested outcome, current branch/base, and worktree status. For new work, refresh
   the intended base unless the user specifies a revision. Preserve unrelated changes; use an
   isolated `.sbx/<task>/` worktree when needed.
2. Read the affected implementation, callers, nearby tests and documentation before proposing a
   change. Use `rg`, Git history and available code-navigation tools; optional MCP servers are not
   prerequisites.
3. Trace the whole behavior across affected commands and external consumers. For a bug, establish
   expected versus actual behavior and a concrete reproducer before changing the implementation.
4. For nontrivial work, record affected files, compatibility, tests and documentation updates.
   Continue within the user's existing authorization; clarify only unresolved scope or consequential
   choices.
5. Keep changes focused. For interface changes, update the interface, implementations, consumers,
   mocks, tests and affected documentation together.
6. Put rendered smoke outputs and diagnostic evidence in the workspace's designated scratch
   directory outside the checkout. Keep credentials and customer-specific data out of tracked files.

A request only to plan or review does not authorize implementation. Follow the full user request
when it combines planning, implementation or delivery. A skill does not expand that authority. New
PR work uses a new branch from the requested base; authorized review fixes stay on the existing PR
branch.

## Where to start

Read the row for the task, then follow its relevant references. Do not load every operational skill
for every source change.

| Task | Implementation | Documentation and repository skill |
| --- | --- | --- |
| CLI, target routing, command compatibility | `pkg/cmd`, `pkg/target`, `pkg/target/host`, `pkg/options`, `pkg/app` | [Targets](docs/advanced/targets.md), [CLI](docs/reference/cli.md), [pipeline](skills/k8s-launch-kit-pipeline/SKILL.md) |
| Configuration, defaults, CLI overrides | `pkg/config`, `pkg/configinput`, `pkg/configflags`, `pkg/resolve` | [Configuration](docs/reference/configuration.md), [config](skills/k8s-launch-kit-config/SKILL.md) |
| Discovery and topology presets | `pkg/nicconfigdaemon`, `pkg/networkoperatorplugin/discovery*`, `pkg/presets` | [Discovery](docs/user/discovery.md), [presets](docs/user/presets.md), [discover](skills/k8s-launch-kit-discover/SKILL.md) |
| Profiles, grouping, templates, networking | `pkg/profiles`, `profiles`, `pkg/networkoperatorplugin/templates.go`, `pkg/networkoperatorplugin/scopes.go`, `pkg/networkoperatorplugin/spectrumx` | [Generation](docs/advanced/generation.md), [heterogeneous clusters](docs/user/heterogeneous-clusters.md), [Spectrum-X](docs/user/spectrum-x.md), [generate](skills/k8s-launch-kit-generate/SKILL.md), [networking context](skills/k8s-network-engineer/SKILL.md) |
| Artifact parsing and lifecycle handoff | `pkg/bundle`, `pkg/app`, `pkg/target/host`, `pkg/networkoperatorplugin` | [Artifact bundle](docs/architecture/artifact-bundle.md), [architecture](docs/architecture/overview.md) |
| Deployment, preflight, readiness | `pkg/networkoperatorplugin/deploy*`, `pkg/networkoperatorplugin/preflight`, `pkg/networkoperatorplugin/crstate`, `pkg/networkoperatorplugin/helmclient`, `pkg/target/host/deploy.go` | [Deployment](docs/advanced/deployment.md), [deploy](skills/k8s-launch-kit-deploy/SKILL.md), [dry run](skills/k8s-launch-kit-dryrun/SKILL.md) |
| Validation, connectivity, reports | `pkg/target/host/validate*`, `pkg/networkoperatorplugin/connectivity`, `pkg/networkoperatorplugin/crstate` | [Validation](docs/user/validation.md), [validate](skills/k8s-launch-kit-validate/SKILL.md) |
| OpenShift flavor | `pkg/config/flavor.go`, `pkg/networkoperatorplugin/ocp_config.go`, flavor-specific profiles and lifecycle code | [OpenShift](docs/user/openshift.md); relevant lifecycle skill's OpenShift section |
| Cleanup | `pkg/cmd/clean.go`, `pkg/networkoperatorplugin/clean.go` | [Cleanup](docs/user/cleanup.md), [clean](skills/k8s-launch-kit-clean/SKILL.md) |
| Diagnostics and automation | `pkg/cmd/sosreport.go`, `pkg/kubeclient`, `pkg/log`, `pkg/ui`, `pkg/errors` | [Troubleshooting](docs/user/troubleshooting.md), [automation](docs/integrator/automation.md), [troubleshoot](skills/k8s-launch-kit-troubleshoot/SKILL.md) |
| Dependencies, release catalogs, packaging | `go.mod`, `pkg/networkoperatorplugin/releases`, `hack`, `Makefile`, `.github/workflows` | [Installation](docs/user/installation.md), [configuration](docs/reference/configuration.md), [maintenance](docs/user/maintenance.md) |

For actual CLI operation, read [shared guidance](skills/k8s-launch-kit-shared/SKILL.md) and the
matching skill in full. Resolve its references relative to that skill. These skills describe
operating `l8k`; they do not require a deployed cluster or installed binary for source development.
Prefer the skill source in the checkout under development over a machine-local adapter pointing at
another checkout.

## Sources and behavior to preserve

Code and tests show current implementation; reviewed requirements describe intended behavior. If
they disagree with a guide or skill, identify the discrepancy and reconcile it in the change. Do not
silently reinterpret a documented guarantee. Historical plans and prior-session memory are leads to
verify, not proof of current support.

Use Go types/tags, canonical config, profile metadata and release catalogs for mechanical facts.
Query the built binary's `schema`/command help for supported flags. Avoid adding another
hand-maintained flag, default, release or CRD-version table here.

### Configuration and command boundaries

- Preserve precedence: canonical defaults < hardware defaults < user YAML < explicit CLI. Presence
  matters: omitted/null differs from explicit false, zero, empty string and empty collections.
  Supplied values still undergo validation.
- Add config-backed flags through `options.Options` tags and the shared binding/resolver path.
  Complex coordinated inputs use typed resolver requests. Keep command scope and schema metadata
  aligned; do not add a parallel manual flag-to-YAML registry.
- The selected release remains authoritative for catalog-managed coordinates after merging. Preserve
  flavor-specific defaulting and explicit user overrides as implemented in the resolver.
- Resolution must not mutate inputs or shared defaults. Generation preserves source YAML and writes
  `.l8k/resolved-config.yaml`. Deploy/validate prefer an explicit user config, then resolved bundle
  metadata when present, before legacy fallback.
- Discovery refresh with `--user-config` replaces inventory and explicit CLI-targeted fields while
  preserving unrelated fields, comments, omissions and explicit values. Fresh discovery has a
  different defaulting path; test both when changing resolution.
- Bind typed lifecycle requests through the target registry. Keep Cobra/process termination in
  `pkg/cmd`; services return errors and honor context. Unsupported target phases must reject rather
  than fall through into Host execution.

### Artifacts, rendering and topology

- Keep one validated desired-artifact snapshot per operation. Validate rendered artifacts before
  replacing output and validate deploy inputs before Helm/remediation/apply. Preserve source
  file/document diagnostics and deterministic ordering.
- Consumers must not mutate the retained bundle. Reuse desired state while refreshing live
  Kubernetes state during polling. Structural bundle validation does not prove admission, schema
  validity or runtime readiness.
- A new rendered Kind needs an explicit decision in the scope registry. Check singleton, bucket,
  per-source and namespace behavior, unique identities, references and selectors under full and
  subset group selection.
- Keep machine-specific PCI selectors tied to their source group. Do not broaden a selected group
  set, mix north-south NICs into east-west configuration, or derive persisted identifiers
  independently of the shared identity code.
- Preserve preset matching/deviation behavior and topology lifetimes. Keep worker-specific probe
  data transient when it cannot represent the whole group. Check naming/netplan conflicts and
  relevant profile/RA/mode combinations.

### Ownership, readiness and platform support

- Preserve dependency ordering, observation freshness and error classification in deploy/validate.
  Missing permissions, malformed resources and incomplete checks must not become successful
  evidence.
- External Helm ownership disables the Helm boundary; it does not disable CR handling. Preflight
  remediation and explicit `clean` have different deletion scopes. Read the cleanup contract before
  changing either; do not assume cleanup is limited to this run's resources.
- OpenShift is a Host flavor with external operator ownership and different namespaces, selectors
  and permissions. Use its explicit flavor paths and qualification matrix; do not infer support from
  Kubernetes templates or a successful render.
- Connectivity checks must use the intended source interface and topology. Do not substitute guessed
  GPU/rail identities. Preserve per-check gating, failure evidence, report writing and cleanup on
  partial/error paths.
- Render, server dry-run, simulated tests and live validation establish different things. A
  skipped/deferred check is not a pass; qualification applies only to the tested profile, platform
  and hardware.

## Implementation and integration

- Follow nearby Go style, nil-guard public pointer inputs, check collection bounds and return
  descriptive wrapped errors. Add meaningful regression cases for new behavior and failure paths.
- Preserve stderr when executing commands. Use `CombinedOutput()` for ordinary subprocess capture
  and the existing pod-exec helper for separately captured streams. Route diagnostics through
  existing logging, with bounded/redacted output where needed; never print credentials or kubeconfig
  contents.
- Check the pinned operator APIs, chart contracts and generated manifests when changing integration
  behavior. A dependency bump can affect templates, readiness validators, discovery assets, release
  cohorts and consumers outside this repository.
- When bumping NIC Configuration Operator, inspect whether `make sync-nic-config-crds` must refresh
  the embedded discovery CRDs. Regenerate changed canonical config with `go generate ./pkg/config`
  and review generated diffs.
- Use released versions or real pseudo-versions for deliverable cross-repo dependencies. Local
  sibling `replace` directives are temporary development aids and cannot be assumed to work in
  single-repo Docker build contexts.
- Preserve release-cohort consistency and existing GA/prerelease transition rules. Verify current
  catalog/profile gates rather than copying a release snapshot from a skill.
- Match surrounding license headers; new files use the current-year NVIDIA copyright and the
  repository's SPDX convention.

## Verification

Read the checked-out `Makefile`, `go.mod`, lint config and CI workflows for current tool versions
and required gates.

```sh
make build                            # produces build/l8k with version metadata
./build/l8k schema                    # inspect this checkout's capabilities
# Replace the placeholder with each affected package:
go test ./pkg/<affected-package>/... -count=1
make test                             # repository-wide tests
make lint                             # use the golangci-lint version pinned by CI
# Match CI's race gate on a supported platform with CGO enabled:
go test -race -count=1 ./...
# After installing requirements-docs.txt into an isolated Python environment:
mkdocs build --strict
```

- Use Ginkgo/Gomega for new behavioral suites; current code also has Go/testing and testify tests.
  Extend an existing regression suite in its established style and avoid unrelated framework
  migrations. Controller integration tests in sibling operators follow their own envtest guidance.
- Use existing mocks/fakes; explicitly initialize fields relevant to the scenario. For asynchronous
  behavior, model live-state transitions rather than only invocation counts. Assert both expected
  effects and prohibited side effects where the contract needs them.
- For renderer/bundle changes, use valid resolved fixtures, render relevant profiles/modes and group
  subsets, parse output through `bundle.FromFiles`, and inspect identities/selectors/references.
  File counts alone do not verify manifest correctness.
- Keep fixtures anonymized. Use generic host/site/model identifiers unless a public hardware
  identifier is essential to the behavior under test; preserve necessary PCI topology. Do not
  silently change shared fixture assumptions to make a test pass.
- Choose checks by impact: targeted regression tests during iteration; required build/test/lint
  gates for code; strict docs and example checks for documentation. Report any gate not run and why.
  `make build-all` is relevant to platform/build changes and remains a hosted CI gate.
- Diagnose environment failures against a clean base or isolated inputs. Do not assume a
  historically failing preset test is still an expected failure, silently skip it, or stash/reset
  somebody else's checkout to compare.
- Develop with `make build` and `./build/l8k` from the repository root. Global installation is
  optional; source installation uses `scripts/install-local.sh`. The Makefile's install targets can
  download supporting assets and write system paths.
- For cluster images, select the intended target explicitly; this workspace defaults to
  `linux/amd64`. Use `docker buildx build --platform=linux/amd64` with an appropriate load/push
  choice and verify image architecture. Native CLI builds remain supported on their documented
  platforms.

## Running CLI smoke checks and live workflows

Use `--output json` for scripted assertions, retain stderr/logs separately, and check the process
exit code and the command's actual result shape. Do not discard diagnostic evidence by default or
add root-only `--yes` to subcommands. Text/help/schema inspection is appropriate when testing those
interfaces.

Generation without deploy can be tested offline. Discovery creates temporary cluster resources;
connectivity validation also creates test resources. A pipeline's `--dry-run` does not establish
that every phase is read-only. Resolve the target/context and stay within the user's existing
live-operation authority. `clean` has no dry run; inspect its precise scope and retention choice
before invoking it.

For diagnosis, start with the supplied reproducer/logs or sosreport, trace the failing layer, and
collect additional evidence proportionally. Use `debug` then bounded `trace` logging when needed. Do
not run discovery, deployment or a full connectivity matrix merely to inspect source code.

## Required documentation updates

Documentation is part of completing a change. Agents must update the relevant existing sections in
the same PR whenever behavior, usage, interfaces or development procedures change.

1. **Before editing:** search for descriptions of the affected behavior in `README.md`, `docs/`,
   `skills/` and their bundled references. Include the relevant sections in the change plan; follow
   links to find duplicated examples or guarantees.
2. **During implementation:** update those sections alongside the code. Explain the resulting
   behavior, prerequisites, defaults, compatibility and limitations where readers need them. Update
   existing guidance instead of adding a parallel manual or a change log to this file.
3. **Before finishing:** check examples against the current types, schema and command help;
   regenerate affected generated documentation using its documented generator; verify links and run
   `mkdocs build --strict` for site changes. Inspect the final diff for contradictory or stale
   descriptions.
4. **In the PR and completion report:** name the documentation sections updated and the checks
   performed. If no documentation update is needed, explain why the existing documentation remains
   accurate. Do not make unrelated documentation edits just to satisfy this check.

Use this impact map to find the relevant sections; a change does not require editing every row.

| Changed area | Documentation to review and update when affected |
| --- | --- |
| Commands, flags, config fields, defaults or precedence | CLI/configuration reference, relevant user or advanced guide, README examples, phase skill and its references |
| Profiles, templates, selectors or emitted resources | Profile and generation guides, relevant networking guide, render examples and generate/config skill references |
| Package ownership, interfaces, lifecycle or artifact handoff | Architecture overview and diagrams, specific architecture document, exported API comments and library examples |
| Deployment, cleanup, validation or platform support | Operational guides, prerequisites and qualification matrices, troubleshooting guidance, corresponding skills |
| Build, tests, installation, dependencies or release workflow | CONTRIBUTING, README development section, installation/maintenance guides, shared skill and affected workflow documentation |

## Guidance maintenance and delivery

- Keep this file about development rules and navigation. Detailed procedures stay in docs/skills;
  version/default facts stay with their existing source. `CLAUDE.md` points here instead of
  maintaining a second manual.
- Preserve local-only guidance before replacing it. Keep personal paths, credentials, lab state,
  active PR status and historical incident logs outside the shared guide. Update persistent personal
  memory only when explicitly requested.
- For an authorized PR, follow [CONTRIBUTING.md](CONTRIBUTING.md), use DCO sign-off and inspect the
  final diff. When authorized to address review, verify findings against the current head, amend the
  relevant commit where appropriate, and use lease-protected pushes for rewrites.
- Report what changed, why, exact checks/results and remaining limitations. Keep local validation,
  hosted checks, review approval, merge, publication and live qualification distinct. Read-only
  review findings need concrete code paths and evidence.
