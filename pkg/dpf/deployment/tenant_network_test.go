// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const tenantNetworkVIP = "192.0.2.200"
const tenantNetworkNodeIP = "192.0.2.1"

type tenantNetworkTestRunner struct {
	cleanup inventory.Cleanup
	err     error
	rows    []inventory.ProbeResult
	onRun   func()
	enrich  bool
	calls   int
	request config.Request
	nodes   []inventory.Node
}

func (r *tenantNetworkTestRunner) Run(_ context.Context, request config.Request, _ discovery.ManagementClient, inv *inventory.Inventory) ([]inventory.ProbeResult, inventory.Cleanup, error) {
	r.calls++
	r.request = request
	r.nodes = append([]inventory.Node(nil), inv.Management.Nodes...)
	if r.onRun != nil {
		r.onRun()
	}
	if r.enrich {
		for i := range inv.Management.Nodes {
			inv.Management.Nodes[i].Interfaces = []inventory.Interface{{
				Name: tenantNetworkInterface, Index: 2, MTU: 9000, AdminUp: boolPointer(true), OperState: "UP",
				Addresses: []string{tenantNetworkNodeIP + "/24"},
			}}
			inv.Management.Nodes[i].TenantVIPRoute = &inventory.RouteLookup{
				Destination: tenantNetworkVIP, Source: tenantNetworkNodeIP, InterfaceIndex: 2,
			}
		}
	}
	return r.rows, r.cleanup, r.err
}

const tenantNetworkInterface = "enp1s0"

func boolPointer(value bool) *bool { return &value }

func tenantNetworkClient(t *testing.T, nodes func(read int) []corev1.Node) (*apiFixture, *APIClient) {
	t.Helper()
	var nodeReads atomic.Int32
	f := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prefix/api/v1/nodes":
			read := int(nodeReads.Add(1))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, Items: nodes(read)})
		case "/prefix/apis/storage.k8s.io/v1/storageclasses":
			_, _ = w.Write([]byte(`{"apiVersion":"storage.k8s.io/v1","kind":"StorageClassList","items":[]}`))
		case "/prefix/apis":
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"APIGroupList","groups":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
	request := f.request
	request.RequestTimeout = 5 * time.Second
	c, err := NewAPIClient(context.Background(), request, f.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func tenantNetworkNodes() []corev1.Node {
	return []corev1.Node{
		tenantNetworkNode("cp-selected", "node-selected", map[string]string{"node-role.kubernetes.io/control-plane": "", "tenant": "selected", "zone": "a"}, true),
		tenantNetworkNode("cp-other", "node-other", map[string]string{"node-role.kubernetes.io/control-plane": "", "tenant": "other"}, true),
		tenantNetworkNode("worker-selected", "node-worker", map[string]string{"tenant": "selected"}, false),
	}
}

func tenantNetworkNode(name, uid string, labels map[string]string, ready bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Labels: labels},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{Architecture: "amd64", OperatingSystem: "linux"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: tenantNetworkNodeIP}},
		},
	}
}

func tenantNetworkTenant() bootstrap.Tenant {
	return bootstrap.Tenant{
		Name: "selected-tenant", VIP: tenantNetworkVIP, Interface: tenantNetworkInterface,
		NodeSelector: map[string]string{"tenant": "selected"},
	}
}

func successfulTenantNetworkRunner() *tenantNetworkTestRunner {
	return &tenantNetworkTestRunner{
		cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}},
		enrich:  true,
		rows:    []inventory.ProbeResult{{NodeUID: "node-selected", NetworkMode: "node-network", Destination: "management-api", DNS: "resolved"}},
	}
}

func TestCheckTenantNetworkUsesFreshSelectedNodesAndExactVIP(t *testing.T) {
	_, client := tenantNetworkClient(t, func(int) []corev1.Node { return tenantNetworkNodes() })
	runner := successfulTenantNetworkRunner()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	checks, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || len(runner.nodes) != 1 || runner.nodes[0].Name != "cp-selected" || runner.nodes[0].UID != "node-selected" {
		t.Fatalf("runner received unexpected scope: calls=%d nodes=%+v", runner.calls, runner.nodes)
	}
	if runner.request.Input.Site.Provisioning == nil || runner.request.Input.Site.Provisioning.Bootstrap == nil || runner.request.Input.Site.Provisioning.Bootstrap.Tenant.VIP == nil || *runner.request.Input.Site.Provisioning.Bootstrap.Tenant.VIP != tenantNetworkVIP {
		t.Fatalf("runner request omitted exact tenant VIP: %+v", runner.request.Input.Site.Provisioning)
	}
	if runner.request.UseCase != "hbn-bgp" || runner.request.Timeout < time.Second || runner.request.RequestTimeout != 5*time.Second {
		t.Fatalf("unexpected runner request bounds or use case: %+v", runner.request)
	}
	if runner.nodes[0].Interfaces != nil || runner.nodes[0].TenantVIPRoute != nil {
		t.Fatalf("runner received persisted interface or VIP-route facts: %+v", runner.nodes[0])
	}
	if len(checks) != 1 || checks[0].State != "passed" || checks[0].ReasonCode != "TENANT_VIP_ONLINK_ROUTE_VERIFIED" {
		t.Fatalf("fresh network facts were not accepted: %+v", checks)
	}
}

