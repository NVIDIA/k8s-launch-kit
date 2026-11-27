// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func rollbackObject(api, kind string, spec map[string]interface{}) *unstructured.Unstructured {
	obj := ocpTestObject(api, kind, "operator-ns", "default", map[string]interface{}{"spec": spec})
	obj.SetUID(types.UID(kind + "-uid"))
	return obj
}
func getRollbackObject(t *testing.T, c client.Client, ref *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	obj := ref.DeepCopy()
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ref), obj))
	return obj
}
func applyRollbackConfiguration(t *testing.T, c client.Client, desired *unstructured.Unstructured, cohort string) {
	t.Helper()
	current := getRollbackObject(t, c, desired)
	before := current.DeepCopy()
	require.NoError(t, mergeOCPConfiguration(current, desired))
	require.NoError(t, recordOCPConfigurationChange(before, current, cohort, false))
	require.NoError(t, c.Update(context.Background(), current))
}

func TestOCPConfigurationRollbackCohortsAndAdditiveRedeploy(t *testing.T) {
	for _, kind := range []string{"SriovOperatorConfig", "ConfigMap"} {
		t.Run(kind, func(t *testing.T) {
			var initial, desired *unstructured.Unstructured
			switch kind {
			case "SriovOperatorConfig":
				initial = rollbackObject("sriovnetwork.openshift.io/v1", kind, map[string]interface{}{"disablePlugins": []interface{}{"site"}, "disableDrain": true, "configDaemonNodeSelector": map[string]interface{}{"site-label": "keep"}})
				desired = initial.DeepCopy()
				desired.Object["spec"] = map[string]interface{}{"disablePlugins": []interface{}{"mellanox"}, "disableDrain": false, "configDaemonNodeSelector": map[string]interface{}{"l8k-label": "managed"}}
			case "ConfigMap":
				initial = ocpTestObject("v1", kind, "openshift-multus", "cni-sysctl-allowlist", map[string]interface{}{"data": map[string]interface{}{"allowlist.conf": "# site\nsite-entry", "other": "keep"}})
				initial.SetUID("allowlist-uid")
				desired = initial.DeepCopy()
				require.NoError(t, unstructured.SetNestedField(desired.Object, "l8k-entry\n", "data", "allowlist.conf"))
			}
			c := fake.NewClientBuilder().WithObjects(initial).Build()
			applyRollbackConfiguration(t, c, desired, "cohort-a")
			// A site addition between deployments must not become part of l8k's delta.
			site := getRollbackObject(t, c, initial)
			switch kind {
			case "SriovOperatorConfig":
				require.NoError(t, unstructured.SetNestedSlice(site.Object, []interface{}{"site", "mellanox", "new-site"}, "spec", "disablePlugins"))
				require.NoError(t, unstructured.SetNestedSlice(desired.Object, []interface{}{"mellanox", "second-l8k"}, "spec", "disablePlugins"))
			case "ConfigMap":
				require.NoError(t, unstructured.SetNestedField(site.Object, "# site\nsite-entry\nl8k-entry\nnew-site\n", "data", "allowlist.conf"))
				require.NoError(t, unstructured.SetNestedField(desired.Object, "l8k-entry\nsecond-l8k\n", "data", "allowlist.conf"))
			}
			require.NoError(t, c.Update(context.Background(), site))
			applyRollbackConfiguration(t, c, desired, "cohort-b")
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort-a", false)
			require.NoError(t, err)
			require.Contains(t, messages[0], "other deployment cohort")
			beforeLast := getRollbackObject(t, c, initial)
			if kind == "SriovOperatorConfig" {
				plugins, _, _ := unstructured.NestedStringSlice(beforeLast.Object, "spec", "disablePlugins")
				require.Contains(t, plugins, "mellanox")
			}
			messages, err = restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort-b", false)
			require.NoError(t, err)
			if kind == "SriovOperatorConfig" {
				require.Empty(t, messages)
			}
			got := getRollbackObject(t, c, initial)
			require.Empty(t, got.GetAnnotations()[ocpRollbackAnnotation])
			switch kind {
			case "SriovOperatorConfig":
				plugins, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "disablePlugins")
				require.Equal(t, []string{"site", "new-site"}, plugins)
				selector, _, _ := unstructured.NestedStringMap(got.Object, "spec", "configDaemonNodeSelector")
				require.Equal(t, map[string]string{"site-label": "keep"}, selector)
				drain, _, _ := unstructured.NestedBool(got.Object, "spec", "disableDrain")
				require.True(t, drain)
			case "ConfigMap":
				data, _, _ := unstructured.NestedString(got.Object, "data", "allowlist.conf")
				require.Equal(t, "# site\nsite-entry\nnew-site\n", data)
			}
		})
	}
}

