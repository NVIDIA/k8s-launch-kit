// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var _ = ginkgo.Describe("OpenShift skip-wait shared rollback", func() {
	ginkgo.It("can repeat waived cleanup and configure a later deployment without upgrading hardware proof", func() {
		ctx := context.Background()
		cfg, artifacts, root := ocpCleanFixture()
		owner, err := ocpBundleOwner(artifacts, cfg.NetworkOperator.Namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		baseline := []interface{}{
			map[string]interface{}{"name": "SITE", "value": "keep"},
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "valueFrom": map[string]interface{}{"configMapKeyRef": map[string]interface{}{"name": "site", "key": "enabled"}}},
		}
		sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-system", "nvidia-network-operator", map[string]interface{}{
			"spec":   map[string]interface{}{"name": "nvidia-network-operator", "channel": "site-channel", "config": map[string]interface{}{"env": baseline, "nodeSelector": map[string]interface{}{"site": "keep"}}},
			"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0"},
		})
		sub.SetUID("subscription-uid")
		sub.SetAnnotations(map[string]string{"site": "keep"})
		activeEnv := []interface{}{
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "true"},
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": config.DefaultMaintenanceOperatorNamespace},
		}
		csv := ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "operator-system", "nvidia-network-operator.v26.7.0", map[string]interface{}{
			"spec": map[string]interface{}{"install": map[string]interface{}{"spec": map[string]interface{}{"deployments": []interface{}{map[string]interface{}{"name": "network-operator"}}}}},
		})
		// Keep the existing deployed env: skip-wait must not require OLM to adopt
		// the restored baseline. The next deployment checks its own desired env.
		deployment := ocpTestObject("apps/v1", "Deployment", "operator-system", "network-operator", map[string]interface{}{
			"spec":   map[string]interface{}{"replicas": int64(1), "template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "operator", "env": activeEnv}}}}},
			"status": map[string]interface{}{"observedGeneration": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1), "replicas": int64(1)},
		})
		deployment.SetGeneration(1)
		c := fake.NewClientBuilder().WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{Create: policyTestCreate}).WithObjects(root, sub, csv, deployment).Build()
		_, err = mergeSubscriptionEnvChangedForCohort(ctx, c, "operator-system", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true", "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": config.DefaultMaintenanceOperatorNamespace}, false, owner)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		opts := CleanOptions{SkipWait: true, PollInterval: time.Millisecond, ResourceTimeout: time.Second}
		for attempt := 0; attempt < 2; attempt++ {
			plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			result, err := plan.Execute(ctx, opts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if attempt == 0 {
				gomega.Expect(result.Retained).To(gomega.ContainElement(gomega.ContainSubstring("waived rollout verification")))
			}
			live := sub.DeepCopy()
			gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(sub), live)).To(gomega.Succeed())
			gomega.Expect(live.Object["spec"]).To(gomega.Equal(sub.Object["spec"]))
			gomega.Expect(live.GetAnnotations()).To(gomega.Equal(sub.GetAnnotations()))
		}
		receipt := &corev1.ConfigMap{}
		receiptKey := client.ObjectKey{Namespace: "operator-system", Name: ocpCleanReceiptName(owner)}
		gomega.Expect(c.Get(ctx, receiptKey, receipt)).To(gomega.Succeed())
		gomega.Expect(receipt.Data["state"]).To(gomega.Equal("complete-unverified"))
		// A default retry must still refuse missing historical workload evidence.
		plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = plan.Execute(ctx, CleanOptions{PollInterval: time.Millisecond, ResourceTimeout: time.Second})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("without a prior verified root UID")))
		gomega.Expect(c.Get(ctx, receiptKey, receipt)).To(gomega.Succeed())
		gomega.Expect(receipt.Data["state"]).To(gomega.Equal("complete-unverified"))

		// Exercise the same receipt invalidation and shared configuration stage used
		// by deploy, including its ordinary Subscription/Deployment rollout check.
		policy := openShiftDeploymentPolicy{}
		selected, err := policy.prepare(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(prepareOCPCleanDeployment(ctx, c, cfg, artifacts, &selected)).To(gomega.Succeed())
		release, err := acquireOCPOperation(ctx, c, "operator-system", owner, "deploy")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() { gomega.Expect(release()).To(gomega.Succeed()) }()
		gomega.Expect(policy.configureAfterPreflight(ctx, c, cfg, selected, false)).To(gomega.Succeed())
		gomega.Expect(apierrors.IsNotFound(c.Get(ctx, receiptKey, receipt))).To(gomega.BeTrue())
		live := sub.DeepCopy()
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(sub), live)).To(gomega.Succeed())
		journal, err := readOCPConfigurationJournal(live)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(journal).NotTo(gomega.BeNil())
		gomega.Expect(journal.RestoreEnv).To(gomega.BeEmpty())
		gomega.Expect(live.GetAnnotations()["site"]).To(gomega.Equal("keep"))
		selector, _, err := unstructured.NestedStringMap(live.Object, "spec", "config", "nodeSelector")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(selector).To(gomega.Equal(map[string]string{"site": "keep"}))
	})
})

