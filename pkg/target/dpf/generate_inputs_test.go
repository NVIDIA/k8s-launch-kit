// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func generationSite() config.Input {
	useCase, vip, iface := "hbn-bgp", "192.0.2.20", "eth0"
	release, err := releases.Resolve("26.8.0")
	Expect(err).NotTo(HaveOccurred())
	input := config.Input{Path: "/site/input.yaml", Source: []byte("# original source\n")}
	input.Site.DPF.UseCase = &useCase
	input.Site.Discovery.Hosts = []config.Endpoint{{SSH: &config.SSH{Address: "192.0.2.40"}, CredentialRef: "host"}}
	input.Site.Discovery.DPUs = []config.Endpoint{{BMCEndpoint: "https://192.0.2.30", CredentialRef: "bmc"}}
	missing := "/must-not-open/discovery-password"
	input.Site.Credentials = map[string]config.Credential{"bmc": {Username: "admin", PasswordRef: config.PasswordRef{File: &missing}}}
	input.Site.Management.KubeconfigRef.File = "/must-not-open/kubeconfig"
	input.Site.Provisioning = &config.Provisioning{
		Software:  config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: strings.Repeat("b", 64)}}},
		Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: &vip, Interface: &iface}},
		Flavor:    map[string]any{"dpuMode": "dpu"},
	}
	input.Site.Inventory = &inventory.Inventory{
		FormatVersion: inventory.FormatVersion, Software: &release, HBN: &inventory.HBNInventory{Namespace: "dpf-operator-system"},
		Management:   &inventory.Management{APIEndpoint: "https://192.0.2.10:6443", SystemNamespaceUID: "cluster-one", TrustDigest: "system-roots", KubernetesVersion: "v1.34.0"},
		DPUs:         []inventory.Device{{ID: "dpu-a", InputRef: "/discovery/dpus/0", BMCEndpoint: "https://192.0.2.30", SerialNumber: "SERIAL-A", ObservationState: "observed", PSID: "MT_0000001774", Model: "B4240", ReaderAdapter: "bluefield-card-v1", Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"}}}},
		Hosts:        []inventory.Device{{ID: "host-a", InputRef: "/discovery/hosts/0", SSHEndpoint: "ssh://192.0.2.40:22", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "adapter-a", IdentityState: "known", SerialNumber: "SERIAL-A", PCIAddress: "0000:01:00.0"}}}},
		Associations: []inventory.Association{{DPUID: "dpu-a", HostID: "host-a", HostAdapterKey: "adapter-a", SerialNumber: "SERIAL-A"}},
	}
	return input
}