func TestOCPConfigurationRollbackExactScalarValues(t *testing.T) {
	for _, kind := range []string{"MaintenanceOperatorConfig"} {
		t.Run(kind, func(t *testing.T) {
			var initial, desired *unstructured.Unstructured
			initial = rollbackObject("maintenance.nvidia.com/v1alpha1", kind, map[string]interface{}{"maxParallelOperations": int64(0), "unrelated": false})
			desired = initial.DeepCopy()
			desired.Object["spec"] = map[string]interface{}{"maxParallelOperations": int64(2), "maxUnavailable": "50%"}
			c := fake.NewClientBuilder().WithObjects(initial).Build()
			applyRollbackConfiguration(t, c, desired, "cohort")
			// An idempotent second deploy must retain the original baseline.
			applyRollbackConfiguration(t, c, desired, "cohort")
			_, err := restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort", false)
			require.NoError(t, err)
			got := getRollbackObject(t, c, initial)
			require.Equal(t, initial.Object["spec"], got.Object["spec"])
		})
	}
}

func TestOCPConfigurationRollbackSubscriptionNamedEnvAndValueFrom(t *testing.T) {
	original := map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "valueFrom": map[string]interface{}{"configMapKeyRef": map[string]interface{}{"name": "site", "key": "enabled"}}}
	initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator", "channel": "site-channel", "config": map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "SITE", "value": "keep"}, original}}})
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true", "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": "maintenance-ns"}, false, "cohort")
	require.NoError(t, err)
	got := getRollbackObject(t, c, initial)
	env, _, _ := unstructured.NestedSlice(got.Object, "spec", "config", "env")
	env = append(env, map[string]interface{}{"name": "LATER_SITE", "value": "keep"})
	require.NoError(t, unstructured.SetNestedSlice(got.Object, env, "spec", "config", "env"))
	require.NoError(t, c.Update(context.Background(), got))
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
	require.NoError(t, err)
	got = getRollbackObject(t, c, initial)
	env, _, _ = unstructured.NestedSlice(got.Object, "spec", "config", "env")
	require.Equal(t, []interface{}{map[string]interface{}{"name": "SITE", "value": "keep"}, original, map[string]interface{}{"name": "LATER_SITE", "value": "keep"}}, env)
}

func TestOCPConfigurationRollbackLegacyAndChangedFieldsRetained(t *testing.T) {
	initial := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "10%"})
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	require.Contains(t, messages[0], "no recorded prior values")
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, "50%", "spec", "maxUnavailable"))
	applyRollbackConfiguration(t, c, desired, "cohort")
	changed := getRollbackObject(t, c, initial)
	require.NoError(t, unstructured.SetNestedField(changed.Object, "75%", "spec", "maxUnavailable"))
	require.NoError(t, c.Update(context.Background(), changed))
	messages, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	require.Contains(t, messages[0], "changed after deployment")
	got := getRollbackObject(t, c, initial)
	value, _, _ := unstructured.NestedString(got.Object, "spec", "maxUnavailable")
	require.Equal(t, "75%", value)
	require.NotEmpty(t, got.GetAnnotations()[ocpRollbackAnnotation])
	// The unchanged journal supports retry after its owner resolves the conflict.
	require.NoError(t, unstructured.SetNestedField(got.Object, "50%", "spec", "maxUnavailable"))
	require.NoError(t, c.Update(context.Background(), got))
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	got = getRollbackObject(t, c, initial)
	value, _, _ = unstructured.NestedString(got.Object, "spec", "maxUnavailable")
	require.Equal(t, "10%", value)
}

