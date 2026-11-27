// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ocpTestObject(apiVersion, kind, namespace, name string, extra map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
	for key, value := range extra {
		obj.Object[key] = value
	}
	return obj
}

func TestCheckOCPOperatorsRequiresInstalledCSVAndServedAPI(t *testing.T) {
	ctx := context.Background()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{
		Namespace: "operator-ns", SelectedRelease: "26.7",
	}}
	sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-ns", "nvidia-network-operator", map[string]interface{}{
		"spec":   map[string]interface{}{"name": "nvidia-network-operator"},
		"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0-1"},
	})
	csv := ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "operator-ns", "nvidia-network-operator.v26.7.0-1", map[string]interface{}{
		"spec":   map[string]interface{}{"version": "26.7.0-1"},
		"status": map[string]interface{}{"phase": "Succeeded"},
	})
	crd := ocpTestObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "nicclusterpolicies.mellanox.com", map[string]interface{}{
		"spec":   map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v1alpha1", "served": true}}},
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}},
	})
	newClient := func(objects ...client.Object) client.Client {
		return fake.NewClientBuilder().WithObjects(objects...).Build()
	}
	require.NoError(t, CheckOCPOperators(ctx, newClient(sub, csv, crd), cfg, false))
	customSub := sub.DeepCopy()
	customSub.SetName("custom-network-sub")
	require.NoError(t, CheckOCPOperators(ctx, newClient(customSub, csv, crd), cfg, false))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(crd), cfg, false), "not installed")
	err := CheckOCPOperators(ctx, newClient(sub, csv), cfg, false)
	require.ErrorContains(t, err, "required API")
	require.True(t, apierrors.IsNotFound(err), "live preflight preserves wrapped API errors")

	wrongSub := sub.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(wrongSub.Object, "wrong-operator", "spec", "name"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(wrongSub, csv, crd), cfg, false), "expected")

	failedCSV := csv.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(failedCSV.Object, "Failed", "status", "phase"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, failedCSV, crd), cfg, false), "expected Succeeded")

	unserved := crd.DeepCopy()
	require.NoError(t, unstructured.SetNestedSlice(unserved.Object, []interface{}{map[string]interface{}{"name": "v1alpha1", "served": false}}, "spec", "versions"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, csv, unserved), cfg, false), "not served")

	wrongVersion := *cfg
	operatorCfg := *cfg.NetworkOperator
	operatorCfg.SelectedRelease = "26.4"
	wrongVersion.NetworkOperator = &operatorCfg
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, csv, crd), &wrongVersion, false), "does not match")
}

func readyOCPOperatorObjects(op ocpOperator, version string) []client.Object {
	csvName := op.packageName + ".v" + version
	return []client.Object{
		ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", op.namespace, op.packageName, map[string]interface{}{
			"spec": map[string]interface{}{"name": op.packageName}, "status": map[string]interface{}{"installedCSV": csvName},
		}),
		ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", op.namespace, csvName, map[string]interface{}{
			"spec": map[string]interface{}{"version": version}, "status": map[string]interface{}{"phase": "Succeeded"},
		}),
		ocpTestObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", op.api, map[string]interface{}{
			"spec":   map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v1", "served": true}}},
			"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}},
		}),
	}
}

