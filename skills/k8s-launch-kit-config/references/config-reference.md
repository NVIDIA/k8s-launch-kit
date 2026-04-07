# l8k-config.yaml Full Annotated Reference

This is the complete l8k configuration file with every field annotated.
Comments describe the type, default value, and effect on generated manifests.

```yaml
# ============================================================================
# Network Operator Settings
# Controls the NVIDIA Network Operator Helm chart installation.
# ============================================================================
networkOperator:
  # string | default: "v26.1.0"
  # Helm chart version. Determines which operator image and CRDs are deployed.
  version: v26.1.0

  # string | default: "network-operator-v26.1.0"
  # Component image tag used for operator container images.
  componentVersion: network-operator-v26.1.0

  # string | default: "nvcr.io/nvidia/mellanox"
  # Container registry for all operator component images.
  # Change this if using a private mirror or air-gapped registry.
  repository: nvcr.io/nvidia/mellanox

  # string | default: "nvidia-network-operator"
  # Kubernetes namespace where the operator and all its components are deployed.
  namespace: nvidia-network-operator

# string | default: "default"
# Namespace where workload pods run. NetworkAttachmentDefinitions are created here
# so that pods in this namespace can reference secondary networks.
podNamespace: default

# ============================================================================
# DOCA / OFED Driver
# Controls the OFED driver DaemonSet in the NicClusterPolicy.
# ============================================================================
docaDriver:
  # bool | default: true
  # When true, the NicClusterPolicy includes an ofedDriver section that deploys
  # OFED/DOCA driver pods on every matching node.
  enable: true

  # string | default: "doca3.3.0-26.01-1.0.0.0-0"
  # DOCA driver container image version. Must match the operator version.
  version: doca3.3.0-26.01-1.0.0.0-0

  # bool | default: true
  # Unload in-tree storage kernel modules (e.g., ib_isert, ib_srpt) before
  # loading OFED modules. Prevents module conflicts on storage nodes.
  unloadStorageModules: true

  # bool | default: false
  # Enable NFS over RDMA kernel module support in the OFED driver.
  enableNFSRDMA: false

  # bool | default: true
  # When true, adds UNLOAD_THIRD_PARTY_RDMA_MODULES env var to the ofedDriver container.
  # The list is populated from thirdPartyRDMAModules discovered per group.
  # These modules are blacklisted and unloaded before OFED driver reload.
  unloadThirdPartyRDMAModules: true

# ============================================================================
# NV-IPAM (IP Address Management)
# Controls IP allocation for secondary networks.
# ============================================================================
nvIpam:
  # string | default: "nv-ipam-pool"
  # Name of the IPPool custom resource created in the operator namespace.
  poolName: nv-ipam-pool

  # --- Auto-generation mode (used when subnets list is empty) ---

  # string | default: "192.168.0.0"
  # Base network address for auto-generated subnets.
  # Each group/rail gets a unique subnet slice starting from this address.
  startingSubnet: "192.168.0.0"

  # int | default: 22
  # CIDR mask length for each auto-generated subnet.
  # /22 = 1022 usable addresses per subnet.
  mask: 22

  # int | default: 1
  # Gateway offset from the network address.
  # offset=1 means gateway = network_address + 1 (e.g., 192.168.0.1).
  offset: 1

  # --- Manual mode (takes precedence if non-empty) ---

  # list | default: [] (empty, triggers auto-generation)
  # Explicit subnet definitions. Provide one entry per rail across all groups.
  # subnets:
  #   - subnet: 192.168.2.0/24     # CIDR notation
  #     gateway: 192.168.2.1       # Gateway IP within the subnet
  #   - subnet: 192.168.3.0/24
  #     gateway: 192.168.3.1

# ============================================================================
# SR-IOV Configuration
# Controls SR-IOV device plugin, network node policies, and network CRs.
# ============================================================================
sriov:
  # int | default: 9000
  # MTU for Ethernet SR-IOV virtual functions.
  # 9000 = jumbo frames (recommended for GPU workloads).
  ethernetMtu: 9000

  # int | default: 4000
  # MTU for InfiniBand SR-IOV virtual functions.
  infinibandMtu: 4000

  # int | default: 8
  # Number of VFs to create per physical function.
  # Must not exceed hardware limit (check totalvfs via SriovNetworkNodeState).
  numVfs: 8

  # int | default: 90
  # Priority of the SriovNetworkNodePolicy (higher = applied later).
  priority: 90

  # string | default: "sriov_resource"
  # Kubernetes extended resource name registered by the device plugin.
  # Pods request this resource: resources.limits["nvidia.com/sriov_resource"]
  # With multirail, suffixed per rail: sriov_resource_rail_0, sriov_resource_rail_1
  resourceName: sriov_resource

  # string | default: "sriov-network"
  # Name of the SriovNetwork CR (and resulting NetworkAttachmentDefinition).
  # Pods reference this in their network annotation.
  networkName: sriov-network

# ============================================================================
# Host Device Configuration
# Controls host device network plugin and network CRs.
# ============================================================================
hostdev:
  # string | default: "hostdev_resource"
  # Device plugin resource name for host device mode.
  resourceName: hostdev_resource

  # string | default: "hostdev-network"
  # HostDeviceNetwork CR name.
  networkName: hostdev-network

# ============================================================================
# RDMA Shared Device Plugin
# Controls shared RDMA device plugin configuration.
# ============================================================================
rdmaShared:
  # string | default: "rdma_shared_resource"
  # Device plugin resource name. With multirail, suffixed per rail:
  # rdma_shared_resource_rail_0, rdma_shared_resource_rail_1, etc.
  resourceName: rdma_shared_resource

  # int | default: 63
  # Maximum number of pods that can share a single HCA (Host Channel Adapter).
  hcaMax: 63

# ============================================================================
# IPoIB Network (InfiniBand IP-over-InfiniBand)
# ============================================================================
ipoib:
  # string | default: "ipoib-network"
  # IPoIBNetwork CR name. With multirail: ipoib-network-rail-0, etc.
  networkName: ipoib-network

# ============================================================================
# Macvlan Network
# ============================================================================
macvlan:
  # string | default: "macvlan-network"
  # MacvlanNetwork CR name. With multirail: macvlan-network-rail-0, etc.
  networkName: macvlan-network

# ============================================================================
# NIC Configuration Operator
# Controls NIC interface renaming via NicConfigurationTemplate CRs.
# ============================================================================
nicConfigurationOperator:
  # bool | default: true
  # Enable NIC interface name templates. Only takes effect when:
  # 1. Merged groups have cross-rail PCI address conflicts, OR
  # 2. Deployment is rdma_shared and PFs have empty NetworkInterface fields.
  deployNicInterfaceNameTemplate: true

  # string | default: "rdma_r%rail_id%"
  # Naming pattern for RDMA devices. %rail_id% is replaced with rail number.
  rdmaPrefix: "rdma_r%rail_id%"

  # string | default: "eth_r%rail_id%"
  # Naming pattern for network devices. %rail_id% is replaced with rail number.
  netdevPrefix: "eth_r%rail_id%"

# ============================================================================
# Spectrum-X NIC Settings
# Used only when profile.spectrumX.enable is true.
# ============================================================================
spectrumX:
  # string | default: "1023"
  # PCI device ID of the NIC. "1023" = ConnectX-8, "a2dc" = BlueField-3 SuperNIC.
  nicType: "1023"

  # string | default: "none"
  # Overlay mode for Spectrum-X fabric.
  overlay: "none"

  # string | default: "roce_p%plane_id%_r%rail_id%"
  # RDMA device naming pattern. %plane_id% and %rail_id% are replaced.
  rdmaPrefix: "roce_p%plane_id%_r%rail_id%"

  # string | default: "eth_p%plane_id%_r%rail_id%"
  # Network device naming pattern. %plane_id% and %rail_id% are replaced.
  netdevPrefix: "eth_p%plane_id%_r%rail_id%"

# ============================================================================
# Profile Selection
# Determines which manifest templates are rendered.
# ============================================================================
profile:
  # string | default: "ethernet"
  # Fabric type: "ethernet" or "infiniband".
  # Selects Ethernet-based or InfiniBand-based profile templates.
  fabric: ethernet

  # string | default: "sriov"
  # Deployment type: "sriov", "rdma_shared", "host_device".
  # Determines how NICs are exposed to pods.
  deployment: sriov

  # bool | default: false
  # Enable multirail mode. When true, generates per-rail resources
  # (one resource/network per physical function).
  multirail: false

  spectrumX:
    # bool | default: false
    # Enable Spectrum-X deployment profile. CLI flag --spectrum-x overrides this.
    enable: false

    # string | default: "RA2.1"
    # Spectrum-X reference architecture version. CLI: --spcx-version
    spcxVersion: "RA2.1"

    # string | default: "swplb"
    # Multiplane mode: "swplb" (software PLB), "hwplb" (hardware PLB), "uniplane".
    # CLI: --multiplane-mode
    multiplaneMode: swplb

    # int | default: 4
    # Number of network planes. Also used as pfsPerNic for Spectrum-X.
    # CLI: --number-of-planes
    numberOfPlanes: 4

  # bool | default: false
  # Enable AI-optimized settings in generated manifests.
  ai: false

# ============================================================================
# Cluster Configuration
# Array of hardware groups. Typically populated by --discover-cluster-config.
# ============================================================================
clusterConfig:
  - # string — Group identifier (auto-generated by discovery as "group-0", etc.)
    identifier: ""

    capabilities:
      nodes:
        # bool — Nodes have SR-IOV capable NICs
        sriov: true
        # bool — Nodes have RDMA capable NICs
        rdma: true
        # bool — Nodes have InfiniBand capable NICs
        ib: true

    # list[string] — Node names belonging to this hardware group
    workerNodes: ["worker-0", "worker-1", "worker-2"]

    # list[PFConfig] — Physical functions detected on nodes in this group
    pfs:
      - deviceID: 1023          # PCI device ID
        pciAddress: 0000:05:00.0  # PCI bus address
        rdmaDevice: "mlx5_0"    # RDMA device name (single-node groups only)
        networkInterface: "net1" # Network interface name (single-node groups only)
        traffic: east-west       # "east-west" (fabric) or "north-south" (DPU)
        rail: 0                  # Sequential rail number (east-west PFs only)

    # map[string]string — Labels that uniquely select nodes in this group
    nodeSelector:
      feature.node.kubernetes.io/pci-15b3.present: "true"

    # list[string] — Kernel modules to blacklist for OFED driver loading.
    # Discovered by execing into nic-configuration-daemon pods.
    # thirdPartyRDMAModules:
    #   - nv_peer_mem
    #   - nvidia_peermem
```
