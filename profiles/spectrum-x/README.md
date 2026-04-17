# Spectrum-X Multi-Rail Profile

## Overview

The Spectrum-X profile provides optimized multi-rail networking with OVS hardware offload, DOCA acceleration, and advanced NIC firmware configuration specifically designed for AI workloads.

## Features

- **Multi-Rail Networking**: Supports multiple network rails for high-bandwidth AI workloads
- **OVS Hardware Offload**: DOCA-accelerated Open vSwitch with hardware offloading
- **RDMA Exclusive Mode**: Dedicated RDMA resources per workload
- **Advanced Firmware Configuration**: Spectrum-X optimized firmware with RA2.2 support
- **Multiple Plane Modes**: Supports none, swplb, hwplb, and uniplane configurations
- **Dynamic Interface Naming**: Automatic NIC interface naming based on plane and rail topology

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

- **nicType**: NIC device type
  - `"1023"` for ConnectX-8
  - `"1025"` for ConnectX-9
  - `"a2dc"` for BlueField-3 SuperNIC
- **firmwareVersion**: Spectrum-X firmware version (e.g., `"RA2.2"`)
- **multiplaneMode**: Multiplane configuration
  - `none`: Single plane
  - `swplb`: Software plane load balancing
  - `hwplb`: Hardware plane load balancing
  - `uniplane`: Unified plane mode
- **numberOfPlanes**: Number of planes (1, 2, or 4)

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
- **rdmaPrefix**: Prefix for RDMA device names (e.g., `"roce_"`)
- **netdevPrefix**: Template for network interface names (e.g., `"nic%nic_id%_p%plane%_r%rail%"`)

### Bridge Configuration

- **bridgeGroupingPolicy**: How to group NICs into bridges
  - `perPF`: One bridge per physical function
  - `perNIC`: One bridge per NIC
  - `all`: Single bridge for all NICs

### CIDR Pool Configuration

- **cidrPoolPerNodePrefix**: Subnet prefix for per-node allocation (typically `31` for point-to-point)
- **cidrPoolGatewayIndex**: Gateway IP index in the subnet (typically `0`)
- **cidrPoolPerNodeExclusions**: IP indices to exclude from allocation

## Generated CRDs

The profile generates the following Kubernetes Custom Resources:

1. **NicClusterPolicy** (`10-nicclusterpolicy.yaml`)
   - Configures Network Operator, NicConfigurationOperator (with `nicFirmwareStorage`),
     NV-IPAM, Spectrum-X Operator with nested `xPlane` block, and secondary network CNI.

2. **NicConfigurationTemplate** (`30-nicconfigurationtemplate.yaml`)
   - Configures Spectrum-X optimized firmware settings (RA2.2).

3. **NICInterfaceNameTemplate** (`35-nicinterfacenametemplate.yaml`)
   - Defines interface naming conventions for multi-rail (one inner list per rail,
     all PCI addresses of that rail grouped together).

4. **CIDRPool** (`60-cidrpool.yaml`)
   - One pool per rail (non-swplb) or per rail-plane (swplb), with IP placeholders
     the cluster operator must fill in.

5. **SpectrumXRailPoolConfig** (`80-spectrumxrailpoolconfig.yaml`)
   - Single `v1alpha2` resource with `railTopology[]`. In swplb, one entry per
     rail-plane; otherwise one entry per rail grouping all planes.

6. **Example DaemonSet** (`90-example-daemonset.yaml`)
   - Example workload requesting one VF per rail (non-swplb) or per rail-plane (swplb).

`NicFirmwareSource` and `NicFirmwareTemplate` must be applied separately by the
operator; l8k does not generate them.

## Example Configuration

```yaml
spectrumX:
  nicType: "1023"
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
  rdmaPrefix: "roce_"
  netdevPrefix: "nic%nic_id%_p%plane%_r%rail%"
  eSwitchMultiport: "true"
  bridgeGroupingPolicy: perPF
  cidrPoolPerNodePrefix: 31
  cidrPoolGatewayIndex: 0
  cidrPoolPerNodeExclusions:
    - "1"

profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX: true
  ai: true
```

## Deployment

### Prerequisites

1. Kubernetes cluster with SR-IOV capable nodes
2. NVIDIA Network Operator v26.1.0 or later
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
  --version v26.1.0 \
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