func TestOCPConfigurationRollbackCreatedObjectAndReplay(t *testing.T) {
	for _, mode := range []string{"unchanged", "site-change", "copied-journal", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			desired := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "50%"})
			created := desired.DeepCopy()
			require.NoError(t, recordOCPConfigurationChange(desired, created, "cohort", true))
			c := fake.NewClientBuilder().WithObjects(created).Build()
			if mode != "unbound" {
				live := getRollbackObject(t, c, created)
				require.NoError(t, bindOCPCreatedConfiguration(context.Background(), c, live, false))
			}
			live := getRollbackObject(t, c, created)
			if mode == "site-change" {
				live.SetLabels(map[string]string{"site": "keep"})
				require.NoError(t, c.Update(context.Background(), live))
			}
			if mode == "copied-journal" {
				live.SetUID("replacement-uid")
				require.NoError(t, c.Update(context.Background(), live))
			}
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort", false)
			if mode == "copied-journal" {
				require.ErrorContains(t, err, "different object UID")
			} else {
				require.NoError(t, err)
			}
			got := desired.DeepCopy()
			err = c.Get(context.Background(), client.ObjectKeyFromObject(desired), got)
			if mode == "unchanged" {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				if mode != "copied-journal" {
					require.NotEmpty(t, messages)
				}
			}
		})
	}
}

func TestOCPConfigurationRollbackConflictRechecksLiveState(t *testing.T) {
	initial := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "10%"})
	attempts := 0
	c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		attempts++
		data, err := patch.Data(obj)
		require.NoError(t, err)
		require.Contains(t, string(data), "resourceVersion")
		if attempts == 1 {
			live := getRollbackObject(t, c, initial)
			require.NoError(t, unstructured.SetNestedField(live.Object, "site", "spec", "newSiteField"))
			require.NoError(t, c.Update(ctx, live))
			return apierrors.NewConflict(schema.GroupResource{Group: "maintenance.nvidia.com", Resource: "maintenanceoperatorconfigs"}, obj.GetName(), errors.New("site write"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}).Build()
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, "50%", "spec", "maxUnavailable"))
	applyRollbackConfiguration(t, c, desired, "cohort")
	_, err := restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort", false)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	got := getRollbackObject(t, c, initial)
	value, _, _ := unstructured.NestedString(got.Object, "spec", "newSiteField")
	require.Equal(t, "site", value)
}

func TestOCPConfigurationRollbackPreflightFailsBeforeMutation(t *testing.T) {
	initial := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "10%"})
	initial.SetAnnotations(map[string]string{ocpRollbackAnnotation: `{"version":999,"cohorts":["cohort"]}`})
	writes := 0
	c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		writes++
		return nil
	}, Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		writes++
		return nil
	}}).Build()
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	_, err := preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, []*unstructured.Unstructured{initial}, "cohort")
	require.ErrorContains(t, err, "invalid OpenShift rollback provenance")
	require.Zero(t, writes)
}

