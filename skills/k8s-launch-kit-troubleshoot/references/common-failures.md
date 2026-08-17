# Common Failure Patterns and Remediation

This document covers the 10 most frequently encountered NVIDIA Network Operator issues,
with detailed remediation steps for each.

---

## 1. OFED Driver CrashLoopBackOff

**Symptom**: OFED/DOCA driver pods in `CrashLoopBackOff` state. Logs show module loading
failures or dependency errors.

**Root cause**: Kernel modules that depend on MLX/OFED drivers (e.g., `iw_cm`, `rdma_cm`,
`nv_peer_mem`) are loaded and prevent the OFED driver from replacing the in-tree modules.

**Remediation**:
1. Check `thirdPartyRDMAModules` in your cluster config -- these modules must be unloaded.
2. Set `docaDriver.unloadThirdPartyRDMAModules: true` in your l8k config.
3. Re-run discovery (`--discover-cluster-config`) to refresh the dependent modules list.
4. Redeploy. The generated NicClusterPolicy will include `UNLOAD_THIRD_PARTY_RDMA_MODULES: "true"`
   (a boolean flag) in the ofedDriver section.
5. If the issue persists, exec into the OFED pod and check `/sys/module/*/holders/` to
   find additional unlisted dependents.

---

## 2. SR-IOV Config Daemon Not Running

**Symptom**: No `sriov-network-config-daemon` pods running. VFs are not being created.

**Root cause**: The NicClusterPolicy does not include SR-IOV device plugin and node policy
sections, so the operator does not deploy the SR-IOV components.

**Remediation**:
1. Verify your NicClusterPolicy has `sriovDevicePlugin` and `sriovNetworkNodePolicy`
   sections: `kubectl get nicclusterpolicy -o yaml`
2. If using l8k, ensure your profile includes SR-IOV (`--deployment-type sriov`).
3. Check that the SriovOperatorConfig exists:
   `kubectl get sriovoperatorconfigs -n nvidia-network-operator`
4. Verify the operator pod is running and has no errors in its logs.

---

## 3. NicClusterPolicy Stuck notReady

**Symptom**: `kubectl get nicclusterpolicy` shows the policy exists but conditions
indicate it is not ready. Components may be partially deployed.

**Remediation**:
1. Inspect the policy conditions:
   `kubectl get nicclusterpolicy -o yaml` -- look at `.status.conditions`
2. Read the network-operator pod logs for reconciliation errors:
   `kubectl logs -n nvidia-network-operator -l app=network-operator --tail=200`
3. Common causes: image pull failures (check image repository and tag), RBAC issues
   (check operator ServiceAccount permissions), CRD version mismatches.
4. If a specific component is failing, check that component's pods under the operator
   namespace for more details.

---

## 4. VF Creation Failures

**Symptom**: SR-IOV config daemon runs but VFs are not created. `SriovNetworkNodeState`
shows sync errors or zero VFs.

**Root cause**: The node's NIC does not support SR-IOV, or `numVfs` exceeds the hardware
maximum.

**Remediation**:
1. Check SR-IOV capability on the node:
   `kubectl get sriovnetworknodestates -n nvidia-network-operator -o yaml`
   Look at `.status.interfaces[].totalvfs` for the hardware maximum.
2. Verify `numVfs` in your config does not exceed `totalvfs`.
3. Check that SR-IOV is enabled in the node's BIOS/firmware.
4. On some platforms, a node reboot is required after enabling SR-IOV in firmware.
5. Verify the PF is not already in use by another VF manager.

---

## 5. RDMA Device Not Visible in Pods

**Symptom**: Pods requesting RDMA resources either fail to schedule or start but cannot
see `/dev/infiniband/` devices.

**Root cause**: The RDMA device plugin is not configured, or the device plugin is not
advertising RDMA-capable resources.

**Remediation**:
1. Check which device plugin is configured: `rdmaSharedDevicePlugin` for shared RDMA,
   or `sriovDevicePlugin` for SR-IOV RDMA.
2. Verify the device plugin pods are running:
   `kubectl get pods -n nvidia-network-operator -l app=sriov-device-plugin`
3. Check node allocatable resources include RDMA resources:
   `kubectl get node <node> -o json | jq '.status.allocatable'`
4. Verify the NICs are RDMA-capable (ConnectX-5 and above for RoCE, ConnectX-3 and
   above for InfiniBand).
5. For shared RDMA, verify `hcaMax` is set appropriately (default 63).

