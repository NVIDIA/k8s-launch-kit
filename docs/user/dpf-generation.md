<!-- SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Generate HBN-BGP deployment files

After [DPF discovery](dpf-discovery.md), generate the management bootstrap and
HBN-BGP manifests on the CLI machine:

```bash
l8k generate --target dpf --use-case hbn-bgp \
  --user-config dpf-site.yaml \
  --save-deployment-files ./hbn-bgp
```

The CLI reads the saved inventory and downloads the selected operator and HBN
charts over HTTPS. It does not log in to hosts, DPUs or switches, read credential
references, or access Kubernetes. Use the separate
[DPF deployment workflow](dpf-deployment.md) to apply the reviewed bundle.
The [workflow diagram](dpf-workflows.md#hbn-bgp-generation) shows every step and
failure path.

Do not combine generation with `--deploy`: generate and review the bundle first,
then run `l8k deploy --target dpf` with explicit `--user-config` and the generated
`--deployment-files` path.

## Prepare the inputs

Keep the inventory produced by discovery in `dpf-site.yaml`. It must identify each
selected DPU, its exact host adapter, verified BMC administrator access and PSID,
plus the management cluster identity and Kubernetes version. If the selection has
changed, rerun discovery.

Complete these sections in the saved site file:

| Input | Responsibility |
| --- | --- |
| `provisioning.bootstrap.tenant.vip` and `.interface` | Reserve the tenant API address and identify the management interface on its control-plane nodes. |
| `provisioning.bootstrap.managementAPIIP` and `.managementAPIPort` | Needed when the saved API endpoint is not a literal, reachable IP address. |
| `provisioning.software.osISO` | Supply a credential-free HTTPS URL and SHA-256 digest for the selected DPU OS ISO. |
| `provisioning.software.pldm.<PSID>` | Supply a matching firmware bundle URL and SHA-256 digest for every selected PSID. |
| `provisioning.flavor` | Supply the concrete DPUFlavor `spec` for this hardware and HBN datapath. |
| `provisioning.resources` | Optional overrides of HBN container resources. Defaults are `memory: 6Gi` and `nvidia.com/bf_sf: "4"`. |

The flavor controls device boot, NIC and datapath settings during provisioning.
Use settings appropriate to the selected hardware and software cohort. Schema
validation alone cannot establish their runtime compatibility.

Resource quantities form a flat map; this HBN chart uses the same quantities for
requests and limits. For example, to raise the memory allocation while retaining
the default SF allocation:

```yaml
provisioning:
  resources:
    memory: 8Gi
```

The profile sets `provisioning.bootstrap.bmcFactoryResetPolicy: Never`, which
preserves existing BMC network settings. The current native HBN deployment path
requires `Never`. Although the general configuration schema and generation can
accept and render `OnInitialization`, native deployment rejects that policy
before enrollment. Do not rely on DHCP recovery for this path.

Review [bootstrap and HBN defaults](../reference/configuration.md). Tenant pod and
service ranges belong to the independent DPU cluster. The default ASN allocation
fits five DPUs; a larger estate requires an appropriate base/stride override.

## Plan host workload addresses

Each DPU receives a subnet from the workload address pool. The default pool is
`10.0.120.0/22`, divided into `/29` subnets. HBN uses address index `1` in its
allocated subnet as the gateway, and Launch Kit reserves index `2` for the host.
For example, an allocation of `10.0.120.8/29` gives HBN gateway `10.0.120.9` and
reserves `10.0.120.10` for its host.

Override these indexes together when necessary:

```yaml
provisioning:
  hbn:
    workloadGatewayIndex: 1
    hostAddressIndex: 2
```

The indexes must be different usable addresses within each subnet. Generation
excludes the host address from ordinary dynamic IPAM allocation in every possible
subnet; it does not predict which DPU will receive which subnet. The pool may
contain between 2 and 4096 such subnets. The per-DPU prefix must be more
specific than the pool prefix, so the workload-pool route differs from the
host's connected subnet. A larger pool needs a broader per-DPU subnet or a
smaller workload range.

The host address can only be configured after DPF has allocated that DPU's subnet
and its host-facing interface has been identified. The intended host route covers
the workload pool through that DPU's HBN gateway. It preserves the existing host
default route and management network. Generation prepares the address
reservations; the deploy command configures the host only after observing its
actual DPF allocation and verifying the selected interface.

The profile also carries MTUs through the complete datapath. The DPF high-speed
network and physical service-chain ports use the uplink MTU; the host-facing
service-chain port and host workload interface use the workload MTU. These
values do not change the independent control-plane MTU.

## Review the output

The output directory contains:

- Management namespaces, the DPF operator chart and its rendered preview.
- DPFOperatorConfig, the tenant DPUCluster and one exact BMC discovery target per selected DPU.
- Software/flavor definitions, address pools, service interfaces, HBN service configuration and DPUDeployment.
- The verified chart archives, original source YAML and `.l8k/resolved-config.yaml`.
- An index recording the ordered stages and required deployment checks.

The physical and host-facing interface definitions carry selectors derived from
the intended DPU cluster and selected DPUs' node names. Generation does not check
whether live labels uniquely identify those targets. HBN's resource requests are recorded
both for its container and for DPF's check that the services fit the chosen DPU
flavor. Generation verifies the manifest shapes; actual available resources and
interface readiness still require deployment checks.
Regenerate older preview bundles that omit these selectors or DPF resource
requirements; editing their manifests invalidates the retained deployment plan.

The resolved configuration records the effective defaults and overrides,
including the operator release name and namespace, each selected DPU's BMC
address, the BMC reset policy, the release-selected tenant Kubernetes version,
and the selected OS and firmware URLs and checksums. The management cluster and
DPU cluster have separate Kubernetes versions; generation does not upgrade the
management cluster. Keep it with the manifests so later steps can verify the selected
devices and software without resolving the inputs again. To change the plan,
edit the site configuration and generate a new directory; do not edit files
inside an existing bundle.

Generation validates the full bundle before publishing the directory. Repeating
an identical generation leaves that directory unchanged. Use a new destination
for changed inputs; generation does not replace a different or incomplete bundle.

A successful result means the artifacts are ready for review. Discovery blockers
remain unresolved until the environment is corrected and checked again. BGP
adjacency, host traffic, tenant health and consumed artifact versions need live
validation after deployment; they cannot be established by rendering YAML.
