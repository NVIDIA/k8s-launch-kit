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

# []string | default: ["default"]
# Namespaces the secondary-network CRs (SriovNetwork, SriovIBNetwork,
# HostDeviceNetwork, MacvlanNetwork, IPoIBNetwork) and their example test
# DaemonSets are rendered into — one independent copy per namespace, each with
# its NetworkAttachmentDefinitions created there so pods in that namespace can
# reference the secondary networks. Shared resources (IPPool, NicNodePolicy,
# SriovNetworkNodePolicy, NicClusterPolicy, ...) are NOT duplicated. With more
# than one namespace, the per-namespace CR copies get a "-<namespace>" name
# suffix so they don't collide.
networkNamespaces: ["default"]

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

  # bool | default: false, auto-enabled by discovery when storage modules are found
  # Unload known storage-over-RDMA kernel modules (ib_isert, nvme_rdma, nvmet_rdma,
  # rpcrdma, xprtrdma, ib_srpt) before loading OFED modules.
  # Discovery automatically sets this to true when storage modules are detected.
  unloadStorageModules: true

  # bool | default: false
  # Enable NFS over RDMA kernel module support in the OFED driver.
  enableNFSRDMA: false

  # bool | default: false, auto-enabled by discovery when third-party RDMA modules are found
  # When true, sets UNLOAD_THIRD_PARTY_RDMA_MODULES env var to "true" in the ofedDriver container.
  # Third-party RDMA modules are blacklisted and unloaded before OFED driver reload.
  # Discovery automatically sets this to true when third-party RDMA modules are detected.
  unloadThirdPartyRDMAModules: true

# ============================================================================
# Maintenance and Upgrade Concurrency
# Controls Maintenance Operator request scheduling and legacy drain limits.
# ============================================================================
maintenance:
  # int-or-percent | default: 4
  # Global Maintenance Operator work limit. Use a positive integer or a
  # percentage string from "1%" through "100%". Integer 0 is rejected because
  # the current scheduler calculates zero available slots. Percentages use all
  # cluster nodes and round up.
  maxParallelOperations: 4

  # int-or-percent | default: 4
  # Global unavailable-node limit under requestor mode (Network Operator 26.1+).
  # Before 26.1, this is wired to SriovNetworkPoolConfig for SR-IOV's internal
  # drainer. A non-negative integer or "1%"-"100%" is accepted. Integer 0
  # pauses new work. In requestor mode, already cordoned or NotReady nodes
  # consume this budget. Requestor-mode percentages use all cluster nodes and
  # round down; legacy SR-IOV percentages use the selected pool and round down.
  maxUnavailable: 4

  # int | default: 3600
  # Non-negative delay before a Ready NodeMaintenance request can be garbage
  # collected. 0 makes it immediately eligible; this is not an operation timeout.
  # Keep it below any cluster-autoscaler idle interval.
  maxNodeMaintenanceTimeSeconds: 3600

  # int | default: 4
  # Non-negative OFED upgrade limit used only before Network Operator 26.1.
  # 0 means unlimited on the legacy path. Requestor mode ignores this field.
  maxParallelUpgrades: 4

# Network Operator 26.1+ enables OFED and SR-IOV requestors in generated Helm
# values. SR-IOV needs both the Network Operator drain requestor and the SR-IOV
# external drainer. Applying CRs alone cannot enable these Deployment settings;
# upgrade an existing Helm release with --overwrite-existing.

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

  # --- IP exclusions (optional) ---

  # int | default: 0
  # Reserve the first N host addresses of EVERY subnet (network address upward),
  # applied to both auto-generated and manual subnets. Mask-agnostic.
  # Renders into the IPPool spec.exclusions[]. e.g. 10 → .0–.9 on a /24.
  # reserveFirstIPs: 10

  # int | default: 0
  # Reserve the last N host addresses of EVERY subnet (broadcast downward).
  # e.g. 6 → .250–.255 on a /24. The gateway is NOT auto-excluded — it is
  # covered by the low reserve block.
  # reserveLastIPs: 6

  # --- Manual mode (takes precedence if non-empty) ---

  # list | default: [] (empty, triggers auto-generation)
  # Explicit subnet definitions. Provide one entry per rail across all groups.
  # Each subnet may carry its own `exclusions` (startIP/endIP) ranges; the
  # computed reserveFirstIPs/reserveLastIPs ranges are prepended to them.
  # subnets:
  #   - subnet: 192.168.2.0/24     # CIDR notation
  #     gateway: 192.168.2.1       # Gateway IP within the subnet
  #     exclusions:                # optional, merged on top of the reserve pattern
  #       - {startIP: 192.168.2.2, endIP: 192.168.2.3}
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
  # string | default: "none"
  # Overlay mode for Spectrum-X fabric.
  overlay: "none"

  # Prefix block selected for multiplaneMode none.
  singlePlane:
    netdevPrefix: "eth_r%rail_id%"
    rdmaPrefix: "roce_r%rail_id%"

  # Prefix block selected for multiplaneMode hwplb.
  hwplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%"

  # Prefix block selected for multiplaneMode swplb.
  swplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%_p%plane_id%"

