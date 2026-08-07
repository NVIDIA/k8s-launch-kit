<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF integration roadmap

**Status:** Accepted direction; implementation in progress

**Last reviewed:** 2026-08-07

**Current repository snapshots:**

- `k8s-launch-kit`: `4d083d5` — target-aware CLI foundation merged in PR #165;
- `dpf-install`: `20f1371`;
- `dpf-operator`: `97570f8`.

This plan describes how Launch Kit will absorb the useful behavior of
`dpf-install` while preserving the existing Network Operator host workflow.
The implemented target boundary is documented in
[Target-aware CLI architecture](../advanced/targets.md). The next Launch Kit
milestone is specified in the
[HostTarget migration plan](host-target-migration-plan.md). The current
repository-wide component and data-flow map is maintained in the
[Launch Kit architecture](overview.md).

## Objective

Launch Kit becomes a target-aware environment orchestrator. It retains one
public lifecycle:

```text
discover -> generate -> deploy -> validate
```

The lifecycle applies independently to the host and DPF management domains,
and later to a composed environment:

```text
Management cluster
  `-- DPF Operator and DPF custom resources
       `-- one or more generated DPU clusters

Workload/host cluster
  `-- Network Operator and host-side networking

External infrastructure
  |-- BlueField BMC and Redfish
  |-- local or jump-node command execution
  |-- storage providers
  `-- optional host provisioning
```

The management and workload roles may resolve to the same Kubernetes context
in Host Trusted deployments. They remain distinct logical roles because they
have different ownership, credentials, and lifecycle boundaries.

## Accepted architectural decisions

The following decisions are now established:

1. `host` and `dpf` are targets; an existing Network Operator plugin is not a
   target abstraction.
2. Omitting `--target` always means `host`.
3. Existing host commands, flags, configuration, artifacts, JSON, and exit
   codes remain compatible.
4. `dpf` is the canonical target name. A DPU is infrastructure managed by DPF,
   not the top-level target name.
5. Target-native configuration remains separate. Launch Kit will not create a
   host/DPF union configuration structure.
6. DPF owns its profile schema, release defaults, and generated manifests.
7. Environment composition sits above independently functional target
   drivers.
8. Destructive physical actions require named approval and are never implied
   by JSON or noninteractive mode.

## Current implementation status

PR #165 delivered the compatibility-first command boundary:

- `pkg/target` contains target names, phases, descriptors, capabilities,
  registry validation, invocation policy, and bound operations;
- lifecycle commands accept optional `--target`, with `host` as the default;
- explicitly supplied flags are validated against target ownership;
- `l8k schema` advertises target and phase capabilities additively;
- all DPF phases are recognized but unavailable, so DPF cannot fall through to
  host execution;
- the original host command bodies remained unchanged at that foundation
  stage.

The follow-on HostTarget migration is implemented on the current feature
branch and pending pull-request review. Every lifecycle phase now binds a
typed, immutable Host request through the registry and executes a concrete
Host operation. Standalone deploy and validate orchestration live outside
Cobra, process exit remains at the command boundary, and DPF is rejected
before any Host dependency is constructed.

The DPF generation dependency is also still open. Current `dpf-operator` main
does not expose a public profile renderer or a stable `dpfctl generate`
contract. Launch Kit must not copy documentation manifests to bypass this
boundary.

## Responsibility boundaries

### Common target runtime

The common runtime owns only concerns with identical semantics across targets:

- target and phase resolution;
- common output and execution policy;
- phase start, completion, and error categorization;
- logical context and artifact resolution;
- top-level cancellation and timeouts;
- approvals and journal integration;
- the versioned automation result envelope.

It does not own host fabric choices, DPF trust mode, target-native defaults,
manifest rendering, readiness semantics, or internal operation ordering.

### Host target

The Host target owns the current Network Operator behavior:

- host `cluster-config.yaml` and CLI precedence;
- node, NIC, GPU, profile, and topology discovery;
- Network Operator profile matching and manifest rendering;
- Helm installation, NCP/NNP/network apply, and reconciliation;
- manifest, version, connectivity, and report validation;
- existing `plugin.Plugin` implementations.

Host-only artifacts remain under:

```text
deployment/network-operator/
```

### DPF target

The DPF target owns:

