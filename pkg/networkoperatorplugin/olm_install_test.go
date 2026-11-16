// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// ocpInstallTestConfig builds a minimal, valid OCP SR-IOV config for exercising
// the opt-in OLM install rendering.
func ocpInstallTestConfig(t *testing.T) *config.LaunchKitConfig {
	return ocpInstallTestConfigFor(t, &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: false})
}

func ocpInstallTestConfigFor(t *testing.T, profile *config.Profile) *config.LaunchKitConfig {
	t.Helper()
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Flavor = config.FlavorOCP
	require.NoError(t, config.ApplyFlavorDefaults(cfg))
	cfg.Profile = profile
	cfg.NetworkNamespaces = []string{"rdma-workloads"}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NetworkOperator.SkipHelmChart = true
	cfg.Sriov.OperatorNamespace = config.DefaultSriovOperatorNamespace
	cfg.DOCADriver.Enable = false
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
	cfg.ClusterConfig = cfg.ClusterConfig[:2]
	for i, node := range []string{"cloud-dev-66", "cloud-dev-67"} {
		cfg.ClusterConfig[i].PFs = cfg.ClusterConfig[i].PFs[:1]
		cfg.ClusterConfig[i].WorkerNodes = []string{node}
		cfg.ClusterConfig[i].NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}
	return cfg
}

// TestOCPOperatorInstallDefaultOffEmitsNothing verifies the opt-in OLM install
// templates produce no files when management is off — the default output stays
// byte-for-byte identical to today.
func TestOCPOperatorInstallDefaultOffEmitsNothing(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	require.Nil(t, cfg.OCPOperatorInstall)

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)

	for _, name := range []string{"00-operator-namespaces.yaml", "00-operatorgroups.yaml", "00-subscriptions.yaml"} {
		assert.Empty(t, fileNamesMatching(rendered, name), "expected %q not to be emitted when management is off", name)
	}
}

// TestOCPOperatorInstallManagedRendersInstallArtifacts verifies that, with
// management on, the three install artifacts render and the Subscriptions carry
// the four expected packages, Automatic approval, and the config's
// channel/source.
func TestOCPOperatorInstallManagedRendersInstallArtifacts(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{Manage: true}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)

	require.Len(t, fileNamesMatching(rendered, "00-operator-namespaces.yaml"), 1)
	require.Len(t, fileNamesMatching(rendered, "00-operatorgroups.yaml"), 1)
	_, subs := fileMatching(t, rendered, "00-subscriptions.yaml")

	docs := parseDocs(t, "00-subscriptions.yaml", subs)
	require.Len(t, docs, 4, "sriov deployment configures four operators")

	packages := make([]string, 0, len(docs))
	for _, doc := range docs {
		assert.Equal(t, "Subscription", doc["kind"])
		packages = append(packages, specString(t, doc, "name"))
		assert.Equal(t, "Automatic", specString(t, doc, "installPlanApproval"))
		assert.Equal(t, "stable", specString(t, doc, "channel"))
		assert.Equal(t, "openshift-marketplace", specString(t, doc, "sourceNamespace"))
	}
	sort.Strings(packages)
	assert.Equal(t, []string{
		"nfd",
		"nvidia-maintenance-operator",
		"nvidia-network-operator",
		"sriov-network-operator",
	}, packages)

	// NVIDIA packages come from the certified catalog; the Red Hat ones from
	// redhat-operators.
	for _, doc := range docs {
		source := specString(t, doc, "source")
		switch specString(t, doc, "name") {
		case "nvidia-network-operator", "nvidia-maintenance-operator":
			assert.Equal(t, "certified-operators", source)
		default:
			assert.Equal(t, "redhat-operators", source)
		}
	}

	// A namespace is created for every operator — none of these ship with
	// OpenShift. The network-operator namespace comes from
	// networkOperator.namespace in the test config ("network-operator").
	_, namespaces := fileMatching(t, rendered, "00-operator-namespaces.yaml")
	nsDocs := parseDocs(t, "00-operator-namespaces.yaml", namespaces)
	nsNames := make([]string, 0, len(nsDocs))
	for _, doc := range nsDocs {
		assert.Equal(t, "Namespace", doc["kind"])
		nsNames = append(nsNames, metaString(t, doc, "name"))
	}
	sort.Strings(nsNames)
	assert.Equal(t, []string{
		"network-operator",
		"nvidia-maintenance-operator",
		"openshift-nfd",
		"openshift-sriov-network-operator",
	}, nsNames)
}