# Generated Spectrum-X NicConfigurationTemplate resources are per source group
# and derive spec.nicSelector.nicType plus pciAddresses from selected east-west
# PFs. North-south PFs are ignored, including same-device-ID DPUs.

# ============================================================================
# Profile Selection
# Determines which manifest templates are rendered.
# `l8k discover` fills missing values, preserves values already in
# --user-config, applies explicit CLI overrides, and writes the final block.
# ============================================================================
profile:
  # string | default: unanimous discovered linkType (when confirmed)
  # Fabric type: "ethernet" or "infiniband".
  # Selects Ethernet-based or InfiniBand-based profile templates.
  fabric: ethernet

  # string | default: "sriov"
  # Deployment type: "sriov", "rdma_shared", "host_device".
  # Determines how NICs are exposed to pods.
  deployment: sriov

  # bool | default: true when the key is absent
  # Enable multirail mode. When true, generates per-rail resources
  # (one resource/network per physical function). An explicit false is
  # preserved across discover/generate round trips.
  multirail: false

  spectrumX:
    # bool | default: false
    # Enable Spectrum-X deployment profile. The CLI flag --spectrum-x takes
    # the SPC-X RA version as its value (e.g. --spectrum-x RA2.2); when set,
    # both `enable: true` and `spcxVersion: <value>` are derived from the
    # same flag.
    enable: false

    # string | default: "RA2.2"
    # Spectrum-X reference architecture version. Supported: RA2.1 (Network
    # Operator 26.1 only) or RA2.2 (Network Operator 26.4+). Set via the
    # value of --spectrum-x on the CLI.
    spcxVersion: "RA2.2"

    # string | default: derived from GPU platform + east-west NIC device ID
    # Multiplane mode: "none", "swplb" (software PLB), or "hwplb"
    # (hardware PLB). When Spectrum-X is enabled and this is absent,
    # H100/H200/B200/GB200 default to none; B300/GB300 default to the
    # GA swplb path. Platform type cannot select hwplb; override explicitly.
    multiplaneMode: swplb

    # int | default: derived from GPU platform + east-west NIC device ID
    # Number of network planes (1, 2, or 4). Also used as pfsPerNic for
    # Spectrum-X. Single-plane platforms default to 1; B300/GB300 default
    # to 2. Set 4 explicitly for a quad-plane B300 topology.
    numberOfPlanes: 2

# ============================================================================
# Validation Configuration
# Controls l8k validate data-plane checks.
# ============================================================================
validation:
  gpuDirect:
    # bool | always present; discovery writes true only when every worker can
    # satisfy its render bucket's topology-derived gpuResourceType request.
    enabled: false
    # string | qualified extended resource | default: nvidia.com/gpu
    gpuResourceType: nvidia.com/gpu

  # bool | default: true
  # Run the example DaemonSet connectivity matrix.
  connectivity: true

  # string | default: strict
  # quick: same-rail all nodes + one non-gating cross-rail canary per rail pair.
  # full: every source rail x every destination rail x every ordered pod pair;
  #       cross-rail is non-gating.
  # strict: full matrix; cross-rail gates according to profile.routing.
  mode: strict

  # list[string] | default: [icmp, rping, ib_write_bw]
  # Supported checks. Use [] to disable all connectivity checks.
  checks:
    - icmp
    - rping
    - ib_write_bw

  rdma:
    # int | default: 5
    rpingIterations: 5
    # int | default: 65536
    ibWriteSize: 65536
    # float | default: 100
    # 0 disables bandwidth threshold gating.
    ibWriteMinBandwidthGbps: 100

# ============================================================================
# Cluster Configuration
# Array of hardware groups. Typically populated by --discover-cluster-config.
# ============================================================================
clusterConfig:
  - # string — Group identifier (auto-generated by discovery; max 40 bytes)
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
        model: "ConnectX-8 ..."  # VPD model string (NicDevice.Status.modelName).
                                 # Drives rail collapsing: a "2-port"/"Dual-port"
                                 # model keeps a rail per port; other multi-PF
                                 # NICs collapse to one rail (the master PF).
                                 # See `l8k discover --collapse-nic-rails`.

    # map[string]string — Labels that uniquely select nodes in this group
    nodeSelector:
      feature.node.kubernetes.io/pci-15b3.present: "true"

    # list[string] — Kernel modules to blacklist for OFED driver loading.
    # Discovered by execing into nic-configuration-daemon pods.
    # thirdPartyRDMAModules:
    #   - nv_peer_mem
    #   - nvidia_peermem
```
