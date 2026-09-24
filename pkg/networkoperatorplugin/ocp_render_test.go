// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"os"
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

	workloadPath := filepath.Join(t.TempDir(), "workload.yaml")
	require.NoError(t, os.WriteFile(workloadPath, []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: custom\nspec:\n  containers:\n  - name: test\n    image: busybox:1.36\n"), 0600))
	cfg.Workload = &config.WorkloadConfig{Manifest: workloadPath}
	custom, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(custom, "90-workload"), 1)
	_, customPod := fileMatching(t, custom, "90-workload")
	require.Contains(t, customPod, "sriov-network-gpu-model-y")
	require.Contains(t, customPod, "openshift.io/sriov_resource")
	require.Contains(t, customPod, "cloud-dev-66")
	require.Contains(t, customPod, "cloud-dev-67")
	require.NotContains(t, customPod, "cloud-dev-68")
	require.NotContains(t, customPod, "nvidia.com/sriov_resource")
	cfg.NetworkNamespaces = []string{"rdma-a", "rdma-b"}
	multiNS, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(multiNS, "90-workload"), 2)
	for _, ns := range cfg.NetworkNamespaces {
		filename := "90-workload-gpu-model-y-" + ns + ".yaml"
		require.Contains(t, multiNS[filename], "sriov-network-gpu-model-y-"+ns)
		require.Contains(t, multiNS[filename], "namespace: "+ns)
	}
	cfg.NetworkNamespaces = []string{"rdma-workloads"}
	cfg.Workload = nil

	cfg.Profile.Multirail = true
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = true
	named, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(named, "30-nicinterfacenametemplate"), 2)
	for name, content := range named {
		if !strings.Contains(name, "30-nicinterfacenametemplate") {
			continue
		}
		require.Contains(t, content, "kubernetes.io/hostname")
		require.True(t, strings.Contains(content, "cloud-dev-66") != strings.Contains(content, "cloud-dev-67"))
	}
	cfg.Profile.Multirail = false
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false

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
	cfg.Profile.Multirail = true
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = true
	named, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(named, "30-nicinterfacenametemplate"), 2)
	for name, content := range named {
		if strings.Contains(name, "30-nicinterfacenametemplate") {
			require.Contains(t, content, "kubernetes.io/hostname")
			require.True(t, strings.Contains(content, "worker-a") != strings.Contains(content, "worker-b"))
		}
	}
	cfg.Profile.Multirail = false
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
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

func TestOpenShiftNamesSameGPUBucketsByRailCount(t *testing.T) {
	plans := []RenderBucket{
		{Merged: config.ClusterConfig{Identifier: "gpu", PFs: []config.PFConfig{{Traffic: "east-west"}}}, Sources: []config.ClusterConfig{{MergedIdentifier: "gpu"}}},
		{Merged: config.ClusterConfig{Identifier: "gpu", PFs: []config.PFConfig{{Traffic: "east-west"}, {Traffic: "east-west"}}}, Sources: []config.ClusterConfig{{MergedIdentifier: "gpu"}}},
	}
	disambiguateOCPSameGPUBuckets(plans)
	require.Equal(t, "gpu-r1", plans[0].Merged.Identifier)
	require.Equal(t, "gpu-r2", plans[1].Merged.Identifier)
	for _, plan := range plans {
		require.Equal(t, plan.Merged.Identifier, plan.Merged.MergedIdentifier)
		require.Equal(t, plan.Merged.Identifier, plan.Sources[0].MergedIdentifier)
	}
}

func TestOpenShiftRendersSameGPUWithDifferentRailCounts(t *testing.T) {
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Flavor = config.FlavorOCP
	require.NoError(t, config.ApplyFlavorDefaults(cfg))
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NetworkOperator.SkipHelmChart = true
	cfg.DOCADriver.Enable = false
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
	cfg.NetworkNamespaces = []string{"rdma-workloads"}
	groups := []config.ClusterConfig{cfg.ClusterConfig[0], cfg.ClusterConfig[1], cfg.ClusterConfig[0], cfg.ClusterConfig[1]}
	for i := range groups {
		groups[i].Identifier = "machine-" + string(rune('a'+i))
		groups[i].WorkerNodes = []string{"worker-" + string(rune('a'+i))}
		groups[i].NodeSelector = map[string]string{"kubernetes.io/hostname": groups[i].WorkerNodes[0]}
		if i < 2 {
			groups[i].PFs = groups[i].PFs[:1]
		} else {
			groups[i].PFs = groups[i].PFs[:2]
		}
	}
	groups[2].Identifier = groups[0].Identifier // Same source identity in different rail-count buckets.
	cfg.ClusterConfig = groups
	workloadPath := filepath.Join(t.TempDir(), "workload.yaml")
	require.NoError(t, os.WriteFile(workloadPath, []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: custom\nspec:\n  containers:\n  - name: test\n    image: busybox:1.36\n"), 0600))
	cfg.Workload = &config.WorkloadConfig{Manifest: workloadPath}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)
	require.Len(t, fileNamesMatching(rendered, "90-workload"), 2)
	require.Len(t, fileNamesMatching(rendered, "50-sriovnetwork"), 2)
	require.Len(t, fileNamesMatching(rendered, "40-sriovnetworknodepolicy"), 4)
	require.Contains(t, rendered, "90-workload-gpu-model-y-r1.yaml")
	require.Contains(t, rendered, "90-workload-gpu-model-y-r2.yaml")
	require.Contains(t, rendered["90-workload-gpu-model-y-r1.yaml"], "sriov-network-rail-0-gpu-model-y-r1")
	require.Contains(t, rendered["90-workload-gpu-model-y-r2.yaml"], "sriov-network-rail-1-gpu-model-y-r2")
}
