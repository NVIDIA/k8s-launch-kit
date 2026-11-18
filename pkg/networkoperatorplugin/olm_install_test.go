// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		name := specString(t, doc, "name")
		packages = append(packages, name)
		assert.Equal(t, "Automatic", specString(t, doc, "installPlanApproval"))
		// The network operator pins its release-specific channel offline; the
		// others fall back to the stable channel.
		if name == "nvidia-network-operator" {
			assert.Equal(t, "v26.7", specString(t, doc, "channel"))
		} else {
			assert.Equal(t, "stable", specString(t, doc, "channel"))
		}
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

// TestOCPOperatorInstallDedupesSharedNamespace verifies operators configured into
// the same namespace yield one OperatorGroup and one Namespace for it (not one per
// package), while Subscriptions stay per package.
func TestOCPOperatorInstallDedupesSharedNamespace(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.NetworkOperator.Namespace = "shared-ns"
	require.NotNil(t, cfg.Maintenance)
	cfg.Maintenance.OperatorNamespace = "shared-ns"
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{Manage: true}

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
	assert.Equal(t, []string{"openshift-nfd", "openshift-sriov-network-operator", "shared-ns"}, ogNamespaces)

	_, namespaces := fileMatching(t, rendered, "00-operator-namespaces.yaml")
	nsDocs := parseDocs(t, "00-operator-namespaces.yaml", namespaces)
	nsNames := make([]string, 0, len(nsDocs))
	for _, doc := range nsDocs {
		nsNames = append(nsNames, metaString(t, doc, "name"))
	}
	sort.Strings(nsNames)
	assert.Equal(t, []string{"openshift-nfd", "openshift-sriov-network-operator", "shared-ns"}, nsNames)

	_, subs := fileMatching(t, rendered, "00-subscriptions.yaml")
	assert.Len(t, parseDocs(t, "00-subscriptions.yaml", subs), 4)
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

func networkOperatorManifest(name, catalogSource string, channels []interface{}) *unstructured.Unstructured {
	return ocpTestObject("packages.operators.coreos.com/v1", "PackageManifest", "openshift-marketplace", name,
		map[string]interface{}{
			"status": map[string]interface{}{
				"packageName":    "nvidia-network-operator",
				"defaultChannel": "stable",
				"catalogSource":  catalogSource,
				"channels":       channels,
			},
		})
}

func operatorSub(t *testing.T, cfg *config.LaunchKitConfig, pkg string) config.OCPOperatorSubscription {
	t.Helper()
	for _, op := range cfg.OCPOperatorInstall.Operators {
		if op.Package == pkg {
			return op
		}
	}
	t.Fatalf("operator %q not in resolved install set", pkg)
	return config.OCPOperatorSubscription{}
}

var releaseChannel277 = []interface{}{
	map[string]interface{}{"name": "stable", "currentCSVDesc": map[string]interface{}{"version": "26.10.0"}},
	map[string]interface{}{"name": "v26.7", "currentCSVDesc": map[string]interface{}{"version": "26.7.1"}},
}

// TestDiscoverOCPOperatorInstallMatchesReleaseChannel verifies discovery pins the
// network-operator channel to the selected release line, not the catalog default.
func TestDiscoverOCPOperatorInstallMatchesReleaseChannel(t *testing.T) {
	cfg := ocpInstallTestConfig(t) // selectedRelease 26.7
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	c := fake.NewClientBuilder().WithObjects(manifest).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	assert.Equal(t, "v26.7", operatorSub(t, cfg, "nvidia-network-operator").Channel)
}

// TestDiscoverOCPOperatorInstallErrsWithoutReleaseChannel verifies a hard error
// when no channel is on the selected release line.
func TestDiscoverOCPOperatorInstallErrsWithoutReleaseChannel(t *testing.T) {
	cfg := ocpInstallTestConfig(t) // selectedRelease 26.7
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", []interface{}{
		map[string]interface{}{"name": "stable", "currentCSVDesc": map[string]interface{}{"version": "26.10.0"}},
	})
	c := fake.NewClientBuilder().WithObjects(manifest).Build()

	err := DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches selected release")
}

