// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	k8stest "k8s.io/client-go/testing"
)

type coverageBehavior struct {
	failedPathUID      string
	invalidProtoUID    string
	invalidNetworkUID  string
	invalidNetworkFact string
	wrongNodeName      string
}

type coverageLogState struct {
	sync.Mutex
	responses        map[string][]byte
	behavior         coverageBehavior
	created          []string
	requestData      string
	tenantVIP        string
	requestImmutable bool
}

type coverageClient struct {
	kubernetes.Interface
	logs *coverageLogState
}

func (c *coverageClient) CoreV1() typedcorev1.CoreV1Interface {
	return &coverageCore{CoreV1Interface: c.Interface.CoreV1(), logs: c.logs}
}

type coverageCore struct {
	typedcorev1.CoreV1Interface
	logs *coverageLogState
}

func (c *coverageCore) Pods(namespace string) typedcorev1.PodInterface {
	return &coveragePods{PodInterface: c.CoreV1Interface.Pods(namespace), namespace: namespace, logs: c.logs}
}

type coveragePods struct {
	typedcorev1.PodInterface
	namespace string
	logs      *coverageLogState
}

func (p *coveragePods) GetLogs(name string, _ *corev1.PodLogOptions) *rest.Request {
	p.logs.Lock()
	body := bytes.Clone(p.logs.responses[name])
	p.logs.Unlock()
	client := &fakerest.RESTClient{
		Client: fakerest.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
		}),
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		GroupVersion:         corev1.SchemeGroupVersion,
		VersionedAPIPath:     fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", p.namespace, name),
	}
	return client.Request()
}

