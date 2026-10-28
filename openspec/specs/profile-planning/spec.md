<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Profile selection and resource planning

## Purpose

Integrate a profile, template, grouping rule, identifier or topology allocation. Inputs are effective configuration and normalized inventory; outputs are a resource plan and rendered artifacts for exactly the selected groups.

Read the [contract status and usage rules](../../README.md) first. Requirements govern
new or changed integrations; the baseline section identifies existing exceptions.

## Extension decisions

- Declare fabric/flavor/hardware/release eligibility, feature combinations and template set.
- Define render multiplicity independently of Kubernetes API scope: singleton, bucket, source, worker or network namespace.
- Define stable names, selectors, references, topology allocation, native Helm/API types and validation workload capabilities.

## Requirements

### Requirement: Eligibility and exact selection

A profile extension MUST define eligibility and preserve exact selected-group scope through rendering. Shared-network merging SHALL NOT broaden machine-specific PCI/device selection or introduce excluded groups.

#### Scenario: Mixed group subset

- **WHEN** only a subset of heterogeneous source groups is selected
- **THEN** rendered selectors, hardware settings and references address that subset without a cross-product of machine-specific devices.

#### Scenario: Unsupported feature combination

- **WHEN** the requested profile combination is outside declared eligibility
- **THEN** selection rejects it or follows an explicitly documented selection policy.

### Requirement: Multiplicity and unique identities

A template extension MUST declare render multiplicity, API scope and reference namespaces separately. New or changed identity rules SHALL detect collisions before output-map assignment can overwrite an artifact. Stable inputs SHALL produce stable identities.

#### Scenario: Two sources collide

- **WHEN** two selected sources would produce the same output path or resource identity
- **THEN** the changed planning path reports the collision before discarding either artifact.

#### Scenario: Namespace fan-out

- **WHEN** a template is rendered once per network namespace
- **THEN** its API scope and references are derived explicitly; a per-namespace render is not assumed to imply a namespaced Kind.

### Requirement: Consumer compatible payloads

Profiles SHALL emit native chart/API value shapes and declare the tools, images, devices and resources required by their advertised validation checks. Extensions MUST verify these prerequisites across applicable profile branches.

#### Scenario: Ambiguous scalar

- **WHEN** a chart expects a string whose text resembles a number or boolean
- **THEN** rendered YAML preserves the required string type.

#### Scenario: New check workload

- **WHEN** a profile advertises a check needing a tool or device
- **THEN** its workload provides the declared prerequisite or explicitly reports that check unsupported.

### Requirement: Standard NIC configuration

Standard SR-IOV, RDMA-shared and host-device profiles SHALL support a default-enabled NIC configuration template option on Kubernetes and OpenShift. Enabled templates SHALL target each selected source group's east-west NIC type and PCI addresses, route VF count from SR-IOV settings and link type from the resolved fabric, and configure PCI and Baremetal GPUDirect optimizations. Ethernet SHALL additionally configure DSCP trust and priority-3 PFC; InfiniBand SHALL omit RoCE settings. Only SR-IOV profiles SHALL disable the Mellanox plugin, preserving unrelated OCP disabled plugins. Spectrum-X SHALL keep its required templates independently of this standard-profile option.

#### Scenario: Default standard NIC configuration

- **WHEN** the option is omitted for a standard profile
- **THEN** NIC configuration templates and the profile-appropriate plugin-disable request are rendered.

#### Scenario: Explicit opt-out

- **WHEN** the option is explicitly false in YAML or on the CLI
- **THEN** no standard NIC configuration template or new plugin-disable request is rendered.

#### Scenario: Profile-owned NIC configuration templates

- **WHEN** a standard profile loads its NIC configuration template
- **THEN** it loads a template file from its own directory without referencing another profile.

#### Scenario: Standard profile selection

- **WHEN** the option is enabled for a supported standard profile and selected hardware groups
- **THEN** NCO is enabled independently of naming and templates retain exact per-source selectors, fabric-appropriate settings and configured VF count.

#### Scenario: Operator configuration ownership

- **WHEN** enabled SR-IOV configuration is applied to OpenShift with other plugins already disabled
- **THEN** mellanox is added without removing the existing disabled plugins; RDMA-shared and host-device profiles do not request this operator setting.

#### Scenario: Spectrum-X owns NIC configuration

- **WHEN** the standard-profile option is true or false with Spectrum-X
- **THEN** Spectrum-X retains its required NIC configuration templates.

#### Scenario: Single selected source receives interface naming

- **WHEN** a standard multirail profile has interface naming enabled and a single source group is selected from any inventory
- **THEN** its interface-name template retains that source's node selector and east-west rail PCI addresses; group count does not disable naming.

#### Scenario: Standard naming follows configured rail order

- **WHEN** selected east-west PFs have configured rails in a different order from their inventory entries
- **THEN** the naming template orders PCI addresses by rail number, matching policy selectors without mutating inventory; rail IDs must be contiguous from zero, with one east-west PF per rail.

#### Scenario: Unsupported standard naming layout

- **WHEN** an emitted standard interface-name template places PCI functions of one physical NIC in multiple rail lists
- **THEN** generation rejects that layout before output replacement, regardless of inventory Rail metadata or group count; it does not silently disable naming or let excluded sources block a supported selection.

#### Scenario: Whole-NIC scope protection

- **WHEN** an enabled standard template selects an east-west PF whose source inventory includes an excluded PF on the same PCI device
- **THEN** generation rejects the configuration before filtering out excluded PFs; unselected source groups do not affect the selected subset.

#### Scenario: Previously deployed template omitted