// TestDiscoverOCPOperatorInstallPrefersReleaseSpecificChannel verifies the
// release-named channel wins over a moving channel that currently sits on the same
// release, even when the moving channel is listed first.
func TestDiscoverOCPOperatorInstallPrefersReleaseSpecificChannel(t *testing.T) {
	cfg := ocpInstallTestConfig(t) // selectedRelease 26.7
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", []interface{}{
		map[string]interface{}{"name": "stable", "currentCSVDesc": map[string]interface{}{"version": "26.7.1"}},
		map[string]interface{}{"name": "v26.7", "currentCSVDesc": map[string]interface{}{"version": "26.7.1"}},
	})
	c := fake.NewClientBuilder().WithObjects(manifest).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	assert.Equal(t, "v26.7", operatorSub(t, cfg, "nvidia-network-operator").Channel)
}

// TestDiscoverOCPOperatorInstallPrefersExpectedCatalog verifies that, when a
// package is in several catalogs, discovery picks the expected source rather than
// whichever the API lists first.
func TestDiscoverOCPOperatorInstallPrefersExpectedCatalog(t *testing.T) {
	cfg := ocpInstallTestConfig(t) // network-operator expects certified-operators
	// Name the mirror so it sorts before the certified copy: the old first-seen
	// logic would pick it, the fix must still choose certified-operators.
	certified := networkOperatorManifest("nvidia-network-operator-certified", "certified-operators", releaseChannel277)
	mirror := networkOperatorManifest("a-nvidia-network-operator-mirror", "test-catalog", []interface{}{
		map[string]interface{}{"name": "stable", "currentCSVDesc": map[string]interface{}{"version": "26.10.0"}},
	})
	c := fake.NewClientBuilder().WithObjects(mirror, certified).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	op := operatorSub(t, cfg, "nvidia-network-operator")
	assert.Equal(t, "certified-operators", op.Source)
	assert.Equal(t, "v26.7", op.Channel)
}

// TestDiscoverOCPOperatorInstallErrsOnAmbiguousCatalog verifies a hard error when
// a package is in multiple catalogs and none is the expected source.
func TestDiscoverOCPOperatorInstallErrsOnAmbiguousCatalog(t *testing.T) {
	cfg := ocpInstallTestConfig(t) // expects certified-operators
	a := networkOperatorManifest("nvidia-network-operator-a", "mirror-a", releaseChannel277)
	b := networkOperatorManifest("nvidia-network-operator-b", "mirror-b", releaseChannel277)
	c := fake.NewClientBuilder().WithObjects(a, b).Build()

	err := DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple catalogs")
}

// TestEnsureOCPOperatorInstallDefaultsReconcilesNamespace verifies the operator
// namespace tracks the effective config while saved catalog/approval are kept.
func TestEnsureOCPOperatorInstallDefaultsReconcilesNamespace(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage: true,
		Operators: []config.OCPOperatorSubscription{{
			Package: "nvidia-network-operator", Namespace: "old-ns",
			Channel: "v26.7", Source: "certified-operators",
			SourceNamespace: "openshift-marketplace", InstallPlanApproval: "Manual",
		}},
	}
	cfg.NetworkOperator.Namespace = "new-ns"

	EnsureOCPOperatorInstallDefaults(cfg)

	op := operatorSub(t, cfg, "nvidia-network-operator")
	assert.Equal(t, "new-ns", op.Namespace, "namespace tracks the effective config")
	assert.Equal(t, "v26.7", op.Channel, "saved channel preserved")
	assert.Equal(t, "Manual", op.InstallPlanApproval, "saved approval preserved")
}

// TestEnsureOCPOperatorInstallDefaultsReconcilesStaleChannel verifies a saved
// network-operator channel is repointed when selectedRelease changes offline.
func TestEnsureOCPOperatorInstallDefaultsReconcilesStaleChannel(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.NetworkOperator.SelectedRelease = "26.10"
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage: true,
		Operators: []config.OCPOperatorSubscription{{
			Package: "nvidia-network-operator", Namespace: cfg.NetworkOperator.Namespace,
			Channel: "v26.7", Source: "certified-operators", SourceNamespace: "openshift-marketplace",
		}},
	}

	EnsureOCPOperatorInstallDefaults(cfg)

	assert.Equal(t, "v26.10", operatorSub(t, cfg, "nvidia-network-operator").Channel,
		"stale channel reconciled to the selected release")
}

