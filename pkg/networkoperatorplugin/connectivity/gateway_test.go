// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = ginkgo.Describe("secondary network gateway validation", func() {
	ginkgo.It("plans one check per namespaced source and rail, including a single pod in every mode", func() {
		t := ginkgo.GinkgoT()
		pod := mkPod("same-name", "rail-0", "rail-1")
		pod.Namespace = "ns-a"
		other := pod
		other.Namespace = "ns-b"
		for _, mode := range []Mode{ModeQuick, ModeFull, ModeStrict} {
			plan := PlanWithOptions([]TestPod{pod}, mode, config.RoutingSourceBased)
			require.NotNil(t, plan.Skip)
			require.Len(t, plan.Gateway, 2)
			assert.Empty(t, plan.ICMPSameRail)
			plan = PlanWithOptions([]TestPod{other, pod, pod}, mode, config.RoutingSourceBased)
			require.Len(t, plan.Gateway, 4)
			assert.Equal(t, "ns-a", plan.Gateway[0].SrcNamespace)
			assert.Equal(t, "ns-b", plan.Gateway[2].SrcNamespace)
		}
		assert.Empty(t, PlanWithOptions(nil, ModeStrict, config.RoutingSourceBased).Gateway)
	})

	ginkgo.It("accepts unique runtime gateways across route tables and explicitly on-link networks", func() {
		t := ginkgo.GinkgoT()
		for _, raw := range []string{`[]`, `[{"dev":"net1","dst":"192.0.2.0/24","scope":"link"}]`, `[{"dst":"192.0.2.0/24","protocol":"kernel","scope":"link","prefsrc":"192.0.2.10","flags":[]},{"type":"local","dst":"192.0.2.10","table":"local","flags":[]}]`} {
			gateway, err := gatewayFromRoutes(raw, "net1", "192.0.2.10")
			require.NoError(t, err)
			assert.Empty(t, gateway)
		}
		gateway, err := gatewayFromRoutes(`[{"dev":"net1","gateway":"192.0.2.254"},{"dev":"net1","table":100,"gateway":"192.0.2.254"}]`, "net1", "192.0.2.10")
		require.NoError(t, err)
		assert.Equal(t, "192.0.2.254", gateway)
		gateway, err = gatewayFromRoutes(`[{"dev":"net1","gateway":"fe80::1"}]`, "net1", "2001:db8::10")
		require.NoError(t, err)
		assert.Equal(t, "fe80::1", gateway)
	})

	ginkgo.It("rejects malformed, ambiguous, management, family-mismatched and indirect gateway evidence", func() {
		t := ginkgo.GinkgoT()
		for _, raw := range []string{
			``, `null`, `{}`, `[null]`, `[{}]`, `[{"dev":"net1"}]`, `[{"dev":"net1","gateway":7}]`, `[{"gateway":null}]`, `[{"gateway":""}]`,
			`[{"dev":"eth0","gateway":"192.0.2.1"}]`,
			`[{"dev":"net1","gateway":"bad"}]`,
			`[{"dev":"net1","gateway":"2001:db8::1"}]`,
			`[{"dev":"net1","gateway":"192.0.2.10"}]`,
			`[{"dev":"net1","gateway":"0.0.0.0"}]`,
			`[{"dev":"net1","gateway":"192.0.2.1"},{"dev":"net1","gateway":"192.0.2.2"}]`,
			`[{"dev":"net1","nexthops":[{"gateway":"192.0.2.1","dev":"net1"}]}]`,
			`[{"dev":"net1","nhid":3}]`,
			`[{"dev":"net1","via":{"family":"inet6","host":"fe80::1"}}]`,
		} {
			_, err := gatewayFromRoutes(raw, "net1", "192.0.2.10")
			require.Error(t, err, raw)
		}
	})

	ginkgo.It("qualifies the source interface before a bounded source-IP-bound echo and scopes IPv6 link-local gateways", func() {
		t := ginkgo.GinkgoT()
		for _, ipv6 := range []bool{false, true} {
			test := gatewayFixture()
			gateway, family := "192.0.2.254", "-4"
			if ipv6 {
				test.SrcIP, gateway, family = "2001:db8::10", "fe80::1", "-6"
			}
			commands := []string{}
			exec := func(ctx context.Context, _ *rest.Config, ns, pod, container string, args []string) (kubeclient.ExecResult, error) {
				_, deadline := ctx.Deadline()
				require.True(t, deadline)
				assert.Equal(t, "ns-a", ns)
				assert.Equal(t, "pod-a", pod)
				assert.Equal(t, "netshoot", container)
				commands = append(commands, args[2])
				switch len(commands) {
				case 1:
					assert.Contains(t, args[2], "ip -j "+family+" route show table all dev 'net1'")
					return kubeclient.ExecResult{Stdout: `[{"dev":"net1","gateway":"` + gateway + `"}]`, Stderr: "route-query-diagnostic"}, nil
				case 2:
					assert.Contains(t, args[2], "route get "+shellArg(gateway)+" from "+shellArg(test.SrcIP))
					if ipv6 {
						assert.Contains(t, args[2], "oif 'net1'")
					} else {
						assert.NotContains(t, args[2], "oif")
					}
					return kubeclient.ExecResult{Stdout: gateway + " dev net1 src " + test.SrcIP}, nil
				default:
					assert.Contains(t, args[2], "timeout -s TERM -k 2 5")
					assert.Contains(t, args[2], "-I "+shellArg(test.SrcIP))
					if ipv6 {
						assert.Contains(t, args[2], "ping -6")
						assert.Contains(t, args[2], "fe80::1%net1")
					}
					return kubeclient.ExecResult{Stdout: "1 received"}, nil
				}
			}
			r := runGateway(context.Background(), nil, "ns-a", "pod-a", "netshoot", test, exec, gatewayTarget{})
			require.NoError(t, r.Err)
			assert.True(t, r.OK)
			assert.True(t, r.ObservedOK)
			assert.Equal(t, gateway, r.Test.DstIP)
			assert.Contains(t, r.GatewayEvidence, "route-query-diagnostic")
			require.Len(t, commands, 3)
		}
	})

	ginkgo.It("retains missing gateway as a skip but rejects execution failures and wrong source routes without pinging", func() {
		t := ginkgo.GinkgoT()
		for _, scenario := range []string{"onlink", "query error", "malformed", "route error", "wrong interface", "indirect", "missing ip", "missing iface", "canceled"} {
			test := gatewayFixture()
			ctx, cancel := context.WithCancel(context.Background())
			if scenario == "canceled" {
				cancel()
			}
			if scenario == "missing iface" {
				test.SrcIface = ""
			}
			calls := 0
			exec := func(context.Context, *rest.Config, string, string, string, []string) (kubeclient.ExecResult, error) {
				calls++
				if calls == 1 {
					switch scenario {
					case "onlink", "canceled":
						return kubeclient.ExecResult{Stdout: `[]`}, nil
					case "query error":
						return kubeclient.ExecResult{Stderr: "ip: not found"}, errors.New("exit 127")
					case "malformed":
						return kubeclient.ExecResult{Stdout: `not json`}, nil
					}
					return kubeclient.ExecResult{Stdout: `[{"dev":"net1","gateway":"192.0.2.254"}]`}, nil
				}
				switch scenario {
				case "wrong interface":
					return kubeclient.ExecResult{Stdout: "192.0.2.254 dev eth0"}, nil
				case "indirect":
					return kubeclient.ExecResult{Stdout: "192.0.2.254 via 192.0.2.1 dev net1"}, nil
				case "missing ip":
					return kubeclient.ExecResult{Stdout: routeSkipMarker}, nil
				default:
					return kubeclient.ExecResult{Stderr: "route unavailable"}, errors.New("route failed")
				}
			}
			r := runGateway(ctx, nil, "ns-a", "pod-a", "netshoot", test, exec, gatewayTarget{})
			cancel()
			assert.False(t, r.OK, scenario)
			assert.False(t, r.ObservedOK, scenario)
			if scenario == "onlink" {
				require.NoError(t, r.Err)
				assert.NotEmpty(t, r.SkipReason)
			} else {
				require.Error(t, r.Err, scenario)
				assert.Empty(t, r.SkipReason, scenario)
			}
			assert.LessOrEqual(t, calls, 2, "must not ping: "+scenario)
		}
	})

	ginkgo.It("fails an unanswered echo with retained evidence and explicit measurement limits", func() {
		t := ginkgo.GinkgoT()
		calls := 0
		exec := func(context.Context, *rest.Config, string, string, string, []string) (kubeclient.ExecResult, error) {
			calls++
			switch calls {
			case 1:
				return kubeclient.ExecResult{Stdout: `[{"dev":"net1","gateway":"192.0.2.254"}]`}, nil
			case 2:
				return kubeclient.ExecResult{Stdout: "192.0.2.254 dev net1 src 192.0.2.10"}, nil
			default:
				return kubeclient.ExecResult{Stdout: "100% packet loss", Stderr: "probe diagnostic"}, errors.New("exit 1")
			}
		}
		r := runGateway(context.Background(), nil, "ns-a", "pod-a", "netshoot", gatewayFixture(), exec, gatewayTarget{})
		require.ErrorContains(t, r.Err, "does not establish whether neighbor resolution failed")
		assert.False(t, r.OK)
		assert.Equal(t, "100% packet loss", r.Stdout)
		assert.Equal(t, "probe diagnostic", r.Stderr)
		encoded, err := json.Marshal(r)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"Family":"gateway"`)
		assert.Contains(t, string(encoded), `"Error":`)
	})

	ginkgo.It("budgets discovery, route qualification and bounded ping for every source endpoint", func() {
		t := ginkgo.GinkgoT()
		plan := MatrixPlan{Gateway: []PingTest{gatewayFixture(), gatewayFixture()}}
		budget := automaticTimeoutBudget(plan, []Check{CheckGateway})
		assert.Equal(t, 2, budget.PlannedTests)
		assert.Equal(t, 70*time.Second, budget.Tests)
	})

	ginkgo.It("renders skipped gateways separately from pair matrices without claiming a pass", func() {
		t := ginkgo.GinkgoT()
		r := PingResult{Test: gatewayFixture(), SkipReason: "no configured gateway", GatewayEvidence: `[{"dev":"net1"}]`}
		matrix := &MatrixResult{PingResults: []PingResult{r}, Summary: MatrixSummary{TotalTests: 1, Skipped: 1}}
		var output bytes.Buffer
		require.NoError(t, RenderHTML(&output, ReportData{Matrix: matrix}))
		assert.Contains(t, output.String(), "Gateway ICMP reachability")
		assert.Contains(t, output.String(), "SKIPPED: no configured gateway")
		assert.NotContains(t, output.String(), "Rail <code>rail-0</code>")
		encoded, err := json.Marshal(matrix)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"Skipped":1`)
		assert.Contains(t, string(encoded), `"Passed":0`)
		assert.Contains(t, string(encoded), `"SkipReason":"no configured gateway"`)
		byRail, _, nodes, rails := groupResultsByKind(matrix.PingResults)
		assert.Empty(t, byRail)
		assert.Empty(t, nodes)
		assert.Empty(t, rails)
		assert.False(t, strings.Contains(output.String(), "ICMP reachable</td>"))
	})
})