func TestOCPConfigurationRollbackSubscriptionWaitRetainsProofAndRetries(t *testing.T) {
	initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator", "config": map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"}}}})
	initial.Object["status"] = map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0"}
	// The CSV default remains when l8k's namespace override is removed.
	baseline := []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"}, map[string]interface{}{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": "csv-default"}}
	csv := ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "operator-ns", "nvidia-network-operator.v26.7.0", map[string]interface{}{"spec": map[string]interface{}{"install": map[string]interface{}{"spec": map[string]interface{}{"deployments": []interface{}{map[string]interface{}{"name": "network-operator", "spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "operator", "env": baseline}}}}}}}}}}})
	deployment := ocpTestObject("apps/v1", "Deployment", "operator-ns", "network-operator", map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1), "template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "operator", "env": []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "true"}, map[string]interface{}{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": "l8k-maintenance"}}}}}}}, "status": map[string]interface{}{"observedGeneration": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1), "replicas": int64(1)}})
	deployment.SetGeneration(1)
	c := fake.NewClientBuilder().WithObjects(initial, csv, deployment).Build()
	desired := map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true", "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": "l8k-maintenance"}
	_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", desired, false, "cohort")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = restoreLiveOCPConfiguration(ctx, c, initial, "cohort", false)
	require.ErrorContains(t, err, "restoration rollout unverified")
	live := getRollbackObject(t, c, initial)
	journal, err := readOCPConfigurationJournal(live)
	require.NoError(t, err)
	require.NotEmpty(t, journal.RestoreEnv)
	require.Empty(t, journal.Fields)
	// A deployment cannot steal a journal whose prior rollback is incomplete.
	_, err = mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", desired, false, "other-cohort")
	require.ErrorContains(t, err, "restoration is pending")
	ready := getRollbackObject(t, c, deployment)
	require.NoError(t, unstructured.SetNestedSlice(ready.Object, []interface{}{map[string]interface{}{"name": "operator", "env": baseline}}, "spec", "template", "spec", "containers"))
	require.NoError(t, c.Update(context.Background(), ready))
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	live = getRollbackObject(t, c, initial)
	require.Empty(t, live.GetAnnotations()[ocpRollbackAnnotation])
}

func TestOCPConfigurationRollbackWaitsForCreatedObjectFinalizers(t *testing.T) {
	for _, skipWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "skip-controller-waits"}[skipWait], func(t *testing.T) {
			created := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "50%"})
			created.SetFinalizers([]string{"site.example/finalizer"})
			desired := created.DeepCopy()
			require.NoError(t, recordOCPConfigurationChange(desired, created, "cohort", true))
			c := fake.NewClientBuilder().WithObjects(created).Build()
			live := getRollbackObject(t, c, created)
			require.NoError(t, bindOCPCreatedConfiguration(context.Background(), c, live, false))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := restoreLiveOCPConfiguration(ctx, c, desired, "cohort", skipWait)
			require.ErrorContains(t, err, "finalizers")
			live = getRollbackObject(t, c, created)
			require.NotNil(t, live.GetDeletionTimestamp())
			require.NotEmpty(t, live.GetAnnotations()[ocpRollbackAnnotation])
			live.SetFinalizers(nil)
			require.NoError(t, c.Update(context.Background(), live))
			_, err = restoreLiveOCPConfiguration(context.Background(), c, desired, "cohort", skipWait)
			require.NoError(t, err)
		})
	}
}

func TestOCPConfigurationRollbackLeavesUnselectedSysctlArtifact(t *testing.T) {
	initial := ocpTestObject("v1", "ConfigMap", "openshift-multus", "cni-sysctl-allowlist", map[string]interface{}{"data": map[string]interface{}{"allowlist.conf": "site-entry"}})
	initial.SetUID("allowlist-uid")
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, "l8k-entry\n", "data", "allowlist.conf"))
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	// Simulate a deployment made before manifest-based allowlist cleanup.
	applyRollbackConfiguration(t, c, desired, "cohort")
	before := getRollbackObject(t, c, initial)
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	objects, err := preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, nil, "cohort")
	require.NoError(t, err)
	require.Empty(t, objects, "an omitted patch cannot authorize removal of previous entries")
	require.Equal(t, before.Object, getRollbackObject(t, c, initial).Object)
}

