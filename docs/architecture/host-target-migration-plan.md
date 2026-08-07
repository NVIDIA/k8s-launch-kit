<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# HostTarget migration plan

**Status:** Implemented on the feature branch; pending pull-request review

**Date:** 2026-08-07

**Depends on:** PR #165, the
[Target-aware CLI architecture](../advanced/targets.md), and the canonical
[Launch Kit architecture](overview.md)

## Goal

Make the existing Host workflow a concrete target implementation without
changing the behavior experienced by current users.

After this migration, all of the following commands bind and execute a Host
operation through the target registry:

```bash
l8k discover ...
l8k generate ...
l8k deploy ...
l8k validate ...
l8k ...                         # legacy root pipeline
```

Adding `--target host` remains an explicit synonym. `--target dpf` remains
unavailable until the independent DPF driver is complete.

## Starting baseline

PR #165 introduced the target-neutral contracts and compatibility boundary:

```go
type Driver interface {
    Descriptor() Descriptor
    Bind(Invocation) (Operation, error)
}

type Operation interface {
    Run(context.Context) error
}
```

Before this migration, the command layer constructed a phase-scoped
`commandTargetDriver`, bound a no-op Host closure during Cobra `PreRun`, and
then executed the old Cobra `Run` body directly. That provided safe target
selection but not actual target execution.

Host orchestration is split across several boundaries:

- `discover`, `generate`, and the root pipeline construct `options.Options`
  and use `app.Launcher`;
- `app.Launcher` owns Network Operator plugin construction, one Kubernetes
  client, workflow output, and JSON finalization;
- standalone `deploy` contains manifest-directory resolution, config loading,
  Kubernetes client creation, plugin setup, apply, and dry-run handling;
- standalone `validate` contains configuration overrides, version checks,
  manifest state checks, connectivity, wait/retry behavior, report assembly,
  and HTML output;
- several application and command paths call `os.Exit` indirectly through
  output/error helpers.

## Compatibility invariants

The migration is accepted only if it preserves:

1. omitted target as Host;
2. all existing Host flag names, defaults, explicit-value semantics, and
   precedence;
3. the flat Host `cluster-config.yaml` schema;
4. the `deployment/network-operator/` artifact layout and fallback lookup;
5. generated file names and contents;
6. text output ordering and wording unless a change is explicitly approved;
7. the existing Host JSON payload shape, stdout ordering, and finalization
   behavior;
8. exit codes and structured error categories;
9. Helm, NCP, NNP, and remaining-manifest apply ordering;
10. validation checks, wait behavior, cleanup, and report-path semantics;
11. the root pipeline's current discover, generate, and optional deploy
    behavior; validation is not added to the root pipeline;
12. existing `plugin.Plugin` behavior as a Host-internal extension.

No Host target package may import DPF code. No target-neutral request may grow
Host fabric, Network Operator, or validation-specific fields.

## Non-goals

This migration does not:

- implement any DPF phase;
- introduce environment configuration or multiple Kubernetes contexts;
- redesign `config.LaunchKitConfig` or `options.Options`;
- change Host artifact paths;
- add validation to the root pipeline;
- generalize Host internal operations into a universal DAG;
- introduce the public versioned phase-result envelope;
- rename or expand the existing `plugin.Plugin` interface;
- deprecate `--enabled-plugins`.

## Target design

### Package boundary

Add a concrete Host implementation under:

