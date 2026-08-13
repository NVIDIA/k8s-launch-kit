# Spectrum-X Multi-Rail Profile

## Overview

The Spectrum-X profile provides optimized multi-rail networking with OVS hardware offload, DOCA acceleration, and advanced NIC firmware configuration specifically designed for AI workloads.

## Features

- **Multi-Rail Networking**: Supports multiple network rails for high-bandwidth AI workloads
- **OVS Hardware Offload**: DOCA-accelerated Open vSwitch with hardware offloading
- **RDMA Exclusive Mode**: Dedicated RDMA resources per workload
- **Advanced Firmware Configuration**: Spectrum-X optimized firmware with RA2.2 support
- **Multiple Plane Modes**: Supports none, swplb, and hwplb configurations
- **Dynamic Interface Naming**: Automatic NIC interface naming based on plane and rail topology
- **Optional DRA Workload Allocation**: `profile.spectrumX.useDRA` enables ResourceClaimTemplate-based GPU/VF allocation

## Profile Requirements

```yaml
profileRequirements:
  fabric: ethernet
  deployment: sriov
  spectrumX: true
  multirail: true
nodeCapabilities:
  sriov: true
  rdma: true
```

## Configuration Parameters

### NIC Configuration

- **nicSelector.nicType / pciAddresses**: Derived in each per-source
  `NicConfigurationTemplate` from the selected east-west PFs. The combined
  selector excludes a north-south DPU even when it has the same device ID as
  the SuperNIC. Missing or mixed east-west device IDs and missing east-west PCI
  addresses fail generation.
- **firmwareVersion**: Spectrum-X firmware version (e.g., `"RA2.2"`)
- **multiplaneMode**: Multiplane configuration
  - `none`: Single plane
  - `swplb`: Software plane load balancing
  - `hwplb`: Hardware plane load balancing
- **numberOfPlanes**: Number of planes (1, 2, or 4)

### Spectrum-X Profile Options

- **useDRA**: `false` by default. When `true`, l8k enables the SR-IOV operator
  `dynamicResourceAllocation` feature gate, sets `SpectrumXRailPoolConfig.spec.draEnabled`
  to `true`, emits `ResourceClaimTemplate` manifests, and renders the example workload
  with DRA claims instead of device-plugin resource requests.
- Generated v1alpha2 `SpectrumXRailPoolConfig` resources omit the removed
  `spec.withBCM` field because current CRDs reject it during strict decoding.

### OVS Configuration

- **ovsBridgeDatapathType**: `netdev` (DPDK datapath)
- **ovsBridgeFailMode**: `secure` (fail secure mode)
- **ovsUplinkInterfaceType**: `dpdk` (DPDK uplink interface)
- **docaEswitchMax**: Number of OVS bridges
  - GB HW PLB: 4 (number of rails)
  - GB SW PLB: planes × 4 (number of rails)
  - Hopper: 8 (number of rails)

### RDMA Configuration

- **rdmaMode**: `exclusive` or `shared`
- **singlePlane**: User-overridable NET and RDMA prefixes for `none`
- **hwplb**: User-overridable rail-plane NET and rail-only RDMA prefixes
- **swplb**: User-overridable rail-plane NET and RDMA prefixes

### Bridge Configuration

- **bridgeGroupingPolicy**: How to group NICs into bridges
  - `perPF`: One bridge per physical function
  - `perNIC`: One bridge per NIC
  - `all`: Single bridge for all NICs

### CIDR Pool Configuration

`--topology-file`, `--topology-scheme`, and `--ip-version` drive CIDRPool
generation. IPv4 uses a `/31` per node with gateway index `0`; IPv6 uses a
`/64` per node with leaf gateway `::2`, gateway index `2`, and a `/40` pool per
rail or rail-plane. IPv6 routes are `/32` for one plane and `/24` for two or
four planes.

## Generated CRDs

The profile generates the following Kubernetes Custom Resources:

1. **NicClusterPolicy** (`10-nicclusterpolicy.yaml`)
   - Configures Network Operator, NicConfigurationOperator (with `nicFirmwareStorage`),
     NV-IPAM, Spectrum-X Operator with nested `xPlane` block, and secondary network CNI.
   - Resolves the xPlane repository and version independently from the selected
     Network Operator release catalog entry.

2. **NICInterfaceNameTemplate** (`25-nicinterfacenametemplate.yaml`)
   - Defines interface naming conventions for multi-rail (one inner list per rail,
     all PCI addresses of that rail grouped together). Renaming runs **before** the
     NIC configuration template so the firmware/optimization template can refer to
     the renamed PFs.

3. **NicConfigurationTemplate** (`30-nicconfigurationtemplate.yaml`)
   - Configures Spectrum-X optimized firmware settings (RA2.2).
   - Rendered once per source hardware group with east-west-only NIC type and
     PCI-address selectors.

4. **CIDRPool** (`60-cidrpool.yaml`)
   - One topology-derived IPv4 or IPv6 pool per rail (non-swplb) or per
     rail-plane (swplb), including routes and per-node static allocations.

5. **SpectrumXRailPoolConfig** (`80-spectrumxrailpoolconfig.yaml`)
   - Single `v1alpha2` resource with `railTopology[]`. In swplb, one entry per
     rail-plane; otherwise one entry per rail grouping all planes. `draEnabled`
     is rendered from `profile.spectrumX.useDRA`; the default is explicit `false`.

6. **ResourceClaimTemplate** (`85-resourceclaimtemplate.yaml`)
   - Rendered only when `profile.spectrumX.useDRA: true`. Each template requests
     a GPU and matching SR-IOV VF resources using DRA device classes.

