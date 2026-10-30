<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# DPF discovery preview

`l8k discover --target dpf` inspects an explicitly selected management Kubernetes
cluster and DPU/host BMCs. The DPF estate is independent of Host networking.
The initial management topology requires three control-plane nodes.

This build exposes an **experimental inventory preview**. The hardware
qualification catalog is empty: reads produce candidate facts and a partial
report, with qualification failures visible and exit `2`. No server/firmware
combination is declared supported by a successful GET. Management probes are
implemented using an existing digest-pinned diagnostic image, with orchestration
and validation in Go. The catalog pins `docker.io/nicolaka/netshoot:v0.16`, but
Kubernetes runtime qualification is pending. The default `management` mode
rejects locally until the catalog enables that qualified image/architecture cohort;
explicitly choose `inventory` for the preview. There is no automatic downgrade.
DPF generate/deploy/validate and the root DPF pipeline remain unavailable.

## Inputs and invocation

Supply three bundles: management kubeconfig access, selected DPU BMC origins with
credentials, and host BMC origins with credentials. All addresses must come from
site inventory; the CLI does not scan subnets or derive endpoints from IP/MACs.
A host may contain several selected or unselected cards.

```yaml
management:
  kubeconfigRef:
    file: ./credentials/management.kubeconfig
  context: management
credentials:
  dpu-reader:
    username: bmc-reader
    passwordRef:
      env: DPU_BMC_PASSWORD
  host-reader:
    username: bmc-reader
    passwordRef:
      env: HOST_BMC_PASSWORD
discovery:
  dpus:
    - bmcEndpoint: https://192.0.2.11
      credentialRef: dpu-reader
  hosts:
    - bmcEndpoint: https://192.0.2.21
      credentialRef: host-reader
  probes:
    mode: inventory
```

```bash
l8k discover --target dpf --user-config dpf-site.yaml \
  --save-cluster-config dpf-discovered.yaml --output json
```

The YAML is ordinary CLI configuration. `--target` chooses the schema; generated
facts live under `inventory`. There is no CRD wrapper. Unknown fields, duplicate
keys, wrong scalar types, ambiguous contexts and duplicate endpoints fail before
remote access. Lists and credentials stay in YAML; no password CLI flag exists.

| Source | Values |
| --- | --- |
| Required site inputs | Explicit kubeconfig file; nonempty DPU and host lists; each origin's credential alias; existing username and env/file password reference |
| Defaultable | Context from the supplied file; verified system trust; collection 10m; request 15s; four BMCs in flight |
| Conditional | Private `discovery.tls.caFile`; endpoint TLS override; `systemURI` for multi-System BMCs |
| Observed | Physical-card/system identities, linked host adapters, component firmware, auth/time unknowns, management nodes/version |
| Release owned | Qualified adapter completeness predicates and the probe image/architecture digests |
| Later phases | Profile/release cohort, DPU API allocation, DHCP/service networks and controller authentication |

The management kubeconfig must use verified HTTPS and certificate/key or bearer
credentials. File and embedded representations of the same credential/trust field are mutually
exclusive; choose token or tokenFile. Exec/auth-provider plugins, basic and anonymous access are rejected
in this version. Ambient kubeconfig files are never merged. BMC authentication
uses one session login using the supplied username/password; Basic fallback is
not included in this preview. Session writes are restricted to the standard
`/redfish/v1/SessionService/Sessions` collection and its exact returned session
child; alternate OEM session paths are unsupported. No password guessing or credential repair occurs.

Passwords resolve only from `{env: NAME}` or `{file: path}`. Environment values
are verbatim. UTF-8 files lose at most one final LF/CRLF. File and CA references
are relative to the input YAML and are rebased when saved elsewhere. Private files
are not copied. Endpoint origins require HTTPS; redirects, cross-origin links,
unbounded pages and arbitrary action endpoints are rejected.

## Observations, reports and refresh