```text
pkg/target/host/
|-- driver.go
|-- launcher.go
|-- deploy.go
|-- validate.go
|-- validate_config.go
|-- validate_helpers.go
`-- paths.go
```

The exact file split may follow existing test seams, but package ownership is
fixed:

- `pkg/target` remains target-neutral;
- `pkg/target/host` may import Host `app`, `options`, `config`,
  `networkoperatorplugin`, `kubeclient`, and `ui` packages;
- `pkg/cmd` owns Cobra parsing, flag-change detection, process exit, and
  translation into typed Host requests;
- Network Operator packages retain their domain logic and validators.

Future DPF code belongs in a peer `pkg/target/dpf` package rather than inside
Host or the common target core.

### Binding target-specific arguments

`target.Invocation` continues to contain only common policy:

- selected target and phase;
- output format, quiet mode, and approval policy;
- dry-run and top-level timeout.

Each command constructs an immutable Host request before registry binding.
Factories capture that typed request in a Host driver adapter:

```go
hostDriver := host.NewDeployAdapter(
    host.DeployRequest{
        Kubeconfig:          kubeconfigFlagValue,
        DeploymentFiles:     deploymentFiles,
        UserConfig:          userConfig,
        OperatorNamespace:   networkOperatorNamespace,
        OverwriteExisting:   overwriteExisting,
    },
    deployService,
)