var _ = Describe("probe coverage", func() {
	It("passes the requested HBN VIP through the immutable request and retains its route facts", func() {
		nodes := []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-b", "node-b")}
		client, _, logs := coverageRunnerClient(nodes, coverageBehavior{})
		inv := coverageInventory(nodes)
		inv.Evidence = nil
		request := coverageHBNRequest("192.0.2.20")

		rows, cleanup, err := coverageRunner().Run(context.Background(), request, coverageManagementClient(client), inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(cleanup.State).To(Equal("complete"))
		Expect(rows).To(HaveLen(4))
		Expect(inv.Evidence).To(HaveLen(4), "nil evidence map is initialized before the first accepted row")

		requestData, requestImmutable, tenantVIP := logs.requestSnapshot()
		Expect(requestImmutable).To(BeTrue())
		var wire struct {
			TenantVIP string `json:"tenantVIP"`
		}
		Expect(json.Unmarshal([]byte(requestData), &wire)).To(Succeed())
		Expect(wire.TenantVIP).To(Equal("192.0.2.20"))
		Expect(tenantVIP).To(Equal(wire.TenantVIP))

		for _, node := range inv.Management.Nodes {
			Expect(node.TenantVIPRoute).NotTo(BeNil())
			Expect(node.TenantVIPRoute.Destination).To(Equal(wire.TenantVIP))
			Expect(node.TenantVIPRoute.InterfaceIndex).To(Equal(2))
			Expect(node.TenantVIPRoute.Source).To(Equal("192.0.2.11"))
			Expect(node.Interfaces).NotTo(BeEmpty())
			Expect(node.Routes).NotTo(BeEmpty())
		}
	})

	It("rejects incomplete node-network facts without publishing them and retains earlier rows", func() {
		cases := []struct {
			name string
			fact string
		}{
			{name: "missing interfaces", fact: "missing-interfaces"},
			{name: "missing requested VIP route", fact: "missing-vip-route"},
			{name: "mismatched VIP route destination", fact: "wrong-vip-destination"},
			{name: "malformed interface prefix", fact: "malformed-prefix"},
		}
		for _, tc := range cases {
			By(tc.name)
			nodes := []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-b", "node-b")}
			client, _, logs := coverageRunnerClient(nodes, coverageBehavior{invalidNetworkUID: "node-b", invalidNetworkFact: tc.fact})
			inv := coverageInventory(nodes)
			request := coverageHBNRequest("192.0.2.20")

			rows, cleanup, err := coverageRunner().Run(context.Background(), request, coverageManagementClient(client), inv)
			Expect(err).To(MatchError(ContainSubstring("probe result binding/coverage invalid")))
			Expect(cleanup.State).To(Equal("complete"))
			Expect(rows).To(HaveLen(3), "accepted rows from earlier observations remain available")
			Expect(rows[0].NodeUID).To(Equal("node-a"))
			Expect(rows[0].NetworkMode).To(Equal("pod-network"))
			Expect(rows[1].NodeUID).To(Equal("node-a"))
			Expect(rows[1].NetworkMode).To(Equal("node-network"))
			Expect(rows[2].NodeUID).To(Equal("node-b"))
			Expect(rows[2].NetworkMode).To(Equal("pod-network"))
			Expect(inv.Management.Nodes[0].TenantVIPRoute).NotTo(BeNil())
			Expect(inv.Management.Nodes[0].TenantVIPRoute.Destination).To(Equal("192.0.2.20"))
			Expect(inv.Management.Nodes[1].Interfaces).To(BeEmpty())
			Expect(inv.Management.Nodes[1].Routes).To(BeEmpty())
			Expect(inv.Management.Nodes[1].TenantVIPRoute).To(BeNil())
			Expect(inv.Evidence).To(HaveLen(3))
			for _, evidence := range inv.Evidence {
				Expect(evidence.Vantage).NotTo(Equal("node-b/node-network"))
			}
			Expect(logs.podCreates()).To(Equal(4))
		}
	})

	It("retains failed path rows and completes later node and network-mode observations", func() {
		nodes := []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-b", "node-b")}
		client, tracker, logs := coverageRunnerClient(nodes, coverageBehavior{failedPathUID: "node-a"})
		inv := coverageInventory(nodes)
		rows, cleanup, err := coverageRunner().Run(context.Background(), coverageRequest(), coverageManagementClient(client), inv)

		Expect(err).To(MatchError(ContainSubstring("required probe paths failed")))
		Expect(rows).To(HaveLen(4))
		Expect(rows[0].NodeUID).To(Equal("node-a"))
		Expect(rows[0].NetworkMode).To(Equal("pod-network"))
		Expect(rows[0].Addresses[0].TLS).To(Equal("unavailable"))
		Expect(rows[1].NodeUID).To(Equal("node-a"))
		Expect(rows[1].NetworkMode).To(Equal("node-network"))
		Expect(rows[2].NodeUID).To(Equal("node-b"))
		Expect(rows[3].NetworkMode).To(Equal("node-network"))
		inv.Collection.Mode = "management"
		inv.Probes = rows
		pathCheckFailed := false
		for _, check := range discovery.ProbeChecks(inv, nil) {
			if check.Subject == "cp-a/pod-network/management API" && check.State == "failed" {
				pathCheckFailed = true
			}
		}
		Expect(pathCheckFailed).To(BeTrue())
		Expect(inv.Management.Nodes[0].Interfaces).To(ContainElement(inventory.Interface{AdminUp: ptr(true), OperState: "UP", Name: "enp1s0", Index: 2, MTU: 9000, Addresses: []string{"192.0.2.11/24"}}))
		Expect(inv.Management.Nodes[1].Interfaces).NotTo(BeEmpty())
		Expect(logs.podCreates()).To(Equal(4))
		Expect(cleanup.State).To(Equal("complete"))
		Expect(cleanup.Leftovers).To(BeEmpty())
		deletedPods, deletedConfigMaps := false, false
		for _, action := range tracker.Actions() {
			deletedPods = deletedPods || action.GetVerb() == "delete" && action.GetResource().Resource == "pods"
			deletedConfigMaps = deletedConfigMaps || action.GetVerb() == "delete" && action.GetResource().Resource == "configmaps"
		}
		Expect(deletedPods).To(BeTrue())
		Expect(deletedConfigMaps).To(BeTrue())
	})

	It("rejects incomplete, duplicate, or unsupported management node scopes before writes", func() {
		cases := []struct {
			name  string
			nodes []inventory.Node
		}{
			{name: "empty", nodes: []inventory.Node{}},
			{name: "missing UID", nodes: []inventory.Node{{Name: "cp-a", Ready: true, OS: "linux", Architecture: "amd64"}}},
			{name: "duplicate UID", nodes: []inventory.Node{coverageNode("cp-a", "same"), coverageNode("cp-b", "same")}},
			{name: "duplicate name", nodes: []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-a", "node-b")}},
			{name: "not ready", nodes: []inventory.Node{{Name: "cp-a", UID: "node-a", OS: "linux", Architecture: "amd64"}}},
			{name: "unschedulable", nodes: []inventory.Node{{Name: "cp-a", UID: "node-a", Ready: true, Unschedulable: true, OS: "linux", Architecture: "amd64"}}},
			{name: "non-linux", nodes: []inventory.Node{{Name: "cp-a", UID: "node-a", Ready: true, OS: "windows", Architecture: "amd64"}}},
			{name: "unsupported architecture", nodes: []inventory.Node{{Name: "cp-a", UID: "node-a", Ready: true, OS: "linux", Architecture: "arm64"}}},
		}
		for _, tc := range cases {
			By(tc.name)
			client, tracker, _ := coverageRunnerClient(tc.nodes, coverageBehavior{})
			inv := coverageInventory(tc.nodes)
			_, cleanup, err := coverageRunner().Run(context.Background(), coverageRequest(), coverageManagementClient(client), inv)
			Expect(err).To(HaveOccurred())
			Expect(tracker.Actions()).To(BeEmpty())
			Expect(cleanup.State).To(Equal("complete"))
		}
	})

	It("preserves prior rows when a later node returns an invalid protocol response", func() {
		nodes := []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-b", "node-b")}
		client, _, logs := coverageRunnerClient(nodes, coverageBehavior{invalidProtoUID: "node-b"})
		inv := coverageInventory(nodes)
		rows, cleanup, err := coverageRunner().Run(context.Background(), coverageRequest(), coverageManagementClient(client), inv)

		Expect(err).To(MatchError(ContainSubstring("probe result binding/coverage invalid")))
		Expect(rows).To(HaveLen(2))
		Expect(rows[0].NodeUID).To(Equal("node-a"))
		Expect(rows[1].NetworkMode).To(Equal("node-network"))
		Expect(inv.Management.Nodes[0].Interfaces).NotTo(BeEmpty())
		Expect(logs.podCreates()).To(Equal(3))
		Expect(cleanup.State).To(Equal("complete"))
	})

	It("aborts immediately when the created Pod is bound to a different node", func() {
		nodes := []inventory.Node{coverageNode("cp-a", "node-a"), coverageNode("cp-b", "node-b")}
		client, _, logs := coverageRunnerClient(nodes, coverageBehavior{wrongNodeName: "node-a"})
		rows, cleanup, err := coverageRunner().Run(context.Background(), coverageRequest(), coverageManagementClient(client), coverageInventory(nodes))

		Expect(err).To(MatchError(ContainSubstring("probe node identity changed")))
		Expect(rows).To(BeEmpty())
		Expect(logs.podCreates()).To(Equal(1))
		Expect(cleanup.State).To(Equal("complete"))
	})

	It("guards nil inputs and malformed DPU targets before creating resources", func() {
		runner := coverageRunner()
		request := coverageRequest()
		client, tracker, _ := coverageRunnerClient([]inventory.Node{coverageNode("cp-a", "node-a")}, coverageBehavior{})
		mg := coverageManagementClient(client)
		_, _, err := runner.Run(context.Background(), request, mg, nil)
		Expect(err).To(HaveOccurred())
		_, _, err = runner.Run(context.Background(), request, discovery.ManagementClient{}, coverageInventory([]inventory.Node{coverageNode("cp-a", "node-a")}))
		Expect(err).To(HaveOccurred())
		Expect(tracker.Actions()).To(BeEmpty())

		request.Targets = []config.Target{{Role: "dpus", Endpoint: "http://192.0.2.50"}}
		_, _, err = runner.Run(context.Background(), request, mg, coverageInventory([]inventory.Node{coverageNode("cp-a", "node-a")}))
		Expect(err).To(HaveOccurred())
		Expect(tracker.Actions()).To(HaveLen(1)) // only the namespace read
	})
})

func coverageNode(name, uid string) inventory.Node {
	return inventory.Node{Name: name, UID: uid, Ready: true, OS: "linux", Architecture: "amd64"}
}

func coverageInventory(nodes []inventory.Node) *inventory.Inventory {
	return &inventory.Inventory{Collection: inventory.Collection{RunID: "coverage-run"}, Management: &inventory.Management{Nodes: nodes}, Evidence: map[string]inventory.Evidence{}}
}

func coverageRequest() config.Request {
	return config.Request{Timeout: 30 * time.Second, RequestTimeout: time.Second}
}

func coverageHBNRequest(vip string) config.Request {
	request := coverageRequest()
	request.UseCase = "hbn-bgp"
	request.Input.Site.Provisioning = &config.Provisioning{
		Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: &vip}},
	}
	return request
}

