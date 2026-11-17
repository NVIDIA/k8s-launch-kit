// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
)

type topologyProbe struct {
	mu       sync.Mutex
	calls    int
	nodeUIDs []string
	cleanup  inventory.Cleanup
	err      error
}

func (p *topologyProbe) Run(_ context.Context, _ config.Request, _ managementaccess.Client, inv *inventory.Inventory) ([]inventory.ProbeResult, inventory.Cleanup, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	for _, node := range inv.Management.Nodes {
		p.nodeUIDs = append(p.nodeUIDs, node.UID)
	}
	return nil, p.cleanup, p.err
}

func managementTopologyKubeconfig(nodeCount, controlPlanes, notReadyIndex int) (string, func()) {
	items := make([]map[string]any, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		labels := map[string]string{}
		if i < controlPlanes {
			labels["node-role.kubernetes.io/control-plane"] = ""
		}
		ready := "True"
		if i == notReadyIndex {
			ready = "False"
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": "node-" + string(rune('a'+i)), "uid": "uid-" + string(rune('a'+i)), "labels": labels},
			"status": map[string]any{
				"nodeInfo":   map[string]string{"architecture": "amd64", "operatingSystem": "linux"},
				"conditions": []map[string]string{{"type": "Ready", "status": ready}},
			},
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.35.0"})
		case "/api/v1/namespaces/kube-system":
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"test-cluster"}}`))
		case "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "NodeList", "items": items})
		case "/api/v1/storageclasses":
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClassList", "items": []any{}})
		case "/apis":
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))

	dir, err := os.MkdirTemp("", "dpf-management-topology-")
	Expect(err).NotTo(HaveOccurred())
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	raw := clientapi.Config{
		CurrentContext: "management",
		Clusters:       map[string]*clientapi.Cluster{"management": {Server: server.URL, CertificateAuthorityData: ca}},
		AuthInfos:      map[string]*clientapi.AuthInfo{"reader": {Token: "offline-test-token"}},
		Contexts:       map[string]*clientapi.Context{"management": {Cluster: "management", AuthInfo: "reader"}},
	}
	data, err := clientcmd.Write(raw)
	Expect(err).NotTo(HaveOccurred())
	filename := filepath.Join(dir, "config")
	Expect(os.WriteFile(filename, data, 0600)).To(Succeed())
	return filename, func() {
		server.Close()
		Expect(os.RemoveAll(dir)).To(Succeed())
	}
}

var _ = Describe("management topology fanout", func() {
	It("probes every observed node for management mode without a control-plane count policy", func() {
		for _, scenario := range []struct {
			name, mode     string
			nodes, masters int
		}{{"one control plane", "management", 1, 1}, {"three control planes", "management", 3, 3}, {"five control planes", "management", 5, 5}, {"three control planes and workers", "management", 5, 3}, {"inventory one control plane", "inventory", 1, 1}, {"inventory five control planes", "inventory", 5, 5}} {
			By(scenario.name)
			kubeconfig, cleanup := managementTopologyKubeconfig(scenario.nodes, scenario.masters, -1)
			req := inventoryRequest()
			req.Targets[1].SSH = nil
			req.Targets[1].Endpoint = "https://dpu.invalid"
			req.Mode = scenario.mode
			req.Kubeconfig = kubeconfig
			req.Timeout = 5 * time.Second
			probe := &topologyProbe{cleanup: inventory.Cleanup{State: "complete", Leftovers: []string{}}}
			report := Collect(context.Background(), req, "offline-topology", Dependencies{BMC: fixtureCollector{admin: true}, Probes: probe})
			cleanup()

			Expect(report.Inventory.Management.Nodes).To(HaveLen(scenario.nodes))
			Expect(report.Discovery.Checks).NotTo(ContainElement(HaveField("ID", "dpf.management.control-plane-count")))
			if scenario.mode == "management" {
				Expect(probe.calls).To(Equal(1))
				expectedUIDs := make([]string, scenario.nodes)
				for i := range expectedUIDs {
					expectedUIDs[i] = "uid-" + string(rune('a'+i))
				}
				Expect(probe.nodeUIDs).To(ConsistOf(expectedUIDs))
			} else {
				Expect(probe.calls).To(BeZero())
				Expect(report.Discovery.Outcome).To(Equal("complete"))
				Expect(ExitCode(report)).To(BeZero())
			}
		}
	})

	It("enforces the HBN multi-master prerequisite without treating cardinality as HA proof", func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		release, err := releases.DefaultForUseCase(releases.UseCaseHBNBGP)
		Expect(err).NotTo(HaveOccurred())
		for _, scenario := range []struct {
			name                          string
			nodes, masters, notReadyIndex int
			state, reason                 string
		}{
			{"none", 0, 0, -1, "unknown", "CONTROL_PLANE_READINESS_UNKNOWN"},
			{"worker only", 1, 0, -1, "unknown", "CONTROL_PLANE_READINESS_UNKNOWN"},
			{"one", 1, 1, -1, "failed", "CONTROL_PLANE_MULTIMASTER_REQUIRED"},
			{"one plus workers", 3, 1, -1, "failed", "CONTROL_PLANE_MULTIMASTER_REQUIRED"},
			{"two", 2, 2, -1, "passed", "CONTROL_PLANE_NODES_READY"},
			{"three", 3, 3, -1, "passed", "CONTROL_PLANE_NODES_READY"},
			{"five", 5, 5, -1, "passed", "CONTROL_PLANE_NODES_READY"},
			{"degraded", 3, 3, 2, "failed", "CONTROL_PLANE_NODE_NOT_READY"},
		} {
			By(scenario.name)
			kubeconfig, cleanup := managementTopologyKubeconfig(scenario.nodes, scenario.masters, scenario.notReadyIndex)
			req := config.Request{HBN: &profile, Software: &release, Mode: "inventory", Kubeconfig: kubeconfig, Timeout: 5 * time.Second, RequestTimeout: time.Second}
			report := Collect(context.Background(), req, "offline-hbn-topology", Dependencies{DeferHBNReadiness: true})
			cleanup()
			Expect(report.Inventory.Management.Nodes).To(HaveLen(scenario.nodes))
			Expect(report.Discovery.Checks).NotTo(ContainElement(HaveField("ID", "dpf.management.control-plane-count")))
			var found *inventory.Check
			for i := range report.Readiness.Checks {
				if report.Readiness.Checks[i].ID == "dpf.hbn.management.control-plane" {
					found = &report.Readiness.Checks[i]
				}
			}
			Expect(found).NotTo(BeNil())
			Expect(found.State).To(Equal(scenario.state))
			Expect(found.ReasonCode).To(Equal(scenario.reason))
			Expect(found.RequiredPhase).To(Equal("deploy"))
			if found.State == "passed" {
				Expect(found.Message).To(ContainSubstring("does not establish control-plane HA"))
			}
			// Other required observations are absent: this one predicate cannot
			// grant overall deployment readiness, even with multiple Ready masters.
			Expect(report.Readiness.State).To(Equal("blocked"))
			before, err := json.Marshal(report.Inventory)
			Expect(err).NotTo(HaveOccurred())
			Expect(controlPlaneCheck(report.Inventory).state).To(Equal(scenario.state))
			after, err := json.Marshal(report.Inventory)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
		}
	})

	It("retains probe failure and incomplete cleanup for any observed node count", func() {
		kubeconfig, cleanup := managementTopologyKubeconfig(5, 5, -1)
		defer cleanup()
		req := inventoryRequest()
		req.Targets[1].SSH = nil
		req.Targets[1].Endpoint = "https://dpu.invalid"
		req.Mode = "management"
		req.Kubeconfig = kubeconfig
		req.Timeout = 5 * time.Second
		probe := &topologyProbe{cleanup: inventory.Cleanup{State: "incomplete", Leftovers: []string{"probe/node-e"}}, err: context.DeadlineExceeded}
		report := Collect(context.Background(), req, "offline-topology-failure", Dependencies{BMC: fixtureCollector{admin: true}, Probes: probe})

		Expect(probe.calls).To(Equal(1))
		Expect(probe.nodeUIDs).To(ConsistOf("uid-a", "uid-b", "uid-c", "uid-d", "uid-e"))
		Expect(report.Errors).To(ContainElement(HaveField("Code", "PROBE_FAILED")))
		Expect(report.Discovery.Cleanup.State).To(Equal("incomplete"))
		Expect(report.Discovery.Cleanup.Leftovers).To(ContainElement("probe/node-e"))
		Expect(report.Discovery.Outcome).To(Equal("partial"))
		Expect(ExitCode(report)).To(Equal(3))
	})
})
