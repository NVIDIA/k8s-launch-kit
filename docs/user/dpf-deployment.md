<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Deploy HBN-BGP on DPUs

Use this procedure after [DPF discovery](dpf-discovery.md) and review of the
[generated HBN-BGP bundle](dpf-generation.md). Launch Kit uses an existing
management Kubernetes cluster and its required DPF foundations to provision the
selected DPUs, create their independent tenant control plane, configure each
selected host's workload interface and check HBN routing and traffic. The
[deployment workflow](dpf-workflows.md#hbn-bgp-deployment) gives the step-by-step
operations and decision paths.

The management Kubernetes cluster, required foundations and aligned switch
fabric must already exist. Launch Kit does not create the management VM or
configure switches. Deployment can replace selected DPU operating systems,
change DPU settings, reboot selected hosts and configure their workload-facing
interfaces. Use a maintenance window that excludes other device provisioning,
firmware writers and host maintenance. Keep the CLI machine online and able to
reach the management cluster and selected hosts during the run; host reboots can
interrupt access temporarily.

## Management-cluster prerequisites

Before starting, install versions compatible with the selected DPF release of
cert-manager, Argo CD, the Node Feature Discovery (NFD) API and master, NVIDIA
Maintenance Operator, and Kamaji. Kamaji needs a healthy shared etcd DataStore
and the supported local-path storage configuration. See the
[management and storage prerequisites](dpf-discovery.md#check-hbn-bgp-prerequisites).
Launch Kit checks these components and their access; this deployment command
does not install them.

## Prepare the site and review the bundle

Keep the discovered site file, generated directory and its metadata together.
Provide management-cluster access and explicit, trusted access to the selected
hosts, DPU BMCs and switches. The CLI machine needs direct connectivity to these
endpoints. The management nodes also need the paths reported by discovery; CLI
reachability alone is insufficient. Each selected host must be able to boot its
existing local operating system after a system reboot. Host firmware setup is
outside this procedure.

For the current BF4 enrollment path, each selected DPU BMC must grant
Administrator access, use the `admin` username and resolve to the same nonempty
password value. Separate protected credential aliases may reference that value.
Use protected password/key references and pinned SSH host keys; do not put
secrets in command arguments or generated manifests.

Confirm these site decisions before generating the bundle:

| Decision | What to establish |
| --- | --- |
| Selected devices | Each DPU maps to the intended physical host and adapter. |
| Tenant API | Reserve the VIP, exclude it from DHCP and confirm its VRID is available on the selected control-plane nodes. Declare these facts under `provisioning.bootstrap.tenant.allocation` only with the responsible owner's confirmation. |
| Software | The OS ISO and each selected PSID firmware bundle match the intended DPU cohort; provide credential-free HTTPS URLs and checksums. |
| Fabric | Switch uplinks already have the intended port bindings, fabric VRF, BGP neighbors/policy, ASN and MTU. Deployment observes this configuration; it does not apply it. |
| Host workload network | The selected PF can be configured without taking over the host management interface, another port or its default route. |
| Host recovery | The selected host can return to its existing local operating system after a requested system reboot. |

The native HBN deployment path requires
`provisioning.bootstrap.bmcFactoryResetPolicy: Never`, preserving existing BMC
network settings. A setting accepted by the general configuration schema does
not imply that the native deployment path supports it; see
[configuration reference](../reference/configuration.md#bootstrap-and-hbn-defaults).

`dpf.hbn.fabricVRF` describes the VRF expected on the selected switch uplinks;
the profile default is `default`. DPU FRR uses its default VRF. These are separate
routing roles, so review the switch fabric and DPU routing settings independently
in the resolved bundle. The profile provides defaultable release, ASN,
address-pool and MTU values. Override the site file only when needed, then
regenerate and review a new bundle. Do not edit files in an existing generated
directory.

For complete field names and profile values, see
[HBN-BGP generation](dpf-generation.md) and the
[bootstrap and HBN configuration reference](../reference/configuration.md#bootstrap-and-hbn-defaults).

## Run deployment

```bash
l8k deploy --target dpf \
  --user-config dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --deploy-timeout 2h
```

Both file arguments are required explicitly. The default and maximum DPF timeout
is two hours; a shorter positive timeout is supported. For automation, add
`--output json`, capture stdout separately from stderr and check the process exit
status. Keep the report path and partial effects from a failed run for inspection.

The DPF target does not support `--dry-run` or Host `--overwrite-existing`
semantics. It does not use Host `--kubeconfig`; management access comes from the
site file. This is an initial deployment procedure, not a general upgrade or
adoption path.

## What runs where

| Operation | Where it runs | What must be observed before continuing |
| --- | --- | --- |
| Refresh deployment prerequisites | The CLI reads selected hosts, BMCs, switches and the management API. | Required identities, access and desired settings still match the reviewed bundle; required unknown or failed facts stop deployment. |
| Install DPF management | The CLI uses the management Kubernetes API and retained chart; DPF controllers run on the management cluster. | Operator and configuration are ready with intended ownership. Conflicts are reported rather than silently adopted. |
| Create the DPU control plane | DPF and its controllers create the selected tenant cluster on the management nodes; the CLI checks through management and tenant APIs. | The tenant API and backing prerequisites are available and match the plan. This does not mean selected DPUs are installed. |
| Enroll selected cards | The CLI supplies the required credential and discovery targets; DPF contacts the selected BMCs. | Each selected DPU is enrolled with the intended device/host identity. The reviewed deployment selectors target those devices later; no manual label operation is required. |
| Stage software and declare HBN | DPF caches the selected OS and PSID firmware; the CLI verifies artifact checksums and creates the selected flavor, IPAM, interface, service and DPUDeployment declarations through the management API. | Artifact bytes match the reviewed bundle and the declarations target only the selected devices and are accepted. Downloads and accepted objects do not prove installed software or a running service. |
| Install and activate DPUs | DPF controllers install the OS and DPU agent. When DPF requests host maintenance, the CLI performs only a supported system reboot over the selected host access after checking the request and safe shutdown state. | The host and selected DPU return with the expected boot observations within the configured deployment timeout, and DPF reports them ready. |
| Configure host workload networking | The CLI reads each actual DPU IPAM allocation through the tenant/management APIs and configures the matching host PF over host access. | The allocated address, selected interface, MTU and workload route are applied and verified while preserving management connectivity and the default route. |
| Check HBN routing and traffic | The CLI reads the intended HBN containers and routing state through the tenant API, then probes selected hosts over host access. | Required BGP peers, remote workload routes and every directed ordinary and do-not-fragment (DF) traffic check pass. A one-host result cannot establish cross-host traffic. |

The table shows operation dependencies and Launch Kit checkpoints; DPF
controllers reconcile resources asynchronously, so HBN startup can begin after
DPU agents join while deployment is still progressing. Launch Kit waits for the
selected service and interface resources and performs final HBN acceptance only
after host workload networking. Do not treat a declared service, ready artifact
cache or DPU provisioning result as proof of BGP or traffic.

## Read the result and recover from a stop

`validated` means the selected multi-host BGP, installed-route and directed
traffic checks passed at the time of observation. A one-host run can be
`provisioned`, but cross-host routing and traffic remain unmeasured. Neither
state is a general hardware qualification, performance result or proof that
every failover path was exercised.

If deployment stops, retain the discovery handoff, reported operation records,
stdout and stderr. A stop can leave earlier management, tenant, enrollment,
software or host-network effects in place. Inspect current resources and device
state before another attempt, especially after timeout or interrupted host
reboot. Do not retry a host reset when the report does not establish whether it occurred.

A matching tenant or resource name does not prove it is safe to reuse. Inspect
ownership, active workloads and network state. Coordinate any selected-tenant
cleanup with its owner and preserve unrelated resources. If desired input or
selection changes, update the site file, repeat discovery when observations
changed and generate a newly reviewed bundle. Host `l8k clean` is not DPF
teardown.