func TestOCPConfigurationRollbackRejectsChangedSharedIdentity(t *testing.T) {
	initial := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "10%"})
	initial.SetNamespace("old-maintenance-ns")
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, "50%", "spec", "maxUnavailable"))
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	applyRollbackConfiguration(t, c, desired, "cohort")
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}, Maintenance: &config.MaintenanceConfig{OperatorNamespace: "new-maintenance-ns"}}
	require.ErrorContains(t, checkOCPConfigurationMapping(context.Background(), c, cfg, "cohort"), "use its prior retained bundle")
	err := applyOCPOperatorConfigurationForCohort(context.Background(), c, cfg, nil, false, "cohort")
	require.ErrorContains(t, err, "old-maintenance-ns/default")
	require.NoError(t, checkOCPConfigurationMapping(context.Background(), c, cfg, "different-cohort"))
}

func TestOCPConfigurationRollbackPreservesAbsentSubscriptionEnv(t *testing.T) {
	for _, existing := range []map[string]interface{}{{"name": "nvidia-network-operator"}, {"name": "nvidia-network-operator", "config": map[string]interface{}{}}, {"name": "nvidia-network-operator", "config": map[string]interface{}{"env": []interface{}{}}}} {
		initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", existing)
		c := fake.NewClientBuilder().WithObjects(initial).Build()
		_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true"}, false, "cohort")
		require.NoError(t, err)
		_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
		require.NoError(t, err)
		got := getRollbackObject(t, c, initial)
		require.Equal(t, initial.Object["spec"], got.Object["spec"])
	}
}

func TestOCPConfigurationRollbackRejectsLostSubscriptionWaitProof(t *testing.T) {
	for _, mode := range []string{"removed-marker", "replaced-object", "changed-claim"} {
		t.Run(mode, func(t *testing.T) {
			initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator"})
			journal := &ocpConfigurationJournal{Version: 1, UID: string(initial.GetUID()), Cohorts: []string{"cohort"}, RestoreEnv: map[string]ocpConfigurationValue{"MAINTENANCE_OPERATOR_ENABLED": {}}}
			require.NoError(t, writeOCPConfigurationJournal(initial, journal))
			live := initial.DeepCopy()
			switch mode {
			case "removed-marker":
				live.SetAnnotations(nil)
			case "replaced-object":
				live.SetUID("new-uid")
			case "changed-claim":
				journal.Cohorts = []string{"other"}
				require.NoError(t, writeOCPConfigurationJournal(live, journal))
			}
			c := fake.NewClientBuilder().WithObjects(live).Build()
			err := completeOCPSubscriptionRestore(context.Background(), c, initial, "cohort")
			require.ErrorContains(t, err, "restoration rollout unverified")
		})
	}
}

func TestOCPConfigurationRollbackCustomSharedSpecFieldsRemainSupported(t *testing.T) {
	initial := rollbackObject("sriovnetwork.openshift.io/v1", "SriovOperatorConfig", map[string]interface{}{"enableInjector": false})
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, true, "spec", "enableInjector"))
	sub := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator"})
	// No installed CSV: configuration writes complete, then rollout observation
	// reports this separate missing prerequisite. This is a real (non dry-run) write.
	c := fake.NewClientBuilder().WithObjects(initial, sub).Build()
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}, Sriov: &config.SriovConfig{OperatorNamespace: "operator-ns"}}
	err := applyOCPOperatorConfigurationForCohort(context.Background(), c, cfg, []*unstructured.Unstructured{desired}, false, "cohort")
	require.ErrorContains(t, err, "installedCSV")
	got := getRollbackObject(t, c, initial)
	enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "enableInjector")
	require.True(t, enabled)
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	got = getRollbackObject(t, c, initial)
	enabled, _, _ = unstructured.NestedBool(got.Object, "spec", "enableInjector")
	require.False(t, enabled)
}

// A standalone restore test uses the same live snapshot that the command
// would have approved during read-only preflight.
func restoreLiveOCPConfiguration(ctx context.Context, c client.Client, target *unstructured.Unstructured, cohort string, skipWait bool) ([]string, error) {
	live := target.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(target), live); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return restoreOCPConfigurationObject(ctx, c, live, cohort, skipWait)
}