// TestDiscoverOCPOperatorInstallHonorsSavedChoices verifies discovery uses the
// saved source to select the catalog and preserves the saved approval.
func TestDiscoverOCPOperatorInstallHonorsSavedChoices(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage: true,
		Operators: []config.OCPOperatorSubscription{{
			Package: "nvidia-network-operator", Source: "mirror-catalog", InstallPlanApproval: "Manual",
		}},
	}
	certified := networkOperatorManifest("nwo-certified", "certified-operators", releaseChannel277)
	mirror := networkOperatorManifest("nwo-mirror", "mirror-catalog", releaseChannel277)
	c := fake.NewClientBuilder().WithObjects(certified, mirror).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	op := operatorSub(t, cfg, "nvidia-network-operator")
	assert.Equal(t, "mirror-catalog", op.Source, "saved source selects the catalog")
	assert.Equal(t, "Manual", op.InstallPlanApproval, "saved approval preserved")
}

// TestDiscoverOCPOperatorInstallSavedSourceResolvesAmbiguity verifies that an
// explicit saved source disambiguates mirror-only catalogs instead of erroring.
func TestDiscoverOCPOperatorInstallSavedSourceResolvesAmbiguity(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage: true,
		Operators: []config.OCPOperatorSubscription{{
			Package: "nvidia-network-operator", Source: "mirror-b",
		}},
	}
	a := networkOperatorManifest("nwo-a", "mirror-a", releaseChannel277)
	b := networkOperatorManifest("nwo-b", "mirror-b", releaseChannel277)
	c := fake.NewClientBuilder().WithObjects(a, b).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	assert.Equal(t, "mirror-b", operatorSub(t, cfg, "nvidia-network-operator").Source)
}

// TestDiscoverOCPOperatorInstallReusesExistingSubscriptionName verifies discovery
// adopts an installed Subscription's custom name so apply updates it rather than
// creating a duplicate; spec.name stays the package.
func TestDiscoverOCPOperatorInstallReusesExistingSubscriptionName(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	existing := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", cfg.NetworkOperator.Namespace, "custom-nwo-subscription",
		map[string]interface{}{"spec": map[string]interface{}{"name": "nvidia-network-operator"}})
	c := fake.NewClientBuilder().WithObjects(manifest, existing).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	op := operatorSub(t, cfg, "nvidia-network-operator")
	assert.Equal(t, "custom-nwo-subscription", op.Name, "reuse the existing Subscription name")
	assert.Equal(t, "nvidia-network-operator", op.Package, "spec.name stays the package")

	// An operator with no existing Subscription falls back to the package name.
	assert.Equal(t, "nfd", operatorSub(t, cfg, "nfd").Name)
}

func operatorGroup(t *testing.T, cfg *config.LaunchKitConfig, namespace string) config.OCPOperatorGroup {
	t.Helper()
	for _, g := range cfg.OCPOperatorInstall.OperatorGroups {
		if g.Namespace == namespace {
			return g
		}
	}
	t.Fatalf("no OperatorGroup for namespace %q", namespace)
	return config.OCPOperatorGroup{}
}

// TestDiscoverOCPOperatorInstallReusesExistingOperatorGroup verifies discovery
// adopts an existing group's name and target namespaces so apply updates it
// instead of creating a second group in the namespace.
func TestDiscoverOCPOperatorInstallReusesExistingOperatorGroup(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	ns := cfg.NetworkOperator.Namespace
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	existing := ocpTestObject("operators.coreos.com/v1", "OperatorGroup", ns, "existing-site-group",
		map[string]interface{}{"spec": map[string]interface{}{"targetNamespaces": []interface{}{ns}}})
	c := fake.NewClientBuilder().WithObjects(manifest, existing).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	g := operatorGroup(t, cfg, ns)
	assert.Equal(t, "existing-site-group", g.Name, "reuse the existing OperatorGroup name")
	assert.Equal(t, []string{ns}, g.TargetNamespaces, "preserve its target namespaces")

	// A namespace with no existing group falls back to a namespace-named OwnNamespace group.
	nfdGroup := operatorGroup(t, cfg, "openshift-nfd")
	assert.Equal(t, "openshift-nfd", nfdGroup.Name)
	assert.Equal(t, []string{"openshift-nfd"}, nfdGroup.TargetNamespaces)
}