func TestDiscoverOCPOperatorsSeparatesPrerequisitesFromInspectionErrors(t *testing.T) {
	for _, tc := range []struct {
		mode, issue string
		failure     bool
	}{
		{mode: "healthy"},
		{mode: "CSV without Subscription"},
		{mode: "original and copied CSV"},
		{mode: "copied CSV only", issue: "not installed"},
		{mode: "Subscription references copied CSV", issue: "copied from another namespace"},
		{mode: "missing all", issue: "not installed"},
		{mode: "pending subscription", issue: "no installedCSV"},
		{mode: "missing CSV", issue: "read nvidia-network-operator CSV"},
		{mode: "failed CSV", issue: "expected Succeeded"},
		{mode: "missing API", issue: "required API"},
		{mode: "unserved API", issue: "not served and established"},
		{mode: "unestablished API", issue: "not served and established"},
		{mode: "absent OLM API", issue: "no matches"},
		{mode: "malformed CSV", issue: "expected Succeeded"},
		{mode: "malformed API", issue: "not served and established"},
		{mode: "forbidden", failure: true},
		{mode: "transport", failure: true},
		{mode: "ambiguous subscriptions", failure: true},
		{mode: "ambiguous CSVs", failure: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx := context.Background()
			cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns", SelectedRelease: "26.7"}, Profile: &config.Profile{Deployment: "sriov"}}
			var objects []client.Object
			for _, op := range requiredOCPOperators(cfg, true) {
				objects = append(objects, readyOCPOperatorObjects(op, "26.7.0-1")...)
			}
			sub, csv, crd := objects[0].(*unstructured.Unstructured), objects[1].(*unstructured.Unstructured), objects[2].(*unstructured.Unstructured)
			switch tc.mode {
			case "missing all":
				objects = nil
			case "CSV without Subscription":
				objects = objects[1:]
			case "original and copied CSV":
				peer := csv.DeepCopy()
				peer.SetName("nvidia-network-operator.v26.7.1")
				peer.SetLabels(map[string]string{"olm.copiedFrom": "original-namespace"})
				objects = append(objects[1:], peer)
			case "copied CSV only":
				csv.SetAnnotations(map[string]string{"olm.copiedFrom": "original-namespace"})
				objects = objects[1:]
			case "Subscription references copied CSV":
				csv.SetLabels(map[string]string{"olm.copiedFrom": "original-namespace"})
			case "pending subscription":
				unstructured.RemoveNestedField(sub.Object, "status", "installedCSV")
			case "missing CSV":
				objects = append(objects[:1], objects[2:]...)
			case "failed CSV":
				require.NoError(t, unstructured.SetNestedField(csv.Object, "Failed", "status", "phase"))
			case "missing API":
				objects = append(objects[:2], objects[3:]...)
			case "unserved API":
				require.NoError(t, unstructured.SetNestedSlice(crd.Object, []interface{}{map[string]interface{}{"name": "v1", "served": false}}, "spec", "versions"))
			case "unestablished API":
				require.NoError(t, unstructured.SetNestedSlice(crd.Object, []interface{}{map[string]interface{}{"type": "Established", "status": "False"}}, "status", "conditions"))
			case "malformed CSV":
				require.NoError(t, unstructured.SetNestedField(csv.Object, int64(1), "status", "phase"))
			case "ambiguous subscriptions":
				peer := sub.DeepCopy()
				sub.SetName("site-one")
				peer.SetName("site-two")
				objects = append(objects, peer)
			case "ambiguous CSVs":
				peer := csv.DeepCopy()
				peer.SetName("nvidia-network-operator.v26.7.1")
				objects = append(objects[1:], peer)
			}
			c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == "Subscription" && key.Namespace == "operator-ns" {
						switch tc.mode {
						case "absent OLM API":
							return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "operators.coreos.com", Kind: "Subscription"}}
						case "forbidden":
							return apierrors.NewForbidden(schema.GroupResource{Group: "operators.coreos.com", Resource: "subscriptions"}, key.Name, errors.New("denied"))
						case "transport":
							return errors.New("connection unavailable")
						}
					}
					err := c.Get(ctx, key, obj, opts...)
					if err == nil && tc.mode == "malformed API" && obj.GetObjectKind().GroupVersionKind().Kind == "CustomResourceDefinition" {
						require.NoError(t, unstructured.SetNestedField(obj.(*unstructured.Unstructured).Object, "invalid", "spec", "versions"))
					}
					return err
				},
			}).Build()
			observations, err := DiscoverOCPOperators(ctx, c, cfg)
			if tc.failure {
				require.Error(t, err)
				require.Nil(t, observations)
				return
			}
			require.NoError(t, err)
			require.Len(t, observations, 4)
			require.Equal(t, "nvidia-network-operator", observations[0].Package)
			require.Equal(t, "operator-ns", observations[0].Namespace)
			switch tc.mode {
			case "missing all", "pending subscription", "missing CSV", "absent OLM API", "copied CSV only", "Subscription references copied CSV":
				require.Empty(t, observations[0].CSV)
			default:
				require.Equal(t, "nvidia-network-operator.v26.7.0-1", observations[0].CSV)
			}
			if tc.issue == "" {
				require.Empty(t, MissingOCPOperatorPrerequisites(&config.LaunchKitConfig{Flavor: cfg.Flavor, NetworkOperator: cfg.NetworkOperator, Profile: cfg.Profile, DiscoveredOCPOperators: observations}))
				require.Equal(t, "26.7.0-1", observations[0].Version)
			} else {
				require.Contains(t, observations[0].Issue, tc.issue)
			}
			require.Nil(t, cfg.DiscoveredOCPOperators, "observation must not mutate the input")
			require.Nil(t, cfg.OCPOperatorInstall, "inspection must not enable operator management")
		})
	}
}