// TestOCPOperatorInstallDedupesSharedNamespace verifies operators sharing a
// namespace yield one OperatorGroup and one Namespace for it (not one per
// package), while Subscriptions stay per package.
func TestOCPOperatorInstallDedupesSharedNamespace(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage: true,
		Operators: []config.OCPOperatorSubscription{
			{Package: "nvidia-network-operator", Namespace: "shared-ns"},
			{Package: "nvidia-maintenance-operator", Namespace: "shared-ns"},
			{Package: "nfd", Namespace: "openshift-nfd"},
		},
	}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma-ocp"), cfg)
	require.NoError(t, err)

	_, groups := fileMatching(t, rendered, "00-operatorgroups.yaml")
	ogDocs := parseDocs(t, "00-operatorgroups.yaml", groups)
	ogNamespaces := make([]string, 0, len(ogDocs))
	for _, doc := range ogDocs {
		assert.Equal(t, "OperatorGroup", doc["kind"])
		ogNamespaces = append(ogNamespaces, metaString(t, doc, "namespace"))
	}
	sort.Strings(ogNamespaces)
	assert.Equal(t, []string{"openshift-nfd", "shared-ns"}, ogNamespaces)

	_, namespaces := fileMatching(t, rendered, "00-operator-namespaces.yaml")
	nsDocs := parseDocs(t, "00-operator-namespaces.yaml", namespaces)
	nsNames := make([]string, 0, len(nsDocs))
	for _, doc := range nsDocs {
		nsNames = append(nsNames, metaString(t, doc, "name"))
	}
	sort.Strings(nsNames)
	assert.Equal(t, []string{"openshift-nfd", "shared-ns"}, nsNames)

	_, subs := fileMatching(t, rendered, "00-subscriptions.yaml")
	assert.Len(t, parseDocs(t, "00-subscriptions.yaml", subs), 3)
}

// TestOCPOperatorInstallRendersForEveryProfile guards that --manage-operators
// emits Subscriptions for every supported OpenShift profile, each with the
// operator set its deployment requires (SR-IOV adds sriov-network-operator).
func TestOCPOperatorInstallRendersForEveryProfile(t *testing.T) {
	base := []string{"nfd", "nvidia-maintenance-operator", "nvidia-network-operator"}
	withSriov := append(append([]string{}, base...), "sriov-network-operator")
	sort.Strings(withSriov)

	cases := []struct {
		dir      string
		profile  *config.Profile
		packages []string
	}{
		{"sriov-ethernet-rdma-ocp", &config.Profile{Fabric: "ethernet", Deployment: "sriov"}, withSriov},
		{"sriov-ib-rdma-ocp", &config.Profile{Fabric: "infiniband", Deployment: "sriov"}, withSriov},
		{"host-device-rdma-ocp", &config.Profile{Fabric: "ethernet", Deployment: "host_device"}, base},
		{"macvlan-rdma-shared-ocp", &config.Profile{Fabric: "ethernet", Deployment: "rdma_shared"}, base},
		{"ipoib-rdma-shared-ocp", &config.Profile{Fabric: "infiniband", Deployment: "rdma_shared"}, base},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			cfg := ocpInstallTestConfigFor(t, tc.profile)
			cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{Manage: true}

			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
				loadProfileFromDir(t, tc.dir), cfg)
			require.NoError(t, err)

			require.Len(t, fileNamesMatching(rendered, "00-subscriptions.yaml"), 1)
			_, subs := fileMatching(t, rendered, "00-subscriptions.yaml")
			docs := parseDocs(t, "00-subscriptions.yaml", subs)

			packages := make([]string, 0, len(docs))
			for _, doc := range docs {
				packages = append(packages, specString(t, doc, "name"))
			}
			sort.Strings(packages)
			assert.Equal(t, tc.packages, packages)
		})
	}
}

// TestDefaultOCPOperatorSubscriptionsNonSriov verifies the builder drops the
// SR-IOV operator for non-sriov deployments (three operators instead of four).
func TestDefaultOCPOperatorSubscriptionsNonSriov(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.Profile.Deployment = "rdma_shared"

	subs := DefaultOCPOperatorSubscriptions(cfg)
	packages := make([]string, 0, len(subs))
	for _, sub := range subs {
		packages = append(packages, sub.Package)
		assert.Equal(t, "Automatic", sub.InstallPlanApproval)
	}
	sort.Strings(packages)
	assert.Equal(t, []string{"nfd", "nvidia-maintenance-operator", "nvidia-network-operator"}, packages)
}