func gatewayFixture() PingTest {
	return PingTest{Kind: Gateway, SrcNamespace: "ns-a", SrcPod: "pod-a", SrcNode: "worker-a", Rail: "rail-0", SrcRail: "rail-0", SrcIP: "192.0.2.10", SrcIface: "net1", Expectation: ExpectRequired}
}

// The fake client retains ready controller state while Apply records ownership
// through the production cleanup path. Pod exec uses an invalid URL, so this
// regression exercises orchestration without accessing any cluster.
type readyGatewayClient struct{ client.Client }

func (c readyGatewayClient) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return nil
}

var _ = ginkgo.Describe("gateway matrix orchestration", func() {
	ginkgo.It("runs gateways before reversed user selections, continues after failures and cleans up", func() {
		t := ginkgo.GinkgoT()
		for _, podCount := range []int{1, 2} {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns-a", UID: "ds-uid"}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: int32(podCount), UpdatedNumberScheduled: int32(podCount), NumberReady: int32(podCount)}}
			objects := []client.Object{ds}
			for i := 0; i < podCount; i++ {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "ns-a", OwnerReferences: []metav1.OwnerReference{{UID: "ds-uid"}}, Annotations: map[string]string{MultusAnnotation: fmt.Sprintf(`[{"name":"ns-a/rail-0","interface":"net1","ips":["192.0.2.%d"]}]`, i+10)}},
					Spec:       corev1.PodSpec{NodeName: fmt.Sprintf("worker-%d", i)},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
				})
			}
			c := readyGatewayClient{fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			artifacts, err := bundle.FromFiles([]bundle.File{{Name: "60-example-daemonset.yaml", Content: "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: test\n  namespace: ns-a\nspec:\n  template:\n    spec:\n      containers:\n      - name: netshoot\n        image: netshoot\n"}})
			require.NoError(t, err)
			checks := []Check{CheckGateway}
			if podCount == 2 {
				checks = []Check{CheckICMP, CheckGateway}
			}
			var output bytes.Buffer
			matrix, err := RunMatrixBundle(context.Background(), c, &rest.Config{Host: "://invalid"}, ui.NewWithWriter(&output), artifacts, Options{Checks: checks})
			require.NoError(t, err)
			require.Nil(t, matrix.Skipped)
			require.GreaterOrEqual(t, len(matrix.PingResults), podCount)
			for _, result := range matrix.PingResults[:podCount] {
				assert.Equal(t, Gateway, result.Test.Kind)
				assert.Error(t, result.Err)
			}
			assert.Equal(t, len(matrix.PingResults), matrix.Summary.Failed)
			assert.Zero(t, matrix.Summary.Passed)
			assert.Zero(t, matrix.Summary.Skipped)
			if podCount == 2 {
				require.Len(t, matrix.PingResults, 4)
				assert.Equal(t, ICMPSameRail, matrix.PingResults[2].Test.Kind)
				assert.Less(t, strings.Index(output.String(), "Stage: Gateway"), strings.Index(output.String(), "Stage: Layer 3"))
			}
			assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{})))
		}
	})
})