func TestOCPConfigurationRollbackSkipWaitReleasesPendingRolloutSafely(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-baseline", true: "changed-baseline"}[drift], func(t *testing.T) {
			initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator", "config": map[string]interface{}{"nodeSelector": map[string]interface{}{"site": "keep"}, "env": []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"}, map[string]interface{}{"name": "SITE", "value": "keep"}}}})
			c := fake.NewClientBuilder().WithObjects(initial).Build()
			_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true"}, false, "cohort")
			require.NoError(t, err)
			// A normal failed rollout retains its marker and restored env for retry.
			_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", false)
			require.ErrorContains(t, err, "restoration rollout unverified")
			pending := getRollbackObject(t, c, initial)
			journal, err := readOCPConfigurationJournal(pending)
			require.NoError(t, err)
			require.NotEmpty(t, journal.RestoreEnv)
			if drift {
				require.NoError(t, unstructured.SetNestedSlice(pending.Object, []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "site-change"}, map[string]interface{}{"name": "SITE", "value": "keep"}}, "spec", "config", "env"))
				require.NoError(t, c.Update(context.Background(), pending))
			}
			before := getRollbackObject(t, c, initial)
			messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
			live := getRollbackObject(t, c, initial)
			if drift {
				require.ErrorContains(t, err, "changed during restoration")
				require.Equal(t, before.Object, live.Object)
			} else {
				require.NoError(t, err)
				require.Contains(t, messages[0], "waived rollout verification")
				require.Equal(t, initial.Object["spec"], live.Object["spec"])
				require.Empty(t, live.GetAnnotations()[ocpRollbackAnnotation])
			}
		})
	}
}

func TestOCPConfigurationRollbackSkipWaitPatchFailureIsAtomic(t *testing.T) {
	initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator", "config": map[string]interface{}{"env": []interface{}{map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"}, map[string]interface{}{"name": "SITE", "value": "keep"}}}})
	reject := true
	patches := 0
	c := fake.NewClientBuilder().WithObjects(initial).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		patches++
		data, err := patch.Data(obj)
		require.NoError(t, err)
		require.Contains(t, string(data), `"resourceVersion"`)
		require.Contains(t, string(data), `"`+ocpRollbackAnnotation+`":null`)
		require.Contains(t, string(data), `"value":"false"`)
		if reject {
			return apierrors.NewForbidden(schema.GroupResource{Group: "operators.coreos.com", Resource: "subscriptions"}, obj.GetName(), errors.New("denied"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}).Build()
	_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true"}, false, "cohort")
	require.NoError(t, err)
	before := getRollbackObject(t, c, initial)
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
	require.ErrorContains(t, err, "denied")
	require.Equal(t, before.Object, getRollbackObject(t, c, initial).Object)
	reject = false
	_, err = restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
	require.NoError(t, err)
	require.Equal(t, 2, patches)
	live := getRollbackObject(t, c, initial)
	require.Equal(t, initial.Object["spec"], live.Object["spec"])
	require.Empty(t, live.GetAnnotations()[ocpRollbackAnnotation])
}

func TestOCPConfigurationRollbackSkipWaitPreservesUnresolvedFields(t *testing.T) {
	initial := rollbackObject("operators.coreos.com/v1alpha1", "Subscription", map[string]interface{}{"name": "nvidia-network-operator", "config": map[string]interface{}{"env": []interface{}{
		map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"},
		map[string]interface{}{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": "site-namespace"},
	}}})
	c := fake.NewClientBuilder().WithObjects(initial).Build()
	_, err := mergeSubscriptionEnvChangedForCohort(context.Background(), c, "operator-ns", map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true", "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": "l8k-namespace"}, false, "cohort")
	require.NoError(t, err)
	live := getRollbackObject(t, c, initial)
	env, _, err := unstructured.NestedSlice(live.Object, "spec", "config", "env")
	require.NoError(t, err)
	env[0].(map[string]interface{})["value"] = "site-change"
	require.NoError(t, unstructured.SetNestedSlice(live.Object, env, "spec", "config", "env"))
	require.NoError(t, c.Update(context.Background(), live))
	messages, err := restoreLiveOCPConfiguration(context.Background(), c, initial, "cohort", true)
	require.NoError(t, err)
	require.Contains(t, messages[0], "changed after deployment")
	live = getRollbackObject(t, c, initial)
	journal, err := readOCPConfigurationJournal(live)
	require.NoError(t, err)
	require.Len(t, journal.Fields, 1)
	require.Equal(t, []string{"env", "MAINTENANCE_OPERATOR_ENABLED"}, journal.Fields[0].Path)
	require.Equal(t, []string{"cohort"}, journal.Cohorts)
	require.Empty(t, journal.RestoreEnv)
	env, _, err = unstructured.NestedSlice(live.Object, "spec", "config", "env")
	require.NoError(t, err)
	require.Equal(t, "site-change", env[0].(map[string]interface{})["value"])
	require.Equal(t, "site-namespace", env[1].(map[string]interface{})["value"])
}
