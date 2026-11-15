<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Deploy HBN-BGP on DPUs

Use this workflow after [discovery](dpf-discovery.md) and
[generation](dpf-generation.md). It provisions the independent DPU cluster and
connects the selected hosts through HBN and your BGP fabric. The
[deployment flow diagram](dpf-workflows.md#hbn-bgp-deployment) includes the
blocking conditions and recovery path.

Deployment can replace the selected DPUs' operating systems, change their device
settings, reboot their hosts and configure the selected host workload interfaces.
Use a maintenance window that excludes other provisioning and firmware writers
for the entire run, including external MFT commands. Launch Kit checks switch
configuration; this command does not
configure switches or create the management Kubernetes cluster or its VMs.

## Prepare the site and review the bundle

Keep the discovered site file, generated directory and its metadata together.
Provide working management-cluster access and explicit access references for the
selected hosts, DPU BMCs and switches. The machine running the CLI needs direct
connectivity to those endpoints. The management nodes also need the network paths
reported by discovery; reachability from the CLI alone is insufficient.

Every selected DPU BMC must grant Administrator privileges. A successful login
with a restricted account is insufficient. The current BF4 HBN crawler also
requires the `admin` username and the same nonempty password value on every
selected DPU. Separate credential aliases may reference the same value. HBN
discovery checks this compatibility separately from general Administrator
authorization. Use protected password/key references;
do not put passwords in manifests or command arguments. Keep SSH host-key pins
and the selected Redfish certificate policy with the site configuration.

Review these site decisions before deployment:

| Decision | What must be established |
| --- | --- |
| Selected devices | Each DPU maps to the intended physical host and PCI adapter. |
| Tenant API | The VIP is reserved, excluded from DHCP allocation, reachable on the selected control-plane interface, and its VRRP router ID is available on the selected nodes. |
| Software | The OS ISO and each PSID-specific firmware bundle belong to the selected release and hardware cohort; their supplied checksums match the intended bytes. |
| Firmware prerequisites | Required firmware is already active and the BMC provides the observations needed for safe reset coordination. Vendor recovery operations are a separate prerequisite. |
| Fabric | Selected uplinks have the intended VRF, BGP neighbors, ASN, policy and MTU. Correct configuration alone does not prove hardware forwarding. |
| Host workload network | The selected PF can be managed without taking over the management interface, default route or an existing workload. |

The tenant allocation declarations live under
`provisioning.bootstrap.tenant.allocation`: `vipReserved`,
`vipExcludedFromDHCP` and `routerIDUniqueOnSelectedNodes`. Set them only after
confirming the corresponding site allocation. A successful ARP probe does not
establish DHCP exclusion. Missing or false declarations block tenant creation.

The profile supplies release coordinates, ASNs, address pools, MTUs and other
defaultable settings. Override the site file when your environment differs, then
rediscover and regenerate. Edit the source configuration rather than files inside
an existing generated bundle.

Automatic reset coordination requires a supported BMC observation format. The
current BlueField-4 reset checks cover the 26.10 BMC format; other formats stop
with a reason when safe shutdown or idle firmware state cannot be established.
This support does not qualify every hardware and firmware combination for the
selected DPF release.

## Run deployment

```bash
l8k deploy --target dpf \
  --user-config dpf-discovered.yaml \
  --deployment-files ./hbn-bgp \
  --deploy-timeout 2h
```

Both file arguments must be explicit. The default DPF timeout is two hours;
a shorter positive timeout is supported. Use `--log-level debug` for additional
operation diagnostics. Normal live progress is written to stderr. For automation,
add `--output json`, retain stderr separately and check the process exit status.

DPF deployment supports neither `--dry-run` nor Host `--overwrite-existing`
semantics. Review the generated bundle before applying it. The DPF target does
not use the Host `--kubeconfig` flag; management access comes from the site file.

## What runs where

| Stage | Operations and location |
| --- | --- |
| Fresh checks | The CLI rereads selected host, BMC, switch and management-cluster facts, checks required permissions and compares them with the reviewed bundle. A changed identity or desired configuration stops deployment. |
| DPF management | The CLI installs the retained DPF operator chart through the management Kubernetes API and waits for the operator and its configuration. Conflicting resources are reported rather than silently adopted. |
| DPU control plane | DPF creates the tenant Kubernetes control plane on the selected management nodes. Launch Kit verifies its API and backing prerequisites and rechecks operator readiness before enrolling DPUs. |
| Enrollment and software | The CLI supplies the required BMC credential Secret and exact discovery targets. DPF contacts the DPU BMCs, enrolls the selected cards and caches the selected software. Launch Kit checks the downloaded bytes against the supplied checksums. |
| Service declarations | The CLI creates the HBN service configuration, service chains and DPUDeployment through the management API. These declarations can exist before any DPU has joined; their existence is not service readiness. |
| DPU installation | DPF transfers the OS installer through the BMC, boots it and configures the installed DPU agent. Installing the OS can take many minutes; BMC `OSRunning` can describe the installer rather than the final agent. |
| Activation | When DPF requests a supported external reset, Launch Kit checks the exact device/request and shutdown state before rebooting the selected host. It confirms the new host boot and DPU agent/kernel boot before acknowledging that reset. Unsupported or unmeasurable reset paths stop with a reason. |
| Service readiness | Once the DPUs join, DPF starts HBN and the service chains on those tenant nodes. Launch Kit waits for the intended devices, current service resources and actual HBN containers. |
| Host networking | The CLI reads each DPU's actual allocated workload subnet, then configures its associated host PF and workload-pool route over host SSH. It preserves the management network and default route. |
| Runtime checks | The CLI reads BGP and installed routes from each HBN container, then tests every directed pair of selected hosts through the workload network, including DF packets at the configured workload MTU. |

The uplink MTU applies to the DPF high-speed network and physical service-chain
ports. The workload MTU applies to the host-facing service-chain port and host PF.
The independent control-plane network retains its own MTU.

At boot, network readiness follows the selected port's existing Netplan
optionality. A required port's static workload address joins Netplan's
"any routable interface" wait. That wait can then succeed through the workload
port; Launch Kit checks management reachability separately.

## Read the result and recover from a stop

`validated` means the complete selected multi-host BGP, route and traffic matrix
passed at the time of the checks. A one-host run can be `provisioned`, but cannot
establish cross-host traffic. Neither result is a general hardware qualification,
a performance benchmark or proof that every failover path was exercised.

If deployment stops, retain the reported operation records, stdout and stderr.
The result identifies the last completed stage; the records retain individual
failure and unavailable-check reasons. Earlier successful effects can remain.
Inspect current resources and device state before another attempt, especially
after a timeout or disconnected reboot request. Do not remove DPF reset annotations
or edit resource status to bypass a pending operation.

Correct an input by editing the site file and generating a new reviewed bundle.
An existing conflicting deployment may need deliberate removal before a clean
run; deployment does not automatically delete it. Host `l8k clean` is not a DPF
cleanup command. Leave unrelated clusters, workloads and externally managed
components in place.
