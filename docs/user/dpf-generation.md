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
references, or access Kubernetes. Deployment is not yet available in this preview.
The [workflow diagram](dpf-workflows.md#hbn-bgp-generation) shows every step and
failure path.

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

The profile sets `provisioning.bootstrap.bmcFactoryResetPolicy: Never`, so the
generated enrollment configuration preserves existing BMC network settings.
An explicit `OnInitialization` instead makes DPF reset each newly enrolled BMC;
that clears its network configuration and needs DHCP to restore the same address.
Verify that behavior before applying enrollment resources. Generation does not
perform the reset or verify DHCP recovery.

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
contain at most 4096 such subnets. A larger pool needs a broader per-DPU subnet
or a smaller workload range.

The host address can only be configured after DPF has allocated that DPU's subnet
and its host-facing interface has been identified. The intended host route covers
the workload pool through that DPU's HBN gateway. It preserves the existing host
default route and management network. This preview generates the address
reservations; it does not configure host interfaces or routes.

## Review the output

The output directory contains:

- Management namespaces, the DPF operator chart and its rendered preview.
- DPFOperatorConfig, the tenant DPUCluster and one exact BMC discovery target per selected DPU.
- Software/flavor definitions, address pools, service interfaces, HBN service configuration and DPUDeployment.
- The verified chart archives, original source YAML and `.l8k/resolved-config.yaml`.
- An index recording the ordered stages and required deployment checks.

The physical and host-facing interface definitions select only the intended DPU
cluster and the selected DPUs' node names. HBN's resource requests are recorded
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
