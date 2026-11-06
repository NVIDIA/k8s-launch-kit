// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = ginkgo.Describe("configured NV-IPAM gateway", func() {
	ginkgo.It("uses exact NAD and configured pool namespace and probes a pool gateway without a via route", func() {
		t := ginkgo.GinkgoT()
		test := gatewayFixture()
		test.SrcNetwork = "tenant/rail-0"
		nad := gatewayNAD("tenant", `{"name":"rail-0","cniVersion":"0.3.1","plugins":[{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a"}},{"type":"tuning","password":"do-not-retain"}]}`)
		pool := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.254")
		other := gatewayPool("unrelated", "198.51.100.0/24", "198.51.100.1")
		c := fake.NewClientBuilder().WithObjects(nad, pool, other).Build()
		target, err := configuredGateway(context.Background(), c, test, "network-operator")
		require.NoError(t, err)
		assert.Equal(t, "192.0.2.254", target.Gateway)
		assert.Contains(t, target.Evidence, "NAD tenant/rail-0")
		assert.Contains(t, target.Evidence, "IPPool network-operator/pool-a subnet=192.0.2.0/24 gateway=192.0.2.254")
		assert.NotContains(t, target.Evidence, "do-not-retain")
		calls := 0
		exec := func(context.Context, *rest.Config, string, string, string, []string) (kubeclient.ExecResult, error) {
			calls++
			switch calls {
			case 1:
				return kubeclient.ExecResult{Stdout: "192.0.2.254 dev net1 src 192.0.2.10"}, nil
			default:
				return kubeclient.ExecResult{Stdout: "1 received"}, nil
			}
		}
		r := runGateway(context.Background(), nil, "ns-a", "pod-a", "netshoot", test, exec, target)
		require.NoError(t, r.Err)
		assert.True(t, r.OK)
		assert.Empty(t, r.SkipReason)
		assert.Equal(t, 2, calls)
		assert.Equal(t, "192.0.2.254", r.Test.DstIP)
	})

	ginkgo.It("fails absent, denied, malformed, or mismatched metadata without runtime fallback", func() {
		t := ginkgo.GinkgoT()
		for _, scenario := range []string{"missing nad", "missing pool", "denied nad", "denied pool", "namespace required", "wrong namespace", "bad source subnet", "bad gateway", "malformed config", "ambiguous ipam"} {
			test := gatewayFixture()
			test.SrcNetwork = "tenant/rail-0"
			nad := gatewayNAD("tenant", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a"}}`)
			pool := gatewayPool("network-operator", "192.0.2.0/24", "192.0.2.254")
			switch scenario {
			case "malformed config":
				require.NoError(t, unstructured.SetNestedField(nad.Object, "{", "spec", "config"))
			case "ambiguous ipam":
				require.NoError(t, unstructured.SetNestedField(nad.Object, `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a,pool-b"}}`, "spec", "config"))
			case "bad source subnet":
				require.NoError(t, unstructured.SetNestedField(pool.Object, "198.51.100.0/24", "spec", "subnet"))
			case "bad gateway":
				require.NoError(t, unstructured.SetNestedField(pool.Object, "192.0.2.10", "spec", "gateway"))
			}
			objects := []client.Object{}
			if scenario != "missing nad" {
				objects = append(objects, nad)
			}
			if scenario != "missing pool" {
				objects = append(objects, pool)
			}
			c := &gatewayDeniedClient{Client: fake.NewClientBuilder().WithObjects(objects...).Build()}
			switch scenario {
			case "denied nad":
				c.deniedKind = "NetworkAttachmentDefinition"
			case "denied pool":
				c.deniedKind = "IPPool"
			}
			namespace := "network-operator"
			switch scenario {
			case "namespace required":
				namespace = ""
			case "wrong namespace":
				namespace = "wrong"
			}
			_, err := configuredGateway(context.Background(), c, test, namespace)
			require.Error(t, err, scenario)
		}
	})

	ginkgo.It("supports route fallback for explicit CIDRPool and other IPAM but rejects ambiguous CNI shapes", func() {
		t := ginkgo.GinkgoT()
		for _, raw := range []string{
			`{"type":"macvlan","ipam":{"type":"whereabouts"}}`,
			`{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool-a","poolType":"cidrpool"}}`,
		} {
			test := gatewayFixture()
			test.SrcNetwork = "rail-0"
			c := fake.NewClientBuilder().WithObjects(gatewayNAD(test.SrcNamespace, raw)).Build()
			target, err := configuredGateway(context.Background(), c, test, "")
			require.NoError(t, err)
			assert.Empty(t, target.Gateway)
			assert.Contains(t, target.Evidence, "runtime interface routes")
		}
		for _, raw := range []string{
			`{}`, `null`, `{"plugins":[{}]}`,
			`{"type":"sriov","ipam":{"type":"nv-ipam"}}`,
			`{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"p","poolType":"invalid"}}`,
			`{"type":"sriov","args":{"cni":{"poolNames":["other"]}},"ipam":{"type":"nv-ipam","poolName":"p"}}`,
			`{"plugins":[{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"p"}},{"type":"macvlan","ipam":{"type":"nv-ipam","poolName":"p"}}]}`,
		} {
			_, _, err := nvIPAMPool(raw)
			require.Error(t, err, raw)
		}
	})

	ginkgo.It("uses the authoritative gateway directly without rejecting unrelated static next hops", func() {
		t := ginkgo.GinkgoT()
		calls := 0
		exec := func(_ context.Context, _ *rest.Config, _, _, _ string, command []string) (kubeclient.ExecResult, error) {
			calls++
			assert.NotContains(t, command[2], "route show")
			if calls == 1 {
				assert.Contains(t, command[2], `route get "192.0.2.254" from "192.0.2.10"`)
				return kubeclient.ExecResult{Stdout: "192.0.2.254 dev net1 src 192.0.2.10"}, nil
			}
			return kubeclient.ExecResult{Stdout: "1 received"}, nil
		}
		r := runGateway(context.Background(), nil, "ns-a", "pod-a", "netshoot", gatewayFixture(), exec, gatewayTarget{Gateway: "192.0.2.254", Evidence: "configured pool"})
		require.NoError(t, r.Err)
		assert.Equal(t, 2, calls)
		assert.True(t, r.OK)
		assert.Empty(t, r.SkipReason)
	})
})

func gatewayNAD(namespace, config string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition", "metadata": map[string]interface{}{"namespace": namespace, "name": "rail-0"}, "spec": map[string]interface{}{"config": config}}}
}

func gatewayPool(namespace, subnet, gateway string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "nv-ipam.nvidia.com/v1alpha1", "kind": "IPPool", "metadata": map[string]interface{}{"namespace": namespace, "name": "pool-a"}, "spec": map[string]interface{}{"subnet": subnet, "gateway": gateway}}}
}

type gatewayDeniedClient struct {
	client.Client
	deniedKind string
}

func (c *gatewayDeniedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == c.deniedKind {
		return fmt.Errorf("permission denied")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