func coverageRunner() Runner {
	return Runner{
		Assets: Assets{
			Repository: "registry.example/probe", ManifestDigest: "sha256:" + string(bytes.Repeat([]byte("a"), 64)),
			PlatformDigests: map[string]string{"amd64": "sha256:" + string(bytes.Repeat([]byte("b"), 64))}, Qualified: true,
		},
		Journal: func([]Receipt) error { return nil },
	}
}

func coverageManagementClient(client kubernetes.Interface) discovery.ManagementClient {
	return discovery.ManagementClient{Client: client, REST: &rest.Config{Host: "https://api.invalid"}, Namespace: "default"}
}

func coverageRunnerClient(nodes []inventory.Node, behavior coverageBehavior) (*coverageClient, *fake.Clientset, *coverageLogState) {
	objects := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}}
	seenNames := map[string]bool{}
	for _, node := range nodes {
		if node.Name == "" || seenNames[node.Name] {
			continue
		}
		seenNames[node.Name] = true
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name, UID: types.UID(node.UID)}})
	}
	base := fake.NewClientset(objects...)
	logs := &coverageLogState{responses: map[string][]byte{}, behavior: behavior}
	client := &coverageClient{Interface: base, logs: logs}
	base.PrependReactor("create", "configmaps", func(action k8stest.Action) (bool, runtime.Object, error) {
		created := action.(k8stest.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		created.UID, created.ResourceVersion = "coverage-cm-uid", "1"
		var request struct {
			TenantVIP string `json:"tenantVIP"`
		}
		if err := json.Unmarshal([]byte(created.Data["request.json"]), &request); err != nil {
			return true, nil, err
		}
		logs.Lock()
		logs.requestData = created.Data["request.json"]
		logs.tenantVIP = request.TenantVIP
		logs.requestImmutable = created.Immutable != nil && *created.Immutable
		logs.Unlock()
		if err := base.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), created, "default"); err != nil {
			return true, nil, err
		}
		return true, created, nil
	})
	base.PrependReactor("create", "pods", func(action k8stest.Action) (bool, runtime.Object, error) {
		created := action.(k8stest.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		logs.Lock()
		logs.created = append(logs.created, created.Name)
		created.UID = types.UID(fmt.Sprintf("coverage-pod-%d", len(logs.created)))
		nodeUID := created.Spec.Containers[0].Args[1]
		if logs.behavior.wrongNodeName == nodeUID {
			created.Spec.NodeName = "different-node"
		} else {
			created.Spec.NodeName = created.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values[0]
		}
		created.ResourceVersion = "1"
		created.Status.Phase = corev1.PodSucceeded
		created.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", ImageID: created.Spec.Containers[0].Image}}
		logs.responses[created.Name] = coverageLog(created, logs.behavior, logs.tenantVIP)
		logs.Unlock()
		if err := base.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), created, "default"); err != nil {
			return true, nil, err
		}
		return true, created, nil
	})
	return client, base, logs
}