- **WHEN** opt-out or subset generation omits a previously deployed standard NIC configuration template
- **THEN** Kubernetes deploy preflight treats it as a stray, blocking unless overwrite-existing authorizes deletion; OCP preserves it, and neither platform automatically restores applied NIC settings.

### Requirement: doSPCX Spectrum-X planning

RA2.3 SHALL be limited to Network Operator 26.7. RA2.4 and RA3.1 SHALL support catalogued
releases >=26.10 with a stable default of 26.10. Both SHALL render one doSPCX
bundle in the resolved operator namespace, the selected architecture version and
per-source platformType. Platform resolution SHALL select the unique longest
case-insensitive supported platform substring in GPUType without requiring a preset,
except that a case-insensitive VR substring in MachineType SHALL take precedence
and resolve to vr, including when GPUType is empty. RA3.1 SHALL preserve the
RA2.4 rail-layout and merge safeguards. Eight-plane HWPLB generation SHALL
retain two-NIC input order and emit four PFs per NIC when each rail has two NICs.
VR with ConnectX-9 SHALL default to HWPLB with eight planes for RA2.4/RA3.1,
preserving explicit YAML/CLI values. An explicit SWPLB mode with no plane count
SHALL retain the CX9 four-plane fallback; defaults do not qualify a fabric recipe.

#### Scenario: VR dual-NIC endpoint consistency

- **WHEN** selected VR sources have four dual-NIC rails and a complete IPv4 eight-plane HWPLB topology
- **THEN** naming preserves four logical rails, while rail topology, CIDRPools and workload attachments expose eight per-NIC endpoints; each endpoint selects its four PFs and one VF, and DRA shares one GPU claim between the two endpoints in a logical rail.

#### Scenario: VR address coordinates

- **WHEN** VR topology provides explicit host indices and all eight planes of each rail
- **THEN** generation allocates a distinct NIC-index-aware /31 per NIC per worker with matching /11 and /8 routes; missing planes, conflicting coordinates, duplicated switch ports, and unsupported address modes are rejected before output replacement.

#### Scenario: Stale VR rail inventory

- **WHEN** selected RA2.4/RA3.1 VR inventory does not contain four dense rails with two physical NICs per rail
- **THEN** generation rejects the inventory and requests NetworkBay discovery instead of inferring pairs from PCI order.

#### Scenario: Unresolved discovery and generation

- **WHEN** a discovered GPU product has no unambiguous supported platform substring
- **THEN** discovery saves an empty platform and warns, while generation rejects unresolved selected RA2.4 groups before replacing output.

#### Scenario: Software-plane merge conflict

- **WHEN** source groups in one RA2.4 render bucket have different effective HWPLB software-plane assignments
- **THEN** generation rejects the merge rather than losing either assignment.

#### Scenario: Incompatible source rail layouts

- **WHEN** selected RA2.4 sources lack complete dense rail IDs, have nonuniform NICs per rail or an invalid planes-per-NIC ratio, assign one physical NIC to multiple rails, or a merged cohort has differing effective rail layouts
- **THEN** generation rejects the selection before rendering even without software-plane overrides; differing PCI addresses alone do not prevent a compatible merge.

#### Scenario: Merged RA2.4 selector includes excluded sources

- **WHEN** a selected merged RA2.4 cohort shares its GPU selector with an excluded source from another bucket
- **THEN** generation rejects the selection rather than configuring excluded workers; selecting one source remains supported.

#### Scenario: Multiple RA2.4 rail-pool parents

- **WHEN** RA2.4 scope planning would emit multiple rail-pool parents through a multi-source strict subset or multiple buckets
- **THEN** generation rejects the selection before rendering or replacing output because operator-created children share rail names; a complete compatible cohort or a single selected source remains supported.

### Requirement: Hardware source identity is explicit

SR-IOV policy and IPPool rendering SHALL preserve exact selected source identifiers
through full-bucket and strict-subset generation. Duplicate source identifiers SHALL
be rejected at the plugin boundary as well as application entrypoints. The
established unnamed sole source SHALL retain its empty ID through explicit
`[""]` source metadata rather than receiving a synthetic name.

#### Scenario: Unnamed sole source

- **WHEN** a valid single-source input has an empty identifier
- **THEN** rendering preserves the identifier and records `[""]`; scope checking requires exactly one unnamed configured source and its worker inventory, and missing configuration cannot downgrade the check.

#### Scenario: One GPU type spans separate buckets

- **WHEN** selected sources sharing a GPU label have different rail-count buckets
- **THEN** each resource retains only its intended source IDs, enabling deployment to reject a shared selector that includes another bucket's workers.

## Baseline and limits

Current profile lookup is first-match; this spec does not introduce universal ambiguity rejection. Existing render maps do not universally detect pre-overwrite collisions: the collision requirement is an extension obligation, with legacy coverage to be assessed when touched. `ScopeClusterWide` may render a namespaced ConfigMap, while SCCs may render per workload namespace. [Resource kinds](../resource-kinds/spec.md) owns API/readiness integration; [artifacts](../artifact-handoff/spec.md) owns post-render structural validation.

## Integration and evidence

- Implementation: [profile selection](../../../pkg/profiles), [profile metadata/templates](../../../profiles), [scope registry](../../../pkg/networkoperatorplugin/scopes.go), [render plan](../../../pkg/networkoperatorplugin/render_plan.go), [templates](../../../pkg/networkoperatorplugin/templates.go).
- Existing suite: [template generation](../../../pkg/networkoperatorplugin/templates_test.go); extend it with collision and full/subset integration cases, not just file counts.
- User documentation: [generation](../../../docs/advanced/generation.md), [heterogeneous clusters](../../../docs/user/heterogeneous-clusters.md).