- typed DPF configuration and trust mode;
- management, workload, and generated DPU-cluster context requirements;
- BMC, Redfish, host PCI/rshim/MFT, and prerequisite discovery;
- DPF prerequisites, operator, system, BFB, flavor, deployment, and service
  plans;
- DPF resource readiness and DPU operational validation;
- DPF-specific evidence and diagnostics.

DPF-only artifacts use a peer directory:

```text
deployment/dpf/
```

### Environment composer

The composer receives complete target plans and owns only cross-target
contracts:

- shared-component ownership;
- singleton `NicClusterPolicy` composition;
- PF/VF and resource-name ownership;
- topology and host-to-DPU relationships;
- maintenance mutual exclusion;
- release compatibility;
- dependencies between target operations.

It does not merge target-native configuration structures.

## Phase semantics

| Phase | Host target | DPF target | Composed environment |
|---|---|---|---|
| Discover | Nodes, NICs, GPUs, capabilities, profiles | Contexts, existing DPF resources, BMC/Redfish inventory, host prerequisites, storage and load-balancer prerequisites | Correlate hosts, BlueFields, BMCs, and DPU clusters; identify ownership conflicts |
| Generate | Network Operator values and custom resources | Prerequisite, operator, system, provisioning, and use-case subplans | Resolve versions, shared components, contracts, and the operation DAG |
| Deploy | Network Operator Helm plus ordered NCP/NNP/network reconciliation | DPF Helm, system resources, discovery gate, BFB/provisioning, and services | Execute operations in dependency order across contexts |
| Validate | Host reconciliation, versions, and connectivity | DPF conditions, DPU phases, operational health, and DPU-cluster workloads | Verify topology, VF advertisement, CNI, traffic, storage, and maintenance contracts |

Discovery may be incomplete, but every value must be classified as
`observed`, `configured`, `derived`, or `unresolved`. Generation fails when a
required value remains unresolved. Launch Kit must not substitute plausible
lab defaults for CIDRs, BMC ranges, load-balancer addresses, BFB versions,
interface mappings, or service-network parameters.

The in-cluster `DPUDiscovery` resource is a deploy-time discovery gate and a
validation surface. It does not require another public lifecycle phase.

## Configuration model

Configuration evolves in three layers:

1. Host-only runs retain the flat `cluster-config.yaml` format.
2. DPF-only runs use a typed `dpf-config.yaml` owned by the DPF target.
3. Multi-target runs use a versioned environment document that references the
   target-native files.

```yaml
apiVersion: launchkit.nvidia.com/v1alpha1
kind: Environment
metadata:
  name: dpf-lab
spec:
  clusters:
    management:
      kubeconfigRef: management
    workload:
      kubeconfigRef: workload
    dpu:
      source: DPUCluster

  targets:
    host:
      configRef: cluster-config.yaml
    dpf:
      configRef: dpf-config.yaml

  executors:
    default: local
    bootstrap:
      type: ssh
      host: jump-node
      credentialRef: jump-node-credentials

  approvals:
    allowed: []
```

Resolved secrets are never written to configuration, plans, journals, logs,
or validation reports. Persist only references to environment variables,
files, Kubernetes Secrets, or credential providers.

## Environment artifacts

Host-only and DPF-only runs keep their existing peer layouts. A composed
environment may add:

```text
deployment/
|-- environment.yaml
|-- inventory.yaml
|-- plan.yaml
|-- catalog.lock.yaml
|-- checksums.yaml
|-- contracts/
|   |-- ownership.yaml
|   |-- topology.yaml
|   |-- networking.yaml
|   |-- maintenance.yaml
|   `-- compatibility.yaml
|-- targets/
|   |-- host/
|   `-- dpf/
|       |-- prerequisites/
|       |-- operator/
|       |-- system/
|       `-- usecase/
`-- validation/
```

The generated plan records source and release provenance, a non-secret digest,
operations and dependency edges, destination context, readiness gates,
destructive-action classification, timeout and retry policy, dry-run support,
compensating or uninstall actions, and expected resource identities.

## Revised implementation roadmap

### Stage 0: Target-aware command foundation — complete

Delivered by PR #165:

- target vocabulary and explicit registry;
- host-by-default CLI selection;
- target-owned flag validation;
- additive schema capabilities;
- unavailable DPF guard;
- compatibility-oriented tests and documentation.