func TestOCPConfigurationRollbackRetainsForManualPolicyOnDistinctPF(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing-config", true: "created-config"}[created], func(t *testing.T) {
			initial := rollbackObject("sriovnetwork.openshift.io/v1", "SriovOperatorConfig", map[string]interface{}{"disablePlugins": []interface{}{"site"}})
			desired := initial.DeepCopy()
			require.NoError(t, unstructured.SetNestedSlice(desired.Object, []interface{}{"mellanox"}, "spec", "disablePlugins"))
			// This unclaimed community-operator policy uses a PF outside the selected
			// deployment. PF reset isolation alone does not protect shared config.
			manual := rollbackObject("sriovnetwork.openshift.io/v1", "SriovNetworkNodePolicy", map[string]interface{}{"numVfs": int64(4), "nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:02:00.0"}}})
			manual.SetName("manual-distinct-pf")
			platform := rollbackObject("sriovnetwork.openshift.io/v1", "SriovNetworkNodePolicy", map[string]interface{}{"numVfs": int64(0), "nicSelector": map[string]interface{}{}})
			c := fake.NewClientBuilder().WithObjects(initial, manual, platform).Build()
			if created {
				current := getRollbackObject(t, c, initial)
				before := current.DeepCopy()
				require.NoError(t, mergeOCPConfiguration(current, desired))
				require.NoError(t, recordOCPConfigurationChange(before, current, "cohort", true))
				require.NoError(t, c.Update(context.Background(), current))
				require.NoError(t, bindOCPCreatedConfiguration(context.Background(), c, current, false))
			} else {
				applyRollbackConfiguration(t, c, desired, "cohort")
			}
			before := getRollbackObject(t, c, initial)
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
			require.NoError(t, err)
			require.Contains(t, messages[0], "manual-distinct-pf")
			got := getRollbackObject(t, c, initial)
			require.Equal(t, before.Object, got.Object, "consumer protection preserves configuration and provenance")
			require.NoError(t, c.Delete(context.Background(), manual))
			_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
			require.NoError(t, err)
			if created {
				got = initial.DeepCopy()
				require.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(initial), got)))
			} else {
				got = getRollbackObject(t, c, initial)
				plugins, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "disablePlugins")
				require.Equal(t, []string{"site"}, plugins)
			}
		})
	}
}

func TestOCPConfigurationRollbackRetainsKnownMaintenanceConsumers(t *testing.T) {
	for _, kind := range []string{"MaintenanceOperatorConfig"} {
		t.Run(kind, func(t *testing.T) {
			var initial, desired *unstructured.Unstructured
			initial = rollbackObject("maintenance.nvidia.com/v1alpha1", kind, map[string]interface{}{"maxUnavailable": "10%"})
			desired = initial.DeepCopy()
			require.NoError(t, unstructured.SetNestedField(desired.Object, "50%", "spec", "maxUnavailable"))
			manual := rollbackObject("mellanox.com/v1alpha1", "NicClusterPolicy", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "site-driver"}})
			manual.SetNamespace("")
			manual.SetName("site-root")
			c := fake.NewClientBuilder().WithObjects(initial, manual).Build()
			applyRollbackConfiguration(t, c, desired, "cohort")
			before := getRollbackObject(t, c, initial)
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
			require.NoError(t, err)
			require.Contains(t, messages[0], "site-root")
			require.Equal(t, before.Object, getRollbackObject(t, c, initial).Object)
			require.NoError(t, c.Delete(context.Background(), manual))
			_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
			require.NoError(t, err)
			got := getRollbackObject(t, c, initial)
			require.Equal(t, initial.Object["spec"], got.Object["spec"])
		})
	}
}