7. **Example DaemonSet** (`90-example-daemonset.yaml`)
   - Example workload requesting one VF per rail (non-swplb) or per rail-plane (swplb).
     In DRA mode, it references the generated `ResourceClaimTemplate` resources instead.

`NicFirmwareSource` and `NicFirmwareTemplate` must be applied separately by the
operator; l8k does not generate them.

## Example Configuration

```yaml
spectrumX:
  firmwareVersion: "RA2.2"
  multiplaneMode: hwplb
  numberOfPlanes: 4
  overlay: "none"
  firmwareBinURLs:
    - https://example.com/fw-ConnectX8.signed.bin.zip
  firmwareBfbURLs:
    - https://example.com/bf-fwbundle-3.1.0-77_25.07-prod.bfb
  rdmaMode: exclusive
  docaEswitchMax: 4
  ovsBridgeDatapathType: netdev
  ovsBridgeFailMode: secure
  ovsUplinkInterfaceType: dpdk
  singlePlane:
    netdevPrefix: "eth_r%rail_id%"
    rdmaPrefix: "roce_r%rail_id%"
  hwplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%"
  swplb:
    netdevPrefix: "eth_r%rail_id%_p%plane_id%"
    rdmaPrefix: "roce_r%rail_id%_p%plane_id%"
  eSwitchMultiport: "true"
  bridgeGroupingPolicy: perPF

profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    multiplaneMode: hwplb
    numberOfPlanes: 4
    topologyType: 2-tier
    ipVersion: ipv6
    topologyFile: ./topology.json
    useDRA: false
```

## Deployment

### Prerequisites

1. Kubernetes cluster with SR-IOV capable nodes
2. NVIDIA Network Operator v26.4.0 or later
3. ConnectX-8, ConnectX-9, or BlueField-3 SuperNIC adapters
4. Firmware compatible with Spectrum-X RA2.2

### Installation with Helm

```bash
# Add NVIDIA Helm repository
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update

# Install Network Operator with Spectrum-X support
helm install network-operator nvidia/network-operator \
  --namespace nvidia-network-operator \
  --create-namespace \
  --version v26.4.0 \
  -f myValues.yaml \
  --wait
```

### Required Helm Values

```yaml
sriovNetworkOperator:
  enabled: true

maintenanceOperator:
  enabled: true

sriov-network-operator:
  sriovOperatorConfig:
    configDaemonNodeSelector:
      network.nvidia.com/operator.nic-configuration.wait: "false"
    featureGates:
      manageSoftwareBridges: true
    disablePlugins:
    - mellanox
```

### Apply Generated Manifests

```bash
# Generate deployment files
l8k --user-config config.yaml --save-deployment-files ./output

# Apply manifests
kubectl apply -f ./output/network-operator/
```

## Testing

Deploy the test pod to verify the configuration:

```bash
kubectl apply -f 90-pod.yaml
```

Check the pod has access to all rails:

```bash
kubectl exec -it spectrum-x-multirail-test-pod -- sh -c "ip addr show && rdma link"
```

## Multi-Rail Topology Examples

### 4-Rail Configuration (ConnectX-8 or ConnectX-9, Quad Plane)

```yaml
clusterConfig:
  pfs:
    - pciAddress: "0000:1a:00.0"
      networkInterface: "nic1_p1_r1"
      traffic: east-west
    - pciAddress: "0000:1a:00.1"
      networkInterface: "nic1_p2_r1"
      traffic: east-west
    - pciAddress: "0000:2a:00.0"
      networkInterface: "nic2_p3_r1"
      traffic: east-west
    - pciAddress: "0000:2a:00.1"
      networkInterface: "nic2_p4_r1"
      traffic: east-west
```

### 4-Rail Configuration (Single NIC, 4 PFs)

```yaml
clusterConfig:
  pfs:
    - pciAddress: "0000:1a:00.0"
      networkInterface: "eth_p1_r1"
      traffic: east-west
    - pciAddress: "0000:1a:00.1"
      networkInterface: "eth_p2_r1"
      traffic: east-west
    - pciAddress: "0000:1a:00.2"
      networkInterface: "eth_p3_r1"
      traffic: east-west
    - pciAddress: "0000:1a:00.3"
      networkInterface: "eth_p4_r1"
      traffic: east-west
```

## Notes

- **RDMA Exclusive Mode**: Requires node reboot and cannot be set when namespaces are configured (tech-preview feature for non-BCM workflows)
- **CIDRPool Size Limits**: Each CIDRPool CRD has a 1.5MB etcd limit
  - With `kubectl apply`: ~6,424 nodes per pool
  - With `kubectl apply --server-side`: ~10,105 nodes per pool (recommended)
- **Firmware Updates**: Use NicFirmwareTemplate with `updatePolicy: Update` for automatic firmware updates

## Troubleshooting

### Check Network Operator Status

```bash
kubectl get pods -n nvidia-network-operator
kubectl logs -n nvidia-network-operator -l app=sriov-network-operator
```

### Verify SR-IOV Configuration

```bash
kubectl get sriovnetworknodepolicy -n nvidia-network-operator
kubectl get sriovnetwork -n nvidia-network-operator
```

### Check OVS Bridge Status

```bash
# On worker nodes
ovs-vsctl show
ovs-appctl dpif/show
```

### Verify RDMA Devices

```bash
# On worker nodes or in pods
rdma link
ibv_devices
```

## References

- [NVIDIA Network Operator Documentation](https://docs.nvidia.com/networking/display/COKAN10)
- [Spectrum-X Architecture Guide](https://docs.nvidia.com/networking/display/SpectrumX)
- [NIC Configuration Operator](https://github.com/Mellanox/nic-configuration-operator)