// TestDiscoverOCPOperatorInstallErrsOnMultipleOperatorGroups verifies a hard error
// when a namespace already has more than one OperatorGroup.
func TestDiscoverOCPOperatorInstallErrsOnMultipleOperatorGroups(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	ns := cfg.NetworkOperator.Namespace
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	og1 := ocpTestObject("operators.coreos.com/v1", "OperatorGroup", ns, "group-a",
		map[string]interface{}{"spec": map[string]interface{}{"targetNamespaces": []interface{}{ns}}})
	og2 := ocpTestObject("operators.coreos.com/v1", "OperatorGroup", ns, "group-b",
		map[string]interface{}{"spec": map[string]interface{}{"targetNamespaces": []interface{}{ns}}})
	c := fake.NewClientBuilder().WithObjects(manifest, og1, og2).Build()

	err := DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TooManyOperatorGroups")
}

// TestDiscoverOCPOperatorInstallErrsOnSelectorOperatorGroup verifies a hard error
// for a selector-based group the descriptor cannot reproduce.
func TestDiscoverOCPOperatorInstallErrsOnSelectorOperatorGroup(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	ns := cfg.NetworkOperator.Namespace
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	og := ocpTestObject("operators.coreos.com/v1", "OperatorGroup", ns, "selector-group",
		map[string]interface{}{"spec": map[string]interface{}{"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"team": "net"}}}})
	c := fake.NewClientBuilder().WithObjects(manifest, og).Build()

	err := DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selector-based")
}

// TestDiscoverOCPOperatorInstallRefreshesSavedOperatorGroupName verifies a saved
// group name is refreshed from the live namespace, so re-discovery adopts a group
// an administrator recreated under a different name instead of duplicating it.
func TestDiscoverOCPOperatorInstallRefreshesSavedOperatorGroupName(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	ns := cfg.NetworkOperator.Namespace
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage:         true,
		OperatorGroups: []config.OCPOperatorGroup{{Name: "old-group", Namespace: ns, TargetNamespaces: []string{ns}}},
	}
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	live := ocpTestObject("operators.coreos.com/v1", "OperatorGroup", ns, "new-site-group",
		map[string]interface{}{"spec": map[string]interface{}{"targetNamespaces": []interface{}{ns}}})
	c := fake.NewClientBuilder().WithObjects(manifest, live).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	assert.Equal(t, "new-site-group", operatorGroup(t, cfg, ns).Name, "refresh the saved group name from the cluster")
}

// TestDiscoverOCPOperatorInstallRefreshesSavedSubscriptionName verifies a saved
// Subscription name is likewise refreshed from the live namespace.
func TestDiscoverOCPOperatorInstallRefreshesSavedSubscriptionName(t *testing.T) {
	cfg := ocpInstallTestConfig(t)
	ns := cfg.NetworkOperator.Namespace
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage:    true,
		Operators: []config.OCPOperatorSubscription{{Package: "nvidia-network-operator", Name: "old-sub", Namespace: ns}},
	}
	manifest := networkOperatorManifest("nvidia-network-operator", "certified-operators", releaseChannel277)
	live := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", ns, "new-sub",
		map[string]interface{}{"spec": map[string]interface{}{"name": "nvidia-network-operator"}})
	c := fake.NewClientBuilder().WithObjects(manifest, live).Build()

	require.NoError(t, DiscoverOCPOperatorInstall(context.Background(), c, cfg, nil))
	assert.Equal(t, "new-sub", operatorSub(t, cfg, "nvidia-network-operator").Name, "refresh the saved Subscription name from the cluster")
}