---

## 6. NIC Name Template Not Applied

**Symptom**: NIC interfaces on nodes are not renamed according to the expected naming
pattern (e.g., `eth_r0`, `eth_r1`). PCI addresses are used instead of friendly names.

**Root cause**: The NIC configuration operator is not deployed, or
`deployNicInterfaceNameTemplate` is not enabled.

**Remediation**:
1. Verify `nicConfigurationOperator.deployNicInterfaceNameTemplate: true` in your config.
2. Check that the nic-configuration-operator pods are running:
   `kubectl get pods -n nvidia-network-operator -l app=nic-configuration-operator`
3. NIC name templates are only enabled when needed: merged groups with cross-rail PCI
   conflicts, or `rdma_shared` deployment with empty NetworkInterface fields.
4. If the operator is running but templates are not applied, check its logs for errors.
5. A node reboot may be required for NIC renaming to take effect.

---

## 7. IP Allocation Failures

**Symptom**: Pods get network interfaces but no IP addresses. NV-IPAM logs show pool
exhaustion or configuration errors.

**Root cause**: NV-IPAM pool does not exist, subnet is exhausted, or subnet configuration
overlaps with existing networks.

**Remediation**:
1. Verify the IPPool exists:
   `kubectl get ippools -n nvidia-network-operator -o yaml`
2. Check the pool has available addresses (compare allocated vs total).
3. Verify subnet configuration does not overlap with the cluster pod CIDR or service CIDR.
4. If using auto-generation (startingSubnet/mask/offset), verify the generated subnets
   are correct by inspecting the generated manifests.
5. Check NV-IPAM node agent logs:
   `kubectl logs -n nvidia-network-operator -l app=nv-ipam-node --tail=100`

---

## 8. NetworkAttachmentDefinition Missing

**Symptom**: Pods requesting secondary networks fail with "network not found" errors.
`net-attach-def` does not exist in the expected namespace.

**Root cause**: The corresponding SriovNetwork, MacvlanNetwork, or HostDeviceNetwork
custom resource was not created, or it was created in the wrong namespace.

**Remediation**:
1. Check which network CRs exist:
   `kubectl get sriovnetworks,macvlannetworks,hostdevicenetworks -n nvidia-network-operator`
2. Verify the CR's `networkNamespace` field matches the namespace where pods will run.
3. Check that the network CR references a valid resource name that matches the device
   plugin configuration.
4. If using l8k, verify the generated manifests include the correct network CRs:
   `./build/l8k --user-config config.yaml --save-deployment-files ./output`
   Then inspect `./output/network-operator/` for network YAML files.

---

## 9. Pod Stuck in ContainerCreating

**Symptom**: Pods with secondary network annotations are stuck in `ContainerCreating`.
Events show CNI errors or timeout messages.

**Root cause**: Multus CNI cannot find or execute the secondary CNI plugin, or the
NetworkAttachmentDefinition is missing/misconfigured.

**Remediation**:
1. Check pod events: `kubectl describe pod <pod-name>` -- look for CNI-related errors.
2. Verify the NetworkAttachmentDefinition exists in the pod's namespace:
   `kubectl get net-attach-def -n <namespace>`
3. Check multus logs on the affected node:
   `kubectl logs -n kube-system -l app=multus --tail=100`
4. Verify the CNI binary exists on the node at `/opt/cni/bin/` (e.g., `sriov`,
   `macvlan`, `host-device`).
5. If the NAD references an SR-IOV resource, verify the resource appears in the node's
   allocatable resources.

---

## 10. Discovery Finds 0 Groups

**Symptom**: Running `--discover-cluster-config` completes but produces an empty
`clusterConfig` array or reports "no groups found".

**Root cause**: The label selector does not match any nodes, or nodes do not have
NVIDIA/Mellanox NICs detected by NFD.

**Remediation**:
1. Verify nodes have the expected label:
   `kubectl get nodes -l feature.node.kubernetes.io/pci-15b3.present=true`
2. If using `--node-selector`, verify your selector matches existing node labels:
   `kubectl get nodes --show-labels`
3. Verify Node Feature Discovery (NFD) is running:
   `kubectl get pods -n node-feature-discovery` (or the namespace where NFD is deployed)
4. Check that NodeFeature CRs are populated:
   `kubectl get nodefeatures -A`
5. If NFD is running but NIC labels are missing, the NICs may not be properly detected
   by the host OS. Check `lspci | grep Mellanox` on the node.
