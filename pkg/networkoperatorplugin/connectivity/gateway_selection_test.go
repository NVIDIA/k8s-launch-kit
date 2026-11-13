// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = ginkgo.Describe("gateway pod network-selection overrides", func() {
	ginkgo.It("rejects an overlapping override pool before reading or probing the NAD pool and preserves pair planning", func() {
		t := ginkgo.GinkgoT()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "tenant", Annotations: map[string]string{
				MultusAnnotation:           `[{"name":"tenant/rail-0","interface":"net1","ips":["192.0.2.10"]}]`,
				networkSelectionAnnotation: `[{"name":"rail-0","interface":"net1","cni-args":{"poolNames":["pool-override"],"poolType":"ippool","credential":"do-not-retain"}}]`,
			}},
			Spec: corev1.PodSpec{NodeName: "worker-a"},
		}
		tp, _, err := buildTestPod(pod)
		require.NoError(t, err)
		other := mkPod("pod-b", "rail-0")
		other.Namespace = "tenant"
		plan := PlanWithOptions([]TestPod{tp, other}, ModeStrict, config.RoutingSourceBased)
		require.Len(t, plan.Gateway, 2)
		require.Len(t, plan.ICMPSameRail, 2, "unsupported gateway overrides must not discard pair endpoints")
		test := plan.Gateway[0]
		require.Equal(t, "pod-a", test.SrcPod)
		original := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.254")
		override := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.253")
		override.SetName("pool-override")
		nad := gatewayNAD("tenant", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a"}}`)
		c := &gatewayPoolReadClient{Client: fake.NewClientBuilder().WithObjects(nad, original, override).Build()}
		target, err := configuredGateway(context.Background(), c, test, "network-operator")
		require.ErrorContains(t, err, "pod cni-args overrides are unsupported")
		assert.Zero(t, c.poolReads, "must reject before looking up the unoverridden NAD pool")
		assert.Empty(t, target.Gateway)
		assert.NotContains(t, target.Evidence, "IPPool")
		assert.NotContains(t, err.Error(), "do-not-retain")
		assert.NotContains(t, err.Error(), "pool-override")
		encoded, marshalErr := json.Marshal(test)
		require.NoError(t, marshalErr)
		assert.NotContains(t, string(encoded), "do-not-retain")
		assert.NotContains(t, string(encoded), "pool-override")
	})

	ginkgo.It("reports an override failure before any gateway probe through the matrix orchestrator", func() {
		t := ginkgo.GinkgoT()
		scheme := runtime.NewScheme()
		require.NoError(t, corev1.AddToScheme(scheme))
		require.NoError(t, appsv1.AddToScheme(scheme))
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "tenant", UID: "ds-uid"}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "tenant", OwnerReferences: []metav1.OwnerReference{{UID: "ds-uid"}}, Annotations: map[string]string{
				MultusAnnotation:           `[{"name":"tenant/rail-0","interface":"net1","ips":["192.0.2.10"]}]`,
				networkSelectionAnnotation: `[{"name":"rail-0","interface":"net1","cni-args":{"poolNames":["pool-override"]}}]`,
			}},
			Spec:   corev1.PodSpec{NodeName: "worker-a"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		}
		pool := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.254")
		override := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.253")
		override.SetName("pool-override")
		nad := gatewayNAD("tenant", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a"}}`)
		reads := &gatewayPoolReadClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds, pod, nad, pool, override).Build()}
		c := readyGatewayClient{Client: reads}
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "60-example-daemonset.yaml", Content: "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: test\n  namespace: tenant\nspec:\n  template:\n    spec:\n      containers:\n      - name: netshoot\n        image: netshoot\n"}})
		require.NoError(t, err)
		result, err := RunMatrixBundle(context.Background(), c, &rest.Config{Host: "://invalid"}, ui.NewSilent(), artifacts, Options{Checks: []Check{CheckGateway}, IPAMNamespace: "network-operator"})
		require.NoError(t, err)
		require.Len(t, result.PingResults, 1)
		r := result.PingResults[0]
		require.ErrorContains(t, r.Err, "pod cni-args overrides are unsupported")
		assert.Zero(t, reads.poolReads)
		assert.Empty(t, r.Route.Command, "gateway source-route qualification and ping must not execute")
		assert.Empty(t, r.Test.DstIP)
		assert.Empty(t, r.Stdout)
		assert.Empty(t, r.SkipReason)
		assert.Equal(t, MatrixSummary{TotalTests: 1, Failed: 1}, result.Summary)
	})

	ginkgo.It("matches the namespace, NAD and requested interface while leaving unrelated attachment args alone", func() {
		t := ginkgo.GinkgoT()
		for _, raw := range []string{
			``, `rail-0`, `tenant/rail-0@net1`, `other/network@net2, tenant/rail-0@net1`,
			`[{"name":"rail-0"}]`, `[{"name":"rail-0","namespace":"tenant","interface":"net1"}]`,
			`[{"name":"tenant/rail-0","interface":"net1","cni-args":{}}]`,
			`[{"name":"rail-0","interface":"net1","cni-args":null}]`,
			`[{"name":"rail-0","namespace":"other","interface":"net1","cni-args":{"poolNames":["other"]}},{"name":"rail-0","interface":"net1"}]`,
			`[{"name":"other-network","cni-args":{"poolType":"cidrpool"}},{"name":"rail-0"}]`,
			`[{"name":"rail-0","interface":"net2","cni-args":{"poolNames":["other"]}},{"name":"rail-0","interface":"net1"}]`,
			`[{"name":"other-network","cni-args":"unrelated"}]`,
		} {
			require.NoError(t, validateGatewayNetworkSelection(raw, "tenant", "tenant", "rail-0", "net1"), raw)
		}
	})

	ginkgo.It("rejects relevant args, malformed evidence and ambiguous attachment selection with redacted errors", func() {
		t := ginkgo.GinkgoT()
		for _, raw := range []string{
			`[{"name":"rail-0","cni-args":{"poolNames":["secret-value"]}}]`,
			`[{"name":"rail-0","cni-args":{"poolType":"cidrpool"}}]`,
			`[{"name":"rail-0","cni-args":{"allocateDefaultGateway":true}}]`,
			`[{"name":"rail-0","cni-args":"secret-value"}]`,
			`[{"name":"rail-0","cni-args":[]}]`,
			`[{"name":"rail-0"},{"name":"rail-0","interface":"net1"}]`,
			`[{"name":"other/rail-0","namespace":"tenant"}]`,
			`[{"name":"rail-0"}`, `[{"name":7}]`, `[null]`, `{"name":"rail-0"}`, `[] trailing`,
			`tenant//rail-0`, `tenant/rail-0@net1@net2`, `rail-0,,other`,
		} {
			err := validateGatewayNetworkSelection(raw, "tenant", "tenant", "rail-0", "net1")
			require.Error(t, err, raw)
			assert.NotContains(t, err.Error(), "secret-value")
		}
	})

	ginkgo.It("rejects CIDRPool type overrides but preserves other IPAM route fallback", func() {
		t := ginkgo.GinkgoT()
		for _, nvIPAM := range []bool{true, false} {
			raw := `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a","poolType":"cidrpool"}}`
			if !nvIPAM {
				raw = `{"type":"macvlan","ipam":{"type":"whereabouts"}}`
			}
			test := gatewayFixture()
			test.SrcNamespace, test.SrcNetwork = "tenant", "rail-0"
			test.sourceSelection = `[{"name":"rail-0","cni-args":{"poolNames":["other"],"poolType":"ippool"}}]`
			c := &gatewayPoolReadClient{Client: fake.NewClientBuilder().WithObjects(gatewayNAD("tenant", raw)).Build()}
			target, err := configuredGateway(context.Background(), c, test, "network-operator")
			if nvIPAM {
				require.ErrorContains(t, err, "pod cni-args")
			} else {
				require.NoError(t, err)
				assert.True(t, strings.Contains(target.Evidence, "runtime interface routes"))
			}
			assert.Zero(t, c.poolReads)
		}
	})
})

type gatewayPoolReadClient struct {
	client.Client
	poolReads int
}

func (c *gatewayPoolReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "IPPool" {
		c.poolReads++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
