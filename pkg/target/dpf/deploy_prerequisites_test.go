// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

type prerequisitePhysicalCollector struct{ devices map[string]inventory.Device }

func (c prerequisitePhysicalCollector) Collect(_ context.Context, target config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	data, _ := json.Marshal(c.devices[target.Role])
	var device inventory.Device
	err := json.Unmarshal(data, &device)
	return device, map[string]inventory.Evidence{}, err
}

var _ = Describe("native deployment prerequisite provider", func() {
	It("returns real complete blocked discovery on resume while fresh deployment stays strict", func() {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/dpudevices.provisioning.dpu.nvidia.com":
				fmt.Fprint(w, `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"dpudevices.provisioning.dpu.nvidia.com","uid":"device-crd","generation":1},"spec":{"group":"provisioning.dpu.nvidia.com","scope":"Namespaced","names":{"plural":"dpudevices","kind":"DPUDevice"},"versions":[{"name":"v1alpha1","served":true,"storage":true}]}}`)

			case "/version":
				fmt.Fprint(w, `{"gitVersion":"v1.35.6"}`)
			case "/api/v1/namespaces/kube-system":
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"management-one"}}`)
			case "/api/v1/nodes":
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"NodeList","metadata":{"resourceVersion":"1"},"items":[]}`)
			case "/apis/provisioning.dpu.nvidia.com/v1alpha1/dpudevices":
				fmt.Fprint(w, `{"apiVersion":"provisioning.dpu.nvidia.com/v1alpha1","kind":"DPUDeviceList","metadata":{"resourceVersion":"1"},"items":[{"apiVersion":"provisioning.dpu.nvidia.com/v1alpha1","kind":"DPUDevice","metadata":{"name":"serial-a","namespace":"dpf-operator-system","uid":"11111111-1111-4111-8111-111111111111","resourceVersion":"1","generation":1},"spec":{"serialNumber":"SERIAL-A"}}]}`)
			case "/apis/provisioning.dpu.nvidia.com/v1alpha1/dpuclusters":
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUClusterList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
			default:
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
			}
		}))
		DeferCleanup(server.Close)
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		kubeconfig := filepath.Join(root, "kubeconfig")
		Expect(os.WriteFile(kubeconfig, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: selected\nclusters:\n- name: management\n  cluster: {server: '%s', certificate-authority-data: '%s'}\nusers:\n- name: reader\n  user: {token: fixture-token}\ncontexts:\n- name: selected\n  context: {cluster: management, user: reader}\n", server.URL, base64.StdEncoding.EncodeToString(ca))), 0600)).To(Succeed())
		input := generationSite()
		managementIP, managementPort := "192.0.2.10", 6443
		input.Site.Provisioning.Bootstrap.ManagementAPIIP = &managementIP
		input.Site.Provisioning.Bootstrap.ManagementAPIPort = &managementPort
		input.Path = filepath.Join(root, "site.yaml")
		input.Site.Inventory = nil
		input.Site.Management.KubeconfigRef.File = kubeconfig
		intent, err := config.ResolveIntent(input.Site)
		Expect(err).NotTo(HaveOccurred())
		physical := prerequisitePhysicalCollector{devices: map[string]inventory.Device{
			"hosts": {ID: "host-a", RecordKey: "host-a", InputRef: "/discovery/hosts/0", SSHEndpoint: "ssh://192.0.2.40:22", ObservationState: "observed", ReaderAdapter: "linux-host-ssh-v1", Adapters: []inventory.Adapter{{Key: "adapter-a", IdentityState: "known", SerialNumber: "SERIAL-A", PCIAddress: "0000:01:00.0"}}},
			"dpus":  {ID: "dpu-a", RecordKey: "dpu-a", InputRef: "/discovery/dpus/0", BMCEndpoint: "https://192.0.2.30", SerialNumber: "SERIAL-A", PSID: "MT_0000001774", Model: "B4240", ReaderAdapter: "bluefield-card-v1", ObservationState: "observed", Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}}},
		}}
		request := config.Request{Input: input, Kubeconfig: kubeconfig, Context: "selected", Mode: "inventory", Timeout: time.Minute, RequestTimeout: time.Second, MaxConcurrentBMCs: 2, DPFNamespace: intent.Namespace, Software: &intent.Release, HBN: intent.Networking, HBNGeneration: intent.Generation, Targets: []config.Target{{Role: "hosts", InputRef: "/discovery/hosts/0", Endpoint: "ssh://192.0.2.40:22", SSH: &config.SSHAccess{}}, {Role: "dpus", InputRef: "/discovery/dpus/0", Endpoint: "https://192.0.2.30"}}}
		deps := discovery.Dependencies{SSH: physical, BMC: physical, DeferHBNReadiness: true}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		DeferCleanup(cancel)
		before := discovery.Collect(ctx, request, "before", deps)
		Expect(before.Errors).To(BeEmpty())
		Expect(before.Discovery.Outcome).To(Equal("complete"))
		input.Site.Inventory = before.Inventory
		data, err := yaml.Marshal(input.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(input.Path, data, 0600)).To(Succeed())
		input, err = config.Decode(data, input.Path)
		Expect(err).NotTo(HaveOccurred())
		request.Input = input
		resolved, err := generationInputs(input)
		Expect(err).NotTo(HaveOccurred())
		resolved.OperatorArchive, err = os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
		Expect(err).NotTo(HaveOccurred())
		resolved.HBNChartArchive, err = os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
		Expect(err).NotTo(HaveOccurred())
		retained, err := generation.Generate(resolved.Inputs)
		Expect(err).NotTo(HaveOccurred())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		collectors := func(*artifacts.Store, *generation.Plan) discovery.Dependencies { return deps }
		for _, resume := range []bool{false, true} {
			verify := deploymentPrerequisitesWithCollectors(retained, filepath.Join(root, fmt.Sprint(resume)), resume, collectors)
			observed, err := verify(ctx, request, plan)
			if resume {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring("prerequisites are blocked")))
			}
			Expect(observed.Inventory).NotTo(BeNil())
			Expect(observed.Checks).NotTo(BeEmpty())
			found := false
			for _, check := range observed.Checks {
				if check.ReasonCode == "DPU_ALREADY_ENROLLED" {
					found = true
					Expect(check.State).To(Equal("failed"))
					Expect(check.Subject).To(Equal("selected DPUs"))
					Expect(check.Vantage).To(Equal("executor"))
				}
			}
			Expect(found).To(BeTrue(), "native provider must preserve the actual initial-state conflict instead of forcing ready")
			if resume {
				for _, check := range observed.Checks {
					Expect(strings.HasPrefix(check.ReasonCode, "OWNED_RESUME_")).To(BeFalse(), "only bound Run operation assessment may produce ownership proof")
				}
			}
		}
	})
})
