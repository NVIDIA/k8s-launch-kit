// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestOpenShiftSharesNetworkButKeepsNodePoliciesPerSource(t *testing.T) {
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Flavor = config.FlavorOCP
	require.NoError(t, config.ApplyFlavorDefaults(cfg))
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: false}
	cfg.NetworkNamespaces = []string{"rdma-workloads"}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NetworkOperator.SkipHelmChart = true
	cfg.Sriov.OperatorNamespace = "openshift-sriov-network-operator"
	cfg.DOCADriver.Enable = false
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
	cfg.ClusterConfig = cfg.ClusterConfig[:2]
	for i, node := range []string{"cloud-dev-66", "cloud-dev-67"} {
		cfg.ClusterConfig[i].PFs = cfg.ClusterConfig[i].PFs[:1]
		cfg.ClusterConfig[i].WorkerNodes = []string{node}
		cfg.ClusterConfig[i].NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(rendered, "20-ippool"), 1)
	require.Len(t, fileNamesMatching(rendered, "50-sriovnetwork"), 1)
	require.Len(t, fileNamesMatching(rendered, "60-example-daemonset"), 1)
	require.Len(t, fileNamesMatching(rendered, "40-sriovnetworknodepolicy"), 2)

	_, pool := fileMatching(t, rendered, "20-ippool")
	_, network := fileMatching(t, rendered, "50-sriovnetwork")
	_, workload := fileMatching(t, rendered, "60-example-daemonset")
	for _, content := range []string{pool, workload} {
		require.Contains(t, content, "kubernetes.io/hostname")
		require.Contains(t, content, "cloud-dev-66")
		require.Contains(t, content, "cloud-dev-67")
		require.NotContains(t, content, "cloud-dev-68")
	}
	require.Contains(t, network, `"poolName": "nv-ipam-pool-gpu-model-y"`)
	require.Contains(t, workload, "sriov-network-gpu-model-y")

	policy66 := rendered["40-sriovnetworknodepolicy-group-0.yaml"]
	policy67 := rendered["40-sriovnetworknodepolicy-group-1.yaml"]
	require.Contains(t, policy66, "cloud-dev-66")
	require.Contains(t, policy66, "0000:19:00.0")
	require.NotContains(t, policy66, "0000:1a:00.0")
	require.Contains(t, policy67, "cloud-dev-67")
	require.Contains(t, policy67, "0000:1a:00.0")
	require.NotContains(t, policy67, "0000:19:00.0")

	cfg.ClusterConfig[0].WorkerNodes = nil
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.ErrorContains(t, err, "requires workerNodes")
	cfg.ClusterConfig[0].WorkerNodes = []string{"cloud-dev-66"}
	cfg.ClusterConfig[0].NodeSelector = nil
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.ErrorContains(t, err, "requires nodeSelector")
}

func TestOpenShiftRendersIdenticalMachineGroupsOnExactWorkers(t *testing.T) {
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Flavor = config.FlavorOCP
	require.NoError(t, config.ApplyFlavorDefaults(cfg))
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov"}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NetworkOperator.SkipHelmChart = true
	cfg.DOCADriver.Enable = true
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
	cfg.ClusterConfig = cfg.ClusterConfig[:2]
	cfg.Maintenance.MaxUnavailable = config.IntOrPercentFromInt32(0)
	identifier := config.GeneratedGroupIdentifier("machine-a", "gpu-model-y")
	for i, worker := range []string{"worker-a", "worker-b"} {
		group := &cfg.ClusterConfig[i]
		group.PFs = group.PFs[:1]
		group.MachineType = "machine-a"
		group.GPUType = "gpu-model-y"
		group.Identifier = identifier
		group.WorkerNodes = []string{worker}
		group.NodeSelector = map[string]string{config.MachineLabelKey: identifier}
	}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(rendered, "40-sriovnetworknodepolicy"), 2)
	require.Len(t, fileNamesMatching(rendered, "11-nicnodepolicy"), 2)
	require.Len(t, fileNamesMatching(rendered, "35-sriovnetworkpoolconfig"), 1)
	require.Len(t, fileNamesMatching(rendered, "20-ippool"), 1)
	require.Len(t, fileNamesMatching(rendered, "50-sriovnetwork"), 1)
	require.Len(t, fileNamesMatching(rendered, "60-example-daemonset"), 1)
	_, pool := fileMatching(t, rendered, "35-sriovnetworkpoolconfig")
	require.Contains(t, pool, "maxUnavailable: 0")
	require.Contains(t, pool, "worker-a")
	require.Contains(t, pool, "worker-b")
	require.NotContains(t, pool, "worker-c")
	for _, worker := range []struct{ name, pci string }{
		{name: "worker-a", pci: "0000:19:00.0"},
		{name: "worker-b", pci: "0000:1a:00.0"},
	} {
		found := false
		for name, policy := range rendered {
			if !strings.HasPrefix(name, "40-sriovnetworknodepolicy-") || !strings.Contains(policy, worker.name) {
				continue
			}
			found = true
			require.Contains(t, policy, worker.pci)
			require.NotContains(t, policy, map[string]string{"worker-a": "worker-b", "worker-b": "worker-a"}[worker.name])
		}
		require.True(t, found, "missing policy for %s", worker.name)
	}
	cfg.Profile.Fabric = "infiniband"
	ibRendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ib-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(ibRendered, "35-sriovnetworkpoolconfig"), 1)
	_, ibPool := fileMatching(t, ibRendered, "35-sriovnetworkpoolconfig")
	require.Contains(t, ibPool, "maxUnavailable: 0")
}

func TestOpenShiftSplitsMultiWorkerHardwareGroup(t *testing.T) {
	sources := []config.ClusterConfig{{
		Identifier: "machine-a", WorkerNodes: []string{"worker-a", "worker-b"},
		NodeSelector: map[string]string{config.MachineLabelKey: "machine-a"},
	}}
	scoped, err := ocpWorkerPolicySources(sources)
	require.NoError(t, err)
	require.Len(t, scoped, 2)
	require.NotEqual(t, scoped[0].Identifier, scoped[1].Identifier)
	require.Equal(t, map[string]string{"kubernetes.io/hostname": "worker-a"}, scoped[0].NodeSelector)
	require.Equal(t, map[string]string{"kubernetes.io/hostname": "worker-b"}, scoped[1].NodeSelector)
}