### Stage 1: Finish HostTarget execution — implemented, pending review

Follow the [HostTarget migration plan](host-target-migration-plan.md):

- characterize all host phase contracts;
- extract standalone deploy and validation orchestration from Cobra;
- bind immutable, phase-specific host requests;
- execute every host phase through the bound target operation;
- preserve existing host output, artifacts, errors, and exit codes;
- remove the no-op `PreRun` adapter after every phase is migrated.

Exit criteria: omitted target and explicit `--target host` use the same
concrete driver and are behaviorally equivalent across all four commands and
the root pipeline.

### Stage 2: Establish the DPF-owned generation contract — parallel prerequisite

DPF should provide, in preference order:

1. a small separately versioned Go rendering module;
2. a stable, release-matched `dpfctl generate` CLI contract;
3. the external `dpf-install` process only as a temporary schedule bridge.

The contract must provide a typed profile schema, deterministic artifacts,
artifact identities and ordering, release provenance, compatibility metadata,
and golden fixtures. Launch Kit must not import the full operator dependency
graph unnecessarily or copy DPF documentation manifests.

Exit criteria: a released DPF-owned interface deterministically renders the
selected profile and release without local Go `replace` directives.

### Stage 3: DPF discovery and `dpf-install` import

Start with Zero Trust passthrough:

- add typed `dpf-config.yaml` and logical context roles;
- import `dpf-install` setup data without importing numeric checkpoints;
- discover existing DPF resources, contexts, BMC/Redfish inventory, host
  prerequisites, storage, and load-balancer state;
- preserve credential references without materializing secrets;
- report the provenance and resolution state of every required value.

Exit criteria: `l8k discover --target dpf` produces a complete or explicitly
unresolved DPF configuration and never guesses infrastructure values.

### Stage 4: DPF generation for Zero Trust passthrough

- enable `l8k generate --target dpf`;
- consume the DPF-owned renderer;
- render prerequisites, operator, system, and passthrough subplans under
  `deployment/dpf/`;
- produce checksums, a release lock, expected resource identities, readiness
  gates, and a target operation DAG;
- introduce a versioned target automation result without changing the
  existing host JSON contract.

Exit criteria: generated output is deterministic, release-matched, contains no
secrets, and matches DPF golden fixtures.

### Stage 5: DPF deploy, resume, validate, and diagnostics

- implement Kubernetes and Helm executors first;
- add local/SSH and BMC execution only where DPF APIs cannot express the
  required action;
- replace numeric stages with an atomic, digest-bound operation journal;
- query live state before skipping any resumed operation;
- require named approval for DPU reimage, reboot, credential mutation, and
  other destructive actions;
- add per-kind validators for `DPFOperatorConfig`, `DPUCluster`,
  `DPUDiscovery`, `DPUDevice`, `DPU`, and `DPUDeployment`;
- reject stale conditions whose observed generation is behind;
- inspect generated DPU-cluster workloads and integrate DPF describe, dump,
  and sosreport evidence.

Exit criteria: a one-DPU Zero Trust deployment is idempotent, resumes after an
interruption or reboot, and passes management-plane, provisioning, DPU-cluster,
and passthrough validation.

### Stage 6: Host Trusted HBN plus OVNK and environment composition

After Host and DPF work independently:

- resolve target configs through an environment document;
- generate both target plans independently;
- arbitrate NFD, Multus, Maintenance Operator, storage, CNI, and device-plugin
  ownership;
- merge DPF host requirements into the singleton `NicClusterPolicy`;
- reject PF/VF overlaps and duplicate resource names before deployment;
- resolve a tested DPF, Network Operator, Kubernetes, BFB, firmware, DOCA/OFED,
  and service-chart cohort;
- add host-to-DPU accelerated-CNI and connectivity validation.

Exit criteria: Host Trusted HBN plus OVNK is deployed from one composed plan
without competing shared resources or uncoordinated disruptive actions.

### Stage 7: Remaining parity and lifecycle

- HBN with GPU/RDMA rails;
- SNAP and storage offload;
- coordinated host/DPU maintenance;
- unified topology and health;
- VPC/isolation and advanced services;
- upgrade, uninstall, use-case replacement, and optional host scale-out;
- parity fixtures for every supported `dpf-install` use case.

If an external compatibility wrapper is introduced, retire it only after the
native path passes identical fixtures and hardware-lab parity gates.