var _ = Describe("DPF generation input boundary", func() {
	It("resolves defaults and planned selectors without opening credentials or changing inputs", func() {
		input := generationSite()
		plan, err := generationInputs(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Operator.Bootstrap.ManagementAPIIP).To(Equal("192.0.2.10"))
		Expect(plan.Workload.Devices).To(HaveLen(1))
		Expect(plan.Workload.Devices[0].PSID).To(Equal("MT_0000001774"))
		Expect(plan.Workload.HBNResources).To(HaveKeyWithValue("memory", "6Gi"))
		Expect(plan.Workload.DPUClusterSelector).To(Equal(plan.Operator.Bootstrap.Tenant.Labels))
		Expect(plan.SourceConfig).To(Equal(input.Source))
		Expect(input.Site.Provisioning.Resources).To(BeNil())
	})
	It("publishes a complete bundle from saved discovery without reading access secrets", func() {
		input := generationSite()
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		siteBytes, err := yaml.Marshal(input.Site)
		Expect(err).NotTo(HaveOccurred())
		source := filepath.Join(root, "site.yaml")
		Expect(os.WriteFile(source, siteBytes, 0600)).To(Succeed())
		var stdout, stderr bytes.Buffer
		fetched := []string{}
		driver := generateDriver{request: GenerateRequest{UserConfig: source, OutputPath: filepath.Join(root, "bundle"), Stdout: &stdout, Stderr: &stderr}, fetchChart: func(_ context.Context, address string) ([]byte, error) {
			fetched = append(fetched, address)
			if strings.HasSuffix(address, "dpf-operator-v26.8.0.tgz") {
				return os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
			}
			return os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
		}}
		operation, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "json"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(operation.Run(context.Background())).To(Succeed())
		Expect(fetched).To(Equal([]string{"https://helm.ngc.nvidia.com/nvidia/doca/charts/dpf-operator-v26.8.0.tgz", "https://helm.ngc.nvidia.com/nvidia/doca/charts/doca-hbn-3.5.0.tgz"}))
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["state"]).To(Equal("generated"))
		Expect(result["deploymentReadiness"]).To(Equal("not-evaluated"))
		loaded, err := bundle.Load(filepath.Join(root, "bundle"))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Index().Stages).To(HaveLen(7))
		saved, err := os.ReadFile(source)
		Expect(err).NotTo(HaveOccurred())
		Expect(saved).To(Equal(siteBytes))
		retained, err := os.ReadFile(filepath.Join(root, "bundle", ".l8k", "source-config.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(retained).To(Equal(siteBytes))
		stdout.Reset()
		Expect(operation.Run(context.Background())).To(Succeed())
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["unchanged"]).To(BeTrue())
	})

	It("rejects missing or contradictory selected identities before generation", func() {
		cases := []func(*config.Input){
			func(i *config.Input) { i.Site.Discovery.Hosts[0].SSH.Address = "192.0.2.41" },
			func(i *config.Input) { i.Site.Discovery.Hosts = nil },
			func(i *config.Input) {
				changed := "/redfish/v1/Systems/other"
				i.Site.Discovery.DPUs[0].SystemURI = &changed
			},
			func(i *config.Input) { i.Site.Inventory = nil },
			func(i *config.Input) {
				i.Site.Inventory.DPUs[0].Observations["bmc-administrator"] = inventory.Observation{State: "unknown", ReasonCode: "NOT_VERIFIED"}
			},
			func(i *config.Input) { i.Site.Inventory.Associations[0].HostAdapterKey = "other" },
			func(i *config.Input) { i.Site.Discovery.DPUs[0].BMCEndpoint = "https://192.0.2.31" },
			func(i *config.Input) { i.Site.Inventory.DPUs[0].InputRef = "/discovery/dpus/1" },
			func(i *config.Input) { i.Site.Inventory.DPUs = append(i.Site.Inventory.DPUs, i.Site.Inventory.DPUs[0]) },
			func(i *config.Input) { i.Site.Inventory.Software.Version = "26.4.0" },
			func(i *config.Input) { i.Site.Inventory.Management.SystemNamespaceUID = "" },
			func(i *config.Input) { i.Site.Provisioning.Flavor = nil },
			func(i *config.Input) { i.Site.Provisioning.Software.PLDM = nil },
			func(i *config.Input) { i.Site.Provisioning.Bootstrap.Tenant.VIP = nil },
			func(i *config.Input) { i.Site.Provisioning.Resources = map[string]string{} },
		}
		for _, mutate := range cases {
			input := generationSite()
			mutate(&input)
			_, err := generationInputs(input)
			Expect(err).To(HaveOccurred())
		}
	})
	It("binds an explicit host BMC system before accepting saved inventory", func() {
		input := generationSite()
		system := "/redfish/v1/Systems/1"
		input.Site.Discovery.Hosts[0] = config.Endpoint{BMCEndpoint: "https://192.0.2.40", SystemURI: &system, CredentialRef: "host"}
		input.Site.Inventory.Hosts[0].SSHEndpoint = ""
		input.Site.Inventory.Hosts[0].BMCEndpoint = "https://192.0.2.40"
		input.Site.Inventory.Hosts[0].SystemURI = system
		_, err := generationInputs(input)
		Expect(err).NotTo(HaveOccurred())
		changed := "/redfish/v1/Systems/2"
		input.Site.Discovery.Hosts[0].SystemURI = &changed
		_, err = generationInputs(input)
		Expect(err).To(MatchError(ContainSubstring("system")))
	})
	It("rejects unsupported effects before reading input or fetching charts", func() {
		for _, inv := range []target.Invocation{
			{Target: target.DPF, Phase: target.Deploy},
			{Target: target.DPF, Phase: target.Generate, Execution: target.ExecutionOptions{DryRun: true}},
		} {
			_, err := NewGenerateAdapter(GenerateRequest{UserConfig: "/missing", OutputPath: "/missing"}).Bind(inv)
			Expect(err).To(HaveOccurred())
		}
		_, err := NewGenerateAdapter(GenerateRequest{UserConfig: "/missing", OutputPath: "/missing", Deploy: true}).Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).To(HaveOccurred())
	})
	It("honors cancellation before file reads or archive downloads", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		driver := generateDriver{request: GenerateRequest{UserConfig: "/must-not-open/site", OutputPath: "/must-not-create/output"}, fetchChart: func(context.Context, string) ([]byte, error) { called = true; return nil, nil }}
		op, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(ctx)).To(MatchError(context.Canceled))
		Expect(called).To(BeFalse())
	})
	It("does not fetch archives or publish when site resolution fails", func() {
		root := GinkgoT().TempDir()
		file := filepath.Join(root, "site.yaml")
		Expect(os.WriteFile(file, []byte("dpf: {useCase: hbn-bgp}\n"), 0600)).To(Succeed())
		called := false
		var stdout, stderr bytes.Buffer
		driver := generateDriver{request: GenerateRequest{UserConfig: file, OutputPath: filepath.Join(root, "output"), Stdout: &stdout, Stderr: &stderr}, fetchChart: func(context.Context, string) ([]byte, error) { called = true; return nil, nil }}
		op, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(context.Background())).NotTo(Succeed())
		Expect(called).To(BeFalse())
		Expect(stdout.String()).To(BeEmpty())
		_, err = os.Stat(filepath.Join(root, "output"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