operation, err := registry.Bind(commonInvocation)
```

The concrete names are illustrative. The constraints are not:

- Cobra and `pflag.FlagSet` never enter the Host package;
- `target.Invocation` never contains a Host request or `any` payload;
- an operation never reads mutable package-level Cobra variables after
  binding;
- request constructors defensively copy slices, maps, and other mutable values
  rather than relying on a shallow `options.Options` copy;
- required Host arguments are validated by the Host adapter or service;
- Host-owned path and environment fallback, including kubeconfig resolution,
  occurs inside the Host boundary rather than in the common runner;
- explicit false and omitted values remain distinguishable.

Use a Host-local explicit-value type where configuration precedence needs the
difference:

```go
type Explicit[T any] struct {
    Value T
    Set   bool
}
```

For the initial migration, discover, generate, and pipeline requests may
contain the existing `options.Options` value. It is already the Host options
model and snapshotting it minimizes behavioral drift. The adapter constructor
must defensively copy its mutable fields. Standalone deploy and validate
receive dedicated request structures because their orchestration is currently
embedded in Cobra and depends on phase-specific flag-change state.

Argument ownership during binding is explicit:

| Layer | Responsibility |
|---|---|
| Cobra command | Parse syntax, record whether flags changed, and construct an immutable typed request |
| Common target runner | Validate target-neutral output and execution policy and resolve target/phase capability |
| Host adapter `Bind` | Validate Host-required arguments and incompatible Host option combinations |
| Host service | Resolve Host path/environment fallback, load Host configuration, construct clients, and perform the phase |

For example, the deploy command passes the raw `--kubeconfig` value. The Host
service retains the established `--kubeconfig` → `$KUBECONFIG` → default-path
resolution. A future DPF adapter receives its own logical context references
and is not forced through Host kubeconfig semantics.

### Driver adapters

Keep the merged `Driver.Bind(Invocation)` contract. Do not replace it with a
union request or expand `plugin.Plugin`.

Provide typed phase factories such as:

```go
host.NewDiscoverAdapter(request, service)
host.NewGenerateAdapter(request, service)
host.NewDeployAdapter(request, service)
host.NewValidateAdapter(request, service)
host.NewPipelineAdapter(request, service)
```

Each factory returns a `target.Driver` that:

1. publishes the common Host descriptor;
2. accepts only its bound phase;
3. validates common and Host-specific requirements;
4. returns an operation capturing the immutable request and service;
5. performs no work during `Bind` beyond validation and construction.

The Host descriptor moves out of `pkg/cmd` and becomes the single source used
by both the registry and schema.

### Command runner

Introduce one command helper that:

1. parses the selected target;
2. validates ownership of explicitly changed flags;
3. constructs the phase-specific target drivers;
4. binds the common invocation;
5. executes `operation.Run(cmd.Context())` exactly once;
6. translates the returned error through the existing structured error and
   exit-code path.

The outer command boundary remains responsible for process termination. Host
services and operations return errors and do not call `os.Exit`.

No converted command retains the old no-op `PreRun` guard. Target ownership,
binding, operation execution, and final process exit each have one owner.

## Phase services

### Discover

The first adapter may call the existing `app.Launcher` with a copied
`options.Options` value configured for discover-only behavior.

Preserve:

- kubeconfig resolution;
- source config lookup and comment-preserving write-back;
- explicit profile overrides;
- node labeling and discovered group behavior;
- preset and configuration source precedence;
- output path selection;
- warning and error categories.

The operation receives `cmd.Context()`. Existing discovery code should use
that context instead of creating an unrelated background context where doing
so can be changed without observable behavior.

### Generate

The initial adapter may also use `app.Launcher` and existing
`options.Options`.

Preserve:

- config discovery and `--config-dir` ownership;
- hardware defaults, explicit CLI precedence, and resolved-config write-back;
- preset substitution and source inventory restoration;
- profile selection and group filtering;
- output directory cleaning and manifest content;
- optional integrated deploy behavior;
- warnings and JSON result behavior.

Integrated `generate --deploy` and the root pipeline must reuse the same Host
deploy service or preserve the exact current launcher call path until that
service can be shared without changing its semantics.

### Deploy

Extract standalone deployment from `pkg/cmd/deploy.go` into a Host service.
The typed request includes:

- kubeconfig path or reference;
- deployment-files path;
- optional user config;
- Network Operator namespace override;
- overwrite-existing policy;
- common dry-run and timeout policy from `target.Invocation`.

The service owns:

- Network Operator subdirectory preference and supplied-directory fallback;
- config loading and selected-release resolution;
- Kubernetes client and REST config construction;
- Network Operator plugin configuration;
- Helm and manifest apply orchestration;
- dry-run reporting and terminal success/error classification.

Inject the narrowest practical side-effect boundaries for tests, such as the
Kubernetes client factory and manifest applier. Do not build a generic
cross-target executor framework in this stage.

### Validate

Extract standalone validation from `pkg/cmd/validate.go` into the Host package.
Split the implementation by responsibility if needed:

- request/config resolution and explicit overrides;
- version and component checks;
- manifest-state validation and optional wait;
- connectivity checks and cleanup;
- preset/topology comparison;
- verdict aggregation;
- text/JSON emission data;
- HTML report assembly and synchronous write.

The typed request contains all standalone validation inputs, with `Explicit`
values for flags where omission must allow config defaults. It does not retain
a Cobra command merely to call `Flag(name).Changed` later.

Report writing remains synchronous on every exit path. The current application
uses `os.Exit`, which skips deferred functions; moving process termination to
the outer command boundary must not accidentally turn report generation into
best-effort deferred cleanup.

### Root pipeline

The root adapter captures the current pipeline `options.Options` and calls the
existing launcher behavior through a Host operation.

It must retain:

- the bare `l8k` help behavior;
- discover, generate, and optional deploy ordering;
- partial-success classification;
- shared generated-profile state used by integrated deploy;
- current JSON finalization;
- the fact that validate remains a separate command.

## Output and process boundary

Application and Host service code return typed errors rather than terminating
the process. The Cobra boundary remains the only owner of exit codes.

The migration must pay particular attention to JSON mode:

- stdout contains exactly one JSON object;
- human logs remain on stderr;
- errors are finalized once, not by both `app.Launcher` and Cobra;
- the existing Host JSON schema does not gain target fields during this work;
- report and artifact writes finish before the outer boundary exits.

A reusable common phase-result envelope is intentionally deferred until the
first DPF automation path is designed. HostTarget may return internal service
results, but the `target.Operation` contract remains error-only for this
migration unless a separate reviewed ADR changes it.

## Single-PR implementation sequence

The approved delivery combines the migration into one reviewable pull request
while retaining clear internal work areas:

1. Characterize the existing Host contracts with target binding, subprocess,
   path resolution, output, and error-boundary tests.
2. Add immutable phase requests, explicit-value markers, the Host descriptor,
   and phase adapters under `pkg/target/host`.
3. Extract standalone deploy and validate orchestration from Cobra into Host
   services without changing apply order, validation semantics, or reports.
4. Route discover, generate, the root pipeline, deploy, and validate through
   the registry and execute one bound operation with the command context.
5. Move process termination to the outer command boundary, remove the no-op
   Host `PreRun`, and keep DPF unavailable before Host dependencies are built.
6. Update CLI architecture documentation, the canonical full component map,
   and bundled agent skills in the same pull request.
7. Run the compatibility matrix and complete a separate critical diff review
   before opening the pull request.

## Test matrix

### Binding and unit tests

- every Host flag is copied into the expected typed request;
- explicit false differs from omission;
- required deploy and validate inputs fail during Host binding;
- the common invocation contains no Host-only fields;
- an operation uses the request snapshot even if Cobra globals later change;
- wrong-phase binding fails deterministically;
- service errors preserve structured categories and causes;
- cancellation and timeout reach the service.

### Compatibility tests

For omitted target and explicit `--target host`, compare:

- resolved requests;
- text stdout/stderr;
- JSON stdout and exit code;
- saved discovered configuration;
- generated file list and file hashes;
- deployment service calls and options;
- validation verdict, warnings, report path, and HTML content;
- config lookup and deployment-directory fallback;
- root pipeline phase sequence.

Normalize only known volatile fields such as timestamps. Do not normalize
resource names, paths, messages, error codes, or manifest content.

### Failure-path tests

- invalid and unreachable kubeconfig;
- missing or malformed Host config;
- unknown profile and mismatched group filters;
- Helm conflict and apply failure;
- reconciliation timeout and cancellation;
- missing, progressing, stale, and failed manifests;
- connectivity failure and cleanup;
- report-write failure;
- JSON failure output and exit status match the pre-migration command;
- `--target dpf` exits before any Host dependency is constructed.

### Repository verification

Each routing PR runs:

```bash
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go test -race -count=1 ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build ./...
golangci-lint run ./...
mkdocs build --strict --clean
```

CLI/help changes also require regenerated README help and consistent bundled
skills. Pure code movement with no user-visible behavior still updates the
architecture documentation and agent guidance describing the execution path.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Cobra globals are read after binding | Capture immutable request values before registry binding and test mutation afterward |
| Explicit false is lost | Use `Explicit[T]` populated from `Flag.Changed` |
| JSON is emitted twice | Move finalization and exit ownership to one outer boundary; retain subprocess tests |
| Error category or exit code changes | Compare structured errors and process exit codes against pre-migration fixtures |
| Deploy path resolution changes | Characterize Network Operator subdirectory preference and fallback before extraction |
| Validation report is skipped on error | Write synchronously before returning the terminal error |
| Context changes shorten existing operations | Preserve zero/unbounded timeout semantics and add cancellation tests |
| Service extraction becomes a redesign | Keep current Host types and dependencies; defer generic executors and common results |
| DPF contaminates Host abstractions | Keep DPF unavailable and enforce package import boundaries |

## Completion criteria

The HostTarget migration is complete when:

- all four lifecycle commands and the root pipeline execute a bound Host
  operation;
- no lifecycle command uses the no-op Host `PreRun` adapter;
- `pkg/cmd` contains parsing, typed request construction, and process-boundary
  concerns rather than Host orchestration;
- standalone deploy and validation orchestration live outside Cobra;
- Host requests are typed and immutable;
- omitted target and explicit Host pass the complete compatibility matrix;
- no Host manifest, config, report, JSON object, or exit code changes without
  explicit approval;
- DPF remains unavailable and cannot enter Host code;
- schema/help continue to advertise accurate target and flag capabilities;
- the full test, race, vet, build, lint, and documentation checks pass.

After these criteria are met, Launch Kit has a real Host target and a stable
runtime seam on which the independent DPF vertical slice can be built.