func TestOCPConfigurationRollbackRetainsActiveNodeMaintenance(t *testing.T) {
	initial := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "10%"})
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, "50%", "spec", "maxUnavailable"))
	request := rollbackObject("maintenance.nvidia.com/v1alpha1", "NodeMaintenance", map[string]interface{}{"nodeName": "site-worker", "requestorID": "site.example"})
	request.SetName("site-request")
	c := fake.NewClientBuilder().WithObjects(initial, request).Build()
	applyRollbackConfiguration(t, c, desired, "cohort")
	before := getRollbackObject(t, c, initial)
	messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
	require.Contains(t, messages[0], "site-request")
	require.Equal(t, before.Object, getRollbackObject(t, c, initial).Object)
	require.NoError(t, c.Delete(context.Background(), request))
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.NoError(t, err)
}

func TestOCPConfigurationRollbackConsumerInspectionDenied(t *testing.T) {
	initial := rollbackObject("sriovnetwork.openshift.io/v1", "SriovOperatorConfig", map[string]interface{}{"disableDrain": true})
	desired := initial.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(desired.Object, false, "spec", "disableDrain"))
	c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if list.GetObjectKind().GroupVersionKind().Kind == "SriovNetworkNodePolicyList" {
			return apierrors.NewForbidden(schema.GroupResource{Group: "sriovnetwork.openshift.io", Resource: "sriovnetworknodepolicies"}, "", errors.New("denied"))
		}
		return c.List(ctx, list, opts...)
	}}).Build()
	applyRollbackConfiguration(t, c, desired, "cohort")
	before := getRollbackObject(t, c, initial)
	_, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
	require.ErrorContains(t, err, "denied")
	require.Equal(t, before.Object, getRollbackObject(t, c, initial).Object)
}

func TestOCPConfigurationRollbackCreatedUIDBindingRetriesWithoutAdoptingSiteChanges(t *testing.T) {
	for _, mode := range []string{"status-update", "site-label"} {
		t.Run(mode, func(t *testing.T) {
			created := rollbackObject("maintenance.nvidia.com/v1alpha1", "MaintenanceOperatorConfig", map[string]interface{}{"maxUnavailable": "50%"})
			before := created.DeepCopy()
			require.NoError(t, recordOCPConfigurationChange(before, created, "cohort", true))
			attempts := 0
			c := fake.NewClientBuilder().WithObjects(created).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				attempts++
				if attempts == 1 {
					current := created.DeepCopy()
					if err := c.Get(ctx, client.ObjectKeyFromObject(created), current); err != nil {
						return err
					}
					if mode == "site-label" {
						current.SetLabels(map[string]string{"site": "keep"})
					} else {
						current.Object["status"] = map[string]interface{}{"controller": "observed"}
					}
					if err := c.Update(ctx, current); err != nil {
						return err
					}
					return apierrors.NewConflict(schema.GroupResource{Group: "maintenance.nvidia.com", Resource: "maintenanceoperatorconfigs"}, obj.GetName(), errors.New("controller write"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			response := getRollbackObject(t, c, created)
			require.NoError(t, bindOCPCreatedConfiguration(context.Background(), c, response, false))
			require.Equal(t, 2, attempts)
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, created, "cohort", false)
			require.NoError(t, err)
			got := created.DeepCopy()
			err = c.Get(context.Background(), client.ObjectKeyFromObject(created), got)
			if mode == "site-label" {
				require.NoError(t, err)
				require.Contains(t, messages[0], "subsequent changes")
				require.Equal(t, "keep", got.GetLabels()["site"])
			} else {
				require.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

func TestOCPConfigurationRollbackPreflightAllowsUnservedDesiredAPI(t *testing.T) {
	// A previous managed-operator cleanup may have removed its served CRD.
	desired := rollbackObject("sriovnetwork.openshift.io/v1", "SriovOperatorConfig", map[string]interface{}{"disableDrain": false})
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == "SriovOperatorConfig" {
				return &meta.NoKindMatchError{GroupKind: desired.GroupVersionKind().GroupKind(), SearchedVersions: []string{"v1"}}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	targets, err := preflightOCPOperatorConfigurationRestore(context.Background(), c, cfg, []*unstructured.Unstructured{desired}, "cohort")
	require.NoError(t, err)
	require.Empty(t, targets)
}