## `dpf-install` parity map

| `dpf-install` capability | Launch Kit destination |
|---|---|
| Setup YAML and precedence | Typed DPF configuration plus `l8k import dpf-install` |
| Interactive setup | Discovery output and explicit missing-field prompts |
| Local and remote execution | Executor abstraction |
| Tool installation and repository clone | Read-only preflight; explicit bootstrap actions only |
| BMC password and serial handling | BMC discovery plus opt-in credential/bootstrap action |
| OVN-K primary CNI | Generated Host target dependency |
| NFS/local storage setup | Pluggable storage provider |
| DPF Operator installation | DPF target deploy |
| DPF system resources | DPF system subplan |
| Use-case stage | Versioned DPF profile catalog |
| Kubespray scale-out | Optional host-provisioner executor |
| Sysinfo | Target validation report and `sosreport` |
| Checkpoint/resume | Digest-bound journal plus live-state queries |
| Validate-only | First-class validate phase |
| Dry run | Per-operation preview classification |
| Upgrade | Future `l8k upgrade --target dpf` |
| Clean/uninstall | Explicit uninstall lifecycle |
| Use-case switching | Plan diff, ordered removal, approval, then deployment |

## Prioritized cross-plane use cases

| Priority | Use case | Purpose |
|---|---|---|
| P0.1 | Zero Trust passthrough | First independent DPF vertical slice with minimal Host coupling |
| P0.2 | HBN plus OVNK accelerated primary CNI | First complete Host/DPF composition and singleton-policy proving point |
| P1 | HBN underlay plus GPU/RDMA rails | Unified workload-to-uplink topology and validation |
| P1 | HBN plus SNAP storage | Coordinated network and storage offload |
| P1 | Coordinated maintenance | Prevent simultaneous host driver and DPU disruption |
| P1 | Unified topology and health | Correlate host, PCI, DPU, and BMC state |
| P2 | VPC and isolation | Tenant isolation spanning Host and DPU |
| P2 | Argus, DTS, and Blueman | Cross-plane observability and security evidence |
| P2 | Multi-DPU roles | Place network, storage, and telemetry services without PF/VF conflicts |
| P3 | Firefly/PTP and DOCA Weave | Follow-on work after core parity and upstream maturity |

Zero Trust passthrough and Host Trusted HBN plus OVNK remain the first two use
cases, but they ship sequentially. This reduces the first DPF milestone's
dependency on environment composition while retaining HBN plus OVNK as the
first full cross-plane proof.

## Acceptance strategy

Required layers include:

- unit tests for schemas, precedence, operation ordering, and ownership;
- golden tests against the DPF renderer;
- fake local, SSH, Helm, Kubernetes, and Redfish executors;
- envtest coverage for DPF validators and stale-condition cases;
- management-cluster tests without hardware;
- one-DPU Zero Trust and Host Trusted hardware-lab tests;
- combined Network Operator and DPF tests;
- cancellation, retry, reboot, journal-corruption, and rollback injection;
- parity fixtures for every supported `dpf-install` profile.

The final parity gate requires:

- generated resources match the selected DPF release;
- existing setup fields import without importing old checkpoint authority;
- no secret appears in an artifact;
- deployment is idempotent;
- resume verifies live state;
- stale readiness never passes validation;
- unsupported version combinations fail before deployment;
- shared-component and PF/VF conflicts fail before deployment;
- destructive actions require explicit approval;
- every phase emits a stable automation result and evidence index;
- composed validation proves control-plane readiness and Host/DPU data paths.

## Decisions still required

1. Will DPF publish a small public Go renderer or a stable `dpfctl generate`
   contract?
2. Which precise DPF 26.4, Network Operator, Kubernetes, BFB, firmware, and
   service-chart versions form the first tested cohort?
3. Which DPF APIs cover reimage, reboot, and credential workflows, and which
   actions still require out-of-band executors?
4. Which destructive bootstrap actions are supported initially? The default
   recommendation excludes BMC password rotation.
5. What is the ownership and release cadence of the environment compatibility
   descriptor?

Safe defaults remain: omission means Host, DPF 26.4 is the provisional parity
baseline, Zero Trust passthrough ships first, Kubespray is optional, BMC
password rotation is deferred, and environment composition begins only after
both target drivers implement all four phases independently.
