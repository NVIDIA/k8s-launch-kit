<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Target-aware CLI architecture

Launch Kit uses the same four lifecycle commands for independently managed
infrastructure domains:

```text
discover -> generate -> deploy -> validate
```

The existing Network Operator workflow is the `host` target. It remains the
default, so existing commands, configuration files, artifacts, JSON output,
and exit codes do not require migration:

```bash
l8k discover ...
l8k generate ...
l8k deploy ...
l8k validate ...
```

Passing `--target host` is an explicit synonym:

```bash
l8k generate --target host ...
```

The `dpf` target provides [inventory discovery](../../docs/user/dpf-discovery.md)
through explicit Redfish or host/ARM SSH sources, optionally with management
Kubernetes observations. Inventory acceptance is independent of provisioning
readiness; the selected use case evaluates deployment eligibility.

Standalone HBN-BGP generation and [deployment](../../docs/user/dpf-deployment.md)
use typed adapters, resolved desired inputs and a validated staged bundle.
Deployment includes runtime validation for the selected hosts and DPUs.
Standalone `validate` and the combined pipeline remain unavailable and cannot
fall through to Host execution. Hardware and diagnostic-image support remain
subject to the documented qualification limits.

Use `l8k schema` to inspect target and phase availability programmatically.

## Flag ownership

Flags are classified by their exact semantics, not by whether their names
sound generic:

- target-agnostic flags have identical meaning for every target, such as
  `--target`, `--output`, and logging controls;
- host flags describe Network Operator behavior, such as `--fabric`,
  `--deployment-type`, Spectrum-X settings, and connectivity checks;
- existing path and kubeconfig flags remain host compatibility inputs until a
  multi-context environment contract gives them target-independent semantics.

Help output separates target selection, host configuration, and
target-agnostic output controls. Every target-aware Cobra flag also carries a
`launchkit.nvidia.com/targets` annotation. The same ownership is exposed in
`l8k schema` through each flag's `targets` field.

Only explicitly supplied flags are checked for target compatibility. Defaults
do not become accidental overrides. This distinction preserves commands such
as `--multirail=false` and `--connectivity=false`:

```console
$ l8k validate --target dpf --connectivity=false
Error: invalid target invocation: flags not valid for target "dpf":
  --connectivity (targets: host)
```

## Binding boundary

The target package contains no Cobra or target-native configuration types.
The command layer performs the following sequence:

1. Parse the target, defaulting omission to `host`.
2. Check explicitly changed flags against their target ownership.
3. Resolve the selected target and phase through the registry.
4. Ask the target CLI adapter to bind its typed arguments.
5. Execute the resulting target-neutral operation.

The central contracts are:

```go
type Driver interface {
    Descriptor() Descriptor
    Bind(Invocation) (Operation, error)
}

type Operation interface {
    Run(context.Context) error
}
```

`Invocation` contains only target-neutral output and execution policy. The
Cobra layer captures explicitly changed flags into a typed, immutable Host
request. A phase-specific adapter validates and snapshots that request while
binding the target-neutral invocation. Drivers never receive a Cobra command,
`pflag.FlagSet`, untyped option map, or host/DPF union config.

Every Host lifecycle command now follows this boundary. The common runner
binds through the registry and calls `Operation.Run(cmd.Context())` exactly
once. Discover, generate, and the root pipeline reuse the Host application
launcher; standalone deploy and validate are Host-owned services outside
Cobra. Those services return errors, while `pkg/cmd` remains the only process
termination boundary. The DPF discovery branch binds its own typed request before collecting Host flags, and constructs only DPF dependencies.

## Host platform selection

Host deploy and validate select one immutable `hostplatform.Selection` after
existing config resolution and CLI overrides. Omitted flavor retains Kubernetes.
The selection is passed explicitly through deployment, readiness polling and
secondary-network policy checks. These consumers do not derive their platform
from config again. Serialized `LaunchKitConfig.Flavor` remains part of config
handoff, but it is not another platform authority for these consumers. Lifecycle
entry points reject an unknown or omitted selection before cluster checks or
effects. Resource-only paths supply a nil config and an explicit selection.

Standalone deploy requires successful config resolution before strict selection.
Static validation retains its existing best-effort treatment of partially parsed
config after a resolution error: only `flavor: ocp` selects OpenShift, otherwise
it selects Kubernetes. This mapping occurs once at the validation boundary;
lower consumers validate and use only the resulting explicit selection.

Kubernetes Helm ownership remains independent of platform: `skipHelmChart`
disables its Helm boundary while resource handling continues. OpenShift requires
external operators and configures their existing resources at the existing
points in deployment. The current phase order and check differences remain:
OpenShift deploy runs preflight before filtering stray-CR results; standalone
OpenShift validate skips Helm checks without a comprehensive OLM prerequisite
check, and its stray-CR scan does not cover every operator namespace.

Rendering, connectivity and cleanup retain their current owners. Cleanup still
rejects OpenShift and checks the live ClusterVersion API before mutations.
Pool collision checking reads the OpenShift Network API when available regardless
of the selected flavor. Platform selection is never inferred from that API.

## Registry guarantees

Registry construction rejects:

- nil drivers;
- invalid or duplicate target names;
- unknown phases;
- unavailable phases without a reason;
- target descriptors that omit any of the four public lifecycle phases.

Binding rejects unknown targets, unavailable target phases, invalid common
execution policy, and nil operations. Target names are lower-case identifiers;
the CLI does not silently normalize misspellings.

## Adding a target

A target integration should:

1. Define a typed target configuration outside the host `config` and `options`
   packages.
2. Register target-specific flags separately and annotate their ownership.
3. Prefer configuration fields over a broad set of target-specific CLI flags.
4. Bind explicit CLI values into a typed request; do not pass Cobra to the
   domain driver.
5. Declare all four phase capabilities and their required logical cluster
   roles.
6. Mark only implemented phases available; document preview and qualification limits.
7. Keep target artifacts separate; host output remains
   `deployment/network-operator/`.
8. Add omitted-target and explicit-host compatibility tests before routing the
   host command through a new adapter.

The existing `plugin.Plugin` interface remains a host profile/rendering
extension. It is not the target abstraction.

## Future context model

`--kubeconfig` currently means the host workload cluster and therefore remains
host-owned. DPF will require logical context roles such as `management`,
`workload`, and generated `dpu` clusters. A later environment layer will map
those roles to credentials without changing the four lifecycle command names.

That environment layer composes target plans and cross-target ownership
contracts; it does not merge host and DPF configuration structures.

## Implementation plans

- [Complete Launch Kit architecture](../architecture/overview.md)
- [DPF integration roadmap](../architecture/dpf-integration-plan.md)
- [HostTarget migration plan](../architecture/host-target-migration-plan.md)
