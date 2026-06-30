# Glossary: NVIDIA Kubernetes Networking Terms

## Hardware

- **ConnectX-8 (CX8)**: NVIDIA network adapter (deviceID `1023`). Supports SR-IOV, RDMA, and Spectrum-X with swplb/hwplb/uniplane multiplane modes.
- **ConnectX-9 (CX9)**: NVIDIA network adapter (deviceID `1025`). Same Spectrum-X capabilities as ConnectX-8.
- **BlueField-3 (BF3)**: NVIDIA DPU/SuperNIC (deviceID `a2dc`). When used with Spectrum-X, only supports multiplane mode `none` with 1 plane.
- **PF (Physical Function)**: A physical NIC port exposed by the hardware. Each PF can be split into multiple VFs via SR-IOV.
- **VF (Virtual Function)**: A lightweight virtual NIC carved from a PF via SR-IOV. Each VF can be assigned to a pod for hardware-accelerated networking.
- **Rail**: A physical NIC index within a node. In multirail deployments, each rail gets its own set of network resources (policies, IP pools, networks). Rail numbers are sequential: rail-0, rail-1, rail-2, etc.
- **Plane**: A logical grouping of rails in Spectrum-X multiplane topologies. In swplb mode, each rail-plane combination gets its own resources. In hwplb mode, planes are managed by the switch hardware.

## Traffic Classification

- **East-west traffic**: GPU interconnect traffic between NICs (ConnectX, SuperNICs). These are the NICs used in generated manifests for SR-IOV policies, OVS bridges, and RDMA resources. Each east-west NIC is assigned a rail index.
- **North-south traffic**: Management/storage traffic handled by BlueField DPUs. These are detected during discovery but excluded from manifest generation.

## Deployment Types

- **SR-IOV (Single Root I/O Virtualization)**: Hardware-accelerated network virtualization. Each PF is split into VFs that are assigned to pods. Best for low-latency, high-throughput workloads.
- **RDMA (Remote Direct Memory Access)**: Zero-copy networking that bypasses the kernel. Enables GPU-direct data transfer. Available over Ethernet (RoCE) or InfiniBand.
- **RoCE (RDMA over Converged Ethernet)**: RDMA protocol running on Ethernet fabric. Used by Spectrum-X and SR-IOV Ethernet RDMA profiles.
- **IPoIB (IP over InfiniBand)**: IP networking over InfiniBand fabric. Used by the IPoIB RDMA Shared profile.
- **MacVLAN**: Layer 2 network isolation using MAC-based virtual LANs. Used by the MacVLAN RDMA Shared profile for multi-tenant environments.
- **Host Device**: Direct PCI passthrough of the entire NIC to a pod. No virtualization overhead but only one pod per NIC.

## Spectrum-X

- **Multiplane mode**: How Spectrum-X organizes switch fabric planes. Options: `none` (BF3 only), `swplb` (software plane load balancing), `hwplb` (hardware plane load balancing), `uniplane` (single logical plane).
- **SWPLB (Software Plane Load Balancing)**: Each rail-plane combination gets its own set of Kubernetes resources. Default for CX8.
- **HWPLB (Hardware Plane Load Balancing)**: Planes managed by switch hardware. Resources generated per-rail only. For larger-scale 2/3-tier topologies.
- **Uniplane**: Single logical plane, no plane separation. Simplest topology.

## Network Operator Components

- **NicClusterPolicy**: The top-level CRD that configures the NVIDIA Network Operator. Controls which components are deployed (OFED driver, SR-IOV device plugin, RDMA shared device plugin, secondary network, NV-IPAM, etc.).
- **SriovNetworkNodePolicy**: Configures which PFs to virtualize and how many VFs to create per node.
- **SriovNetwork / MacvlanNetwork / HostDeviceNetwork**: CRDs that create NetworkAttachmentDefinitions, which pods reference via annotations to request network interfaces.
- **NV-IPAM**: NVIDIA IP Address Management. Allocates IP addresses to pods from configured subnet pools.
- **NicInterfaceNameTemplate**: Renames NIC interfaces to predictable, rail-based names (e.g., `eth_r0`, `eth_r1`) when PCI addresses alone cannot identify rails.

## DOCA/OFED

- **OFED (OpenFabrics Enterprise Distribution)**: NVIDIA's network driver stack for Mellanox NICs. Replaces inbox kernel drivers with optimized versions.
- **DOCA**: NVIDIA's Data Center Infrastructure-on-a-Chip Architecture. The DOCA driver is the containerized OFED driver deployed by the Network Operator.
- **Third-party RDMA module handling**: Before loading DOCA/OFED drivers, kernel modules that depend on inbox MLX modules (e.g., `iw_cm`, `nfsrdma`) must be unloaded. Discovery detects these automatically (including transitive dependencies) and saves them as `thirdPartyRDMAModules` per group for visibility and warnings. `unloadThirdPartyRDMAModules: true` renders `UNLOAD_THIRD_PARTY_RDMA_MODULES: "true"` (a boolean flag) in generated manifests; the driver container has the 24 known third-party modules hardcoded.

## Maintenance

- **NodeMaintenance**: A request for the Maintenance Operator to make one node safe for disruptive work and return it to service afterward.
- **Requestor mode**: Network Operator 26.1+ mode where OFED and SR-IOV ask the Maintenance Operator to coordinate drains instead of draining directly. SR-IOV requires both its external drainer and the Network Operator drain requestor.
- **MaintenanceOperatorConfig**: Cluster policy that limits global active operations and unavailable nodes. Nodes already cordoned or NotReady consume the unavailable-node budget.
- **Internal drainer**: The legacy SR-IOV drain controller used before Network Operator 26.1; its concurrency comes from `SriovNetworkPoolConfig.spec.maxUnavailable` rather than the global Maintenance Operator budget.

## l8k Concepts

- **Group**: A set of worker nodes with identical NIC hardware layouts (same device IDs, same rail count). Discovery creates groups automatically. Multiple groups with compatible hardware may be merged.
- **Group merging**: Combining multiple node groups with the same GPU product type and east-west rail count into a single group, simplifying manifest generation.
- **Non-disruptive discovery**: l8k patches the existing NicClusterPolicy via server-side apply (field owner `l8k-discovery`) instead of deleting/recreating it, avoiding disruption to running workloads.
- **Profile**: A predefined set of templates that generates the correct Kubernetes manifests for a specific networking configuration (e.g., SR-IOV Ethernet RDMA, Spectrum-X Multi-Rail).