func TestCheckTenantNetworkRequiresProbeAndCleanupSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cleanup inventory.Cleanup
		err     error
		wantErr bool
		want    string
	}{
		{name: "cleanup incomplete", cleanup: inventory.Cleanup{State: "incomplete", Leftovers: []string{"pod"}}, wantErr: true},
		{name: "probe error", cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}}, err: errors.New("probe failed"), wantErr: true},
		{name: "no rows", cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}}, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, client := tenantNetworkClient(t, func(int) []corev1.Node { return tenantNetworkNodes() })
			runner := successfulTenantNetworkRunner()
			runner.cleanup, runner.err, runner.enrich = tc.cleanup, tc.err, tc.name != "no rows"
			if tc.name == "no rows" {
				runner.rows = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			checks, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second)
			if tc.wantErr {
				if err == nil {
					t.Fatal("incomplete probe outcome was accepted")
				}
				if len(checks) == 0 {
					t.Fatal("failed probe discarded per-path diagnostics")
				}
				for _, check := range checks {
					if check.ReasonCode == "" || check.Message == "" || check.Subject == "" {
						t.Fatalf("probe failure lacks an actionable diagnostic: %+v", check)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != 1 || checks[0].State != tc.want {
				t.Fatalf("missing probe facts should remain unknown, got %+v", checks)
			}
		})
	}
}

func TestCheckTenantNetworkStopsOnSelectionDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]corev1.Node) []corev1.Node
	}{
		{name: "new selected node", mutate: func(nodes []corev1.Node) []corev1.Node {
			nodes[1].Labels["tenant"] = "selected"
			return nodes
		}},
		{name: "selected UID changed", mutate: func(nodes []corev1.Node) []corev1.Node {
			nodes[0].UID = types.UID("replacement-uid")
			return nodes
		}},
		{name: "selected labels changed", mutate: func(nodes []corev1.Node) []corev1.Node {
			nodes[0].Labels["zone"] = "b"
			return nodes
		}},
		{name: "selected node not ready", mutate: func(nodes []corev1.Node) []corev1.Node {
			nodes[0].Status.Conditions[0].Status = corev1.ConditionFalse
			return nodes
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			_, client := tenantNetworkClient(t, func(read int) []corev1.Node {
				nodes := tenantNetworkNodes()
				if read >= 2 {
					nodes = tc.mutate(nodes)
				}
				reads.Add(1)
				return nodes
			})
			runner := successfulTenantNetworkRunner()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			checks, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second)
			if err == nil || len(checks) != 0 {
				t.Fatalf("selection drift was accepted: checks=%+v err=%v", checks, err)
			}
			if runner.calls != 1 || reads.Load() < 2 {
				t.Fatalf("drift was not checked after probing: runnerCalls=%d nodeReads=%d", runner.calls, reads.Load())
			}
		})
	}
}

func TestCheckTenantNetworkStopsOnClusterIdentityChange(t *testing.T) {
	f, client := tenantNetworkClient(t, func(int) []corev1.Node { return tenantNetworkNodes() })
	runner := successfulTenantNetworkRunner()
	runner.onRun = func() { f.uid.Store("replacement-cluster") }
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	checks, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "identity") || len(checks) != 0 {
		t.Fatalf("cluster replacement was accepted: checks=%+v err=%v", checks, err)
	}
}

func TestCheckTenantNetworkRejectsClusterChangeBetweenObserveAndProbe(t *testing.T) {
	var f *apiFixture
	f, client := tenantNetworkClient(t, func(read int) []corev1.Node {
		if read == 1 {
			f.uid.Store("replacement-cluster")
		}
		return tenantNetworkNodes()
	})
	runner := successfulTenantNetworkRunner()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	checks, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "identity") || len(checks) != 0 || runner.calls != 0 {
		t.Fatalf("stale observation reached probe: checks=%+v calls=%d err=%v", checks, runner.calls, err)
	}
}

func TestCheckTenantNetworkRequiresBoundLiveContextBeforeProbe(t *testing.T) {
	_, client := tenantNetworkClient(t, func(int) []corev1.Node { return tenantNetworkNodes() })
	runner := successfulTenantNetworkRunner()
	if _, err := client.checkTenantNetwork(context.Background(), tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("unbounded context accepted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cancel()
	if _, err := client.checkTenantNetwork(ctx, tenantNetworkTenant(), runner, "tenant-network-test", 5*time.Second); err == nil {
		t.Fatal("canceled context accepted")
	}
	if runner.calls != 0 {
		t.Fatalf("probe ran without live bounded context: %d calls", runner.calls)
	}
}

func TestSelectTenantNodesRejectsMalformedScope(t *testing.T) {
	selected := inventory.Node{
		Name: "cp", UID: "uid", ControlPlane: true, Ready: true, OS: "linux", Architecture: "amd64",
		Labels: map[string]string{"tenant": "selected"},
	}
	for _, tc := range []struct {
		name     string
		nodes    []inventory.Node
		selector map[string]string
	}{
		{name: "empty selector", nodes: []inventory.Node{selected}},
		{name: "no match", nodes: []inventory.Node{selected}, selector: map[string]string{"tenant": "missing"}},
		{name: "duplicate name", nodes: []inventory.Node{selected, {Name: selected.Name, UID: "other-uid", ControlPlane: true, Labels: map[string]string{"tenant": "selected"}}}, selector: map[string]string{"tenant": "selected"}},
		{name: "duplicate UID", nodes: []inventory.Node{selected, {Name: "other", UID: selected.UID, ControlPlane: true, Labels: map[string]string{"tenant": "selected"}}}, selector: map[string]string{"tenant": "selected"}},
		{name: "selected incomplete", nodes: []inventory.Node{{Name: "cp", UID: "uid", ControlPlane: true, Labels: map[string]string{"tenant": "selected"}, Ready: true, OS: "linux"}}, selector: map[string]string{"tenant": "selected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := selectTenantNodes(tc.nodes, tc.selector); err == nil {
				t.Fatal("malformed or empty selected scope accepted")
			}
		})
	}
}