func (s *coverageLogState) podCreates() int {
	s.Lock()
	defer s.Unlock()
	return len(s.created)
}

func (s *coverageLogState) requestSnapshot() (string, bool, string) {
	s.Lock()
	defer s.Unlock()
	return s.requestData, s.requestImmutable, s.tenantVIP
}

func coverageLog(pod *corev1.Pod, behavior coverageBehavior, tenantVIP string) []byte {
	nodeUID, mode := pod.Spec.Containers[0].Args[1], pod.Spec.Containers[0].Args[2]
	now := time.Now()
	row := inventory.ProbeResult{
		NodeUID: nodeUID, NetworkMode: mode, Destination: "management-api", DNS: "resolved",
		Addresses: []inventory.ProbeAddress{{Address: "192.0.2.10", TCP: "reachable", TLS: "verified", HTTP: "observed", HTTPStatus: http.StatusOK}},
	}
	if behavior.failedPathUID == nodeUID && mode == "pod-network" {
		row.Addresses[0].TLS = "unavailable"
		row.Addresses[0].HTTP = "http-unavailable"
		row.Addresses[0].HTTPStatus = 0
	}
	response := Response{
		StartedAt: now.Add(-time.Second), FinishedAt: now, Version: ProtocolVersion, RunID: "coverage-run",
		NodeUID: nodeUID, PodUID: string(pod.UID), NetworkMode: mode, Results: []inventory.ProbeResult{row},
	}
	if behavior.invalidProtoUID == nodeUID {
		response.Version = "unsupported"
	}
	if mode == "node-network" {
		response.Interfaces = []inventory.Interface{{AdminUp: ptr(true), OperState: "UP", Name: "enp1s0", Index: 2, MTU: 9000, Addresses: []string{"192.0.2.11/24"}}}
		response.Routes = []inventory.Route{{Destination: "default", Gateway: "192.0.2.1", InterfaceIndex: 2, Table: 254}}
		if tenantVIP != "" {
			response.TenantVIPRoute = &inventory.RouteLookup{Destination: tenantVIP, InterfaceIndex: 2, Source: "192.0.2.11", Gateway: "192.0.2.1"}
		}
		if behavior.invalidNetworkUID == nodeUID {
			switch behavior.invalidNetworkFact {
			case "missing-interfaces":
				response.Interfaces = nil
			case "missing-vip-route":
				response.TenantVIPRoute = nil
			case "wrong-vip-destination":
				response.TenantVIPRoute.Destination = "192.0.2.21"
			case "malformed-prefix":
				response.Interfaces[0].Addresses = []string{"192.0.2.11/99"}
			}
		}
	}
	body, _ := json.Marshal(response)
	return body
}

var _ = Describe("probe preflight error classification", func() {
	It("does not turn forbidden namespace access into a path coverage result", func() {
		client, base, logs := coverageRunnerClient([]inventory.Node{coverageNode("cp-a", "node-a")}, coverageBehavior{})
		base.PrependReactor("get", "namespaces", func(k8stest.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), "default", fmt.Errorf("denied"))
		})
		rows, cleanup, err := coverageRunner().Run(context.Background(), coverageRequest(), coverageManagementClient(client), coverageInventory([]inventory.Node{coverageNode("cp-a", "node-a")}))
		Expect(err).To(HaveOccurred())
		Expect(rows).To(BeEmpty())
		Expect(logs.podCreates()).To(BeZero())
		Expect(cleanup.State).To(Equal("complete"))
	})
})