Physical identity comes from qualified card/system fields, keeping raw serials.
Ports/PCI functions, CPU serials and addresses cannot substitute for card identity.
Every requested endpoint remains represented on failure; unrelated cards remain
outside selection. Unknown firmware/auth/time values stay unknown. Qualification
requires both a reviewed model/firmware tuple and an adapter-specific completeness
predicate; no site input can waive that requirement.

Complete discovery writes the requested YAML. Partial collection preserves an
existing handoff and writes `partial.yaml` plus `report.json` beneath
`<output-stem>.discovery/<run-id>/`. JSON stdout is one final DPF result, with
progress on stderr; syntax errors before dispatch retain normal Cobra stderr.
The report contains facts, checks, factual diffs, later-phase gaps, errors and
successfully written artifacts. Saved YAML alone contains the factual handoff.
Explicit in-place refresh requires the same input/output path; it keeps
`source-before.yaml`. Stable sidecar locks serialize cooperating writers. External
edits between the final hash check and rename cannot be excluded; avoid concurrent
editing. Output paths must use real directories without symlink aliases.

| Exit | Meaning |
| --- | --- |
| 0 | Explicit-mode checks, cleanup and publication completed |
| 2 | Invalid input, unqualified required capability or proven mismatch |
| 3 | Required observation, authentication, TLS, API or remote cleanup failure |
| 1 | Cancellation or local publication/encoding error |

## Management probe implementation and qualification

Launch Kit builds no discovery image. The release catalog pins the existing
[`nicolaka/netshoot:v0.16`](https://github.com/nicolaka/netshoot/releases/tag/v0.16)
image by manifest and Linux amd64/arm64 digests. A fixed, embedded diagnostic
adapter invokes its `getent`, `curl`, `ip` and `jq` tools; the Go CLI resolves
destinations/public trust and validates the versioned JSON results. No Python,
runtime package installation, downloaded helper or site-supplied command is used.
The image repository can be mirrored without changing its pinned digest.

The NVIDIA candidate
[`nvcr.io/nvidia/k8s/toolbox`](https://catalog.ngc.nvidia.com/orgs/nvidia/k8s/containers/toolbox/latest/layers)
has suitable tools, but its inspected current image is based on Ubuntu 23.04 and
was built in September 2023. DOCA 3.3.0 base/full runtime images were also inspected;
both lack `curl` and `ip`. Netshoot is the selected existing-image alternative.

The runner uses an existing namespace, an immutable ConfigMap containing the
public request, public CAs and fixed adapter, and exact run-owned Pods. It creates no namespaces, RBAC, Secrets,
operators or DPF objects. Each management node has separate Pod-network and
host-network probes. Admission, node/Pod UIDs, image identity, request immutability
and full row coverage are checked before evidence is accepted. No BMC credential
or kubeconfig is sent to a probe. Pods run as non-root with a read-only root
filesystem, dropped capabilities and a bounded memory-backed scratch volume.
Each resolved IP remains a separate observation, including failed alternatives.

Cleanup checks exact UID/resourceVersion and admitted-spec ownership, preserving
foreign modifications. Receipts are journaled atomically before creates and after
UID receipts. Ambiguous creates are inspected by exact name/spec; unresolved
outcomes remain explicit leftovers. SIGKILL can leave resources; consult
`resources.json` and verify current UIDs/ownership before operator recovery.
Automatic cleanup of other runs is not implemented.

The published manifest/platform digests and local non-root tool execution have
been checked. The opt-in `dpf_probe_image` Go test tag exercises the fixed adapter
inside the existing image against local DNS/TLS/HTTP fixtures; set
`DPF_PROBE_TEST_IMAGE` to the immutable catalog reference when running that test.
It defaults to Linux amd64; set `DPF_PROBE_TEST_PLATFORM=linux/arm64` for arm64.
Local containers do not establish Kubernetes network paths or admission behavior.
Before enabling management mode in the release catalog, exercise host-network
admission, scheduling, successful log collection, cancellation and cleanup on
a controlled cluster, and qualify actual vendor resource graphs, component
versions, physical-card correlation and completeness predicates. Software fixtures
establish parser/control-flow behavior; they do not establish physical hardware
support. Discovery performs no firmware, NTP, trust enrollment or power action.