func TestDiscoverOCPOperatorsInspectsOnlyEffectiveRequirements(t *testing.T) {
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns", SelectedRelease: "26.7"}, Profile: &config.Profile{Deployment: "rdma_shared"}, Sriov: &config.SriovConfig{OperatorNamespace: "unused-sriov-ns"}}
	var objects []client.Object
	for _, op := range requiredOCPOperators(cfg, true) {
		objects = append(objects, readyOCPOperatorObjects(op, "26.7.0")...)
	}
	c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			require.NotEqual(t, "unused-sriov-ns", key.Namespace)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	_, err := DiscoverOCPOperators(context.Background(), c, nil)
	require.ErrorContains(t, err, "requires a cluster config")
	_, err = DiscoverOCPOperators(context.Background(), nil, cfg)
	require.ErrorContains(t, err, "requires context and client")
	observations, err := DiscoverOCPOperators(context.Background(), c, cfg)
	require.NoError(t, err)
	require.Len(t, observations, 3)
	cfg.Flavor = config.FlavorK8s
	observations, err = DiscoverOCPOperators(context.Background(), nil, cfg)
	require.NoError(t, err)
	require.Nil(t, observations)
}

func TestMissingOCPOperatorPrerequisitesUsesCurrentObservedIdentityAndRelease(t *testing.T) {
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns", SelectedRelease: "26.7"}, Profile: &config.Profile{Deployment: "rdma_shared"}}
	require.Nil(t, MissingOCPOperatorPrerequisites(nil))
	require.Nil(t, MissingOCPOperatorPrerequisites(cfg), "legacy/offline inputs without observations remain valid")
	cfg.DiscoveredOCPOperators = []config.DiscoveredOCPOperator{
		{Package: "nvidia-network-operator", Namespace: "operator-ns", Version: "26.4.0", Issue: ""},
		{Package: "sriov-network-operator", Namespace: config.DefaultSriovOperatorNamespace, Version: "", Issue: "OpenShift sriov-network-operator is not installed in " + config.DefaultSriovOperatorNamespace},
		{Package: "nfd", Namespace: "other-nfd-ns", Version: "", Issue: "not installed"},
	}
	issues := MissingOCPOperatorPrerequisites(cfg)
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], "does not match selected release")
	cfg.NetworkOperator.SelectedRelease = "26.4"
	require.Nil(t, MissingOCPOperatorPrerequisites(cfg), "version compatibility is recomputed, not stored as a stale failure")
	cfg.Profile.Deployment = "sriov"
	issues = MissingOCPOperatorPrerequisites(cfg)
	require.Len(t, issues, 1)
	require.Contains(t, issues[0], "sriov-network-operator")
	cfg.Sriov = &config.SriovConfig{OperatorNamespace: "new-sriov-ns"}
	require.Nil(t, MissingOCPOperatorPrerequisites(cfg), "a changed namespace has no matching observation")
	copy, err := config.CloneConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.DiscoveredOCPOperators, copy.DiscoveredOCPOperators)
	cfg.Flavor = config.FlavorK8s
	require.Nil(t, MissingOCPOperatorPrerequisites(cfg))
}

func TestOpenShiftPluginDiscoveryWithoutInstalledOperatorsReachesHardwareDiscovery(t *testing.T) {
	listedNodes := false
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.NodeList); ok {
				listedNodes = true
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns", SelectedRelease: "26.7"}}
	err := (&NetworkOperatorPlugin{}).DiscoverClusterConfig(context.Background(), c, cfg)
	require.ErrorContains(t, err, "no Ready schedulable nodes available for discovery")
	require.True(t, listedNodes, "an empty OpenShift cluster must reach hardware discovery without an OLM prerequisite gate")
}
