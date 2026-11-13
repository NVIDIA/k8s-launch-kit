// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package generation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

func TestGeneration(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "offline DPF generation") }

var _ = Describe("offline DPF generation", func() {
	var root, work string
	var in config.Input
	var configPath, chartDir string
	BeforeEach(func() {
		_, file, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		root = filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
		old, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(os.Chdir(old)).To(Succeed()) })
		Expect(os.Chdir(root)).To(Succeed())
		work, err = os.MkdirTemp(root, ".dpf-generation-test-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(os.RemoveAll(work)).To(Succeed()) })
		chartDir = filepath.Join(work, "charts")
		Expect(os.MkdirAll(chartDir, 0o700)).To(Succeed())
		for _, c := range sentry.Catalog() {
			data, err := os.ReadFile(filepath.Join(root, "pkg/dpf/sentry/testdata/charts", c.Filename))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(chartDir, c.Filename), data, 0o600)).To(Succeed())
		}
		in = generationInput(work, chartDir)
		configPath = filepath.Join(work, "site.yaml")
		saveInput(in, configPath)
		in, err = config.Load(configPath)
		Expect(err).NotTo(HaveOccurred())
	})

	It("builds and reloads a complete bundle without mutating source or reading access references", func() {
		before, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		b, err := Build(context.Background(), in, configinput.Values{})
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Index().Profile).To(Equal("sentry-bf4"))
		Expect(b.Index().SelectedDevices).To(Equal([]string{"dpu-1"}))
		Expect(b.Index().Files).To(ContainElement(HaveField("Path", bundle.ResolvedConfigPath)))
		outDir := filepath.Join(work, "bundle")
		_, err = b.Publish(context.Background(), outDir)
		Expect(err).NotTo(HaveOccurred())
		loaded, err := bundle.Load(outDir)
		Expect(err).NotTo(HaveOccurred())
		resolved, err := Configuration(loaded, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.Devices).To(HaveLen(1))
		Expect(resolved.Devices[0].Model).To(Equal("B4240"))
		Expect(loaded.Index().SelectedDevices).To(Equal([]string{resolved.Devices[0].ID}))
		badIndex := loaded.Index()
		badIndex.SelectedDevices = []string{"different-device"}
		_, err = bundle.New(badIndex, loaded.Files())
		Expect(err).To(MatchError(ContainSubstring("bundle digest mismatch")))
		after, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		for _, p := range []string{resolved.KubeconfigRef.File, filepath.Join(work, "missing-password"), filepath.Join(work, "missing-known-hosts")} {
			_, err := os.Stat(p)
			Expect(os.IsNotExist(err)).To(BeTrue(), "generation must not create access-reference files")
		}
	})

	It("rejects an old inventory before publication and an old bundle before cluster access", func() {
		b, err := Build(context.Background(), in, configinput.Values{})
		Expect(err).NotTo(HaveOccurred())
		files := b.Files()
		for i := range files {
			if files[i].Path == bundle.ResolvedConfigPath {
				g, err := config.DecodeResolved(files[i].Data)
				Expect(err).NotTo(HaveOccurred())
				g.Devices[0].Observations = nil
				files[i].Data, err = yaml.Marshal(config.Resolved{FormatVersion: "v1alpha1", Target: "dpf", Config: g})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		index := b.Index()
		index.BundleDigest = ""
		for i := range index.Files {
			index.Files[i].SHA256 = ""
		}
		legacy, err := bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		_, err = Configuration(legacy, "")
		Expect(err).To(MatchError(ContainSubstring("rerun discovery")))

		in.Site.Inventory.DPUs[0].Observations = nil
		saveInput(in, configPath)
		out := filepath.Join(work, "legacy-inventory")
		_, err = Run(context.Background(), Request{UserConfig: configPath, OutputPath: out})
		Expect(err).To(MatchError(ContainSubstring("rerun discovery")))
		_, err = os.Stat(out)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("rejects a consistently rehashed bundle whose resources diverge from its intent", func() {
		b, err := Build(context.Background(), in, configinput.Values{})
		Expect(err).NotTo(HaveOccurred())
		files := b.Files()
		for i := range files {
			if files[i].Path == bundle.ResolvedConfigPath {
				g, err := config.DecodeResolved(files[i].Data)
				Expect(err).NotTo(HaveOccurred())
				port := 30081
				g.Sentry.PolicyServerNodePort = &port
				files[i].Data, err = config.EncodeResolved(g)
				Expect(err).NotTo(HaveOccurred())
			}
		}
		index := b.Index()
		index.BundleDigest = ""
		for i := range index.Files {
			index.Files[i].SHA256 = ""
		}
		reindexed, err := bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred(), "byte integrity and structure still hold")
		_, err = Configuration(reindexed, "")
		Expect(err).To(MatchError(ContainSubstring("differs from its resolved profile intent")))
	})

	It("applies shared CLI overrides and rejects explicit empty values", func() {
		b, err := Build(context.Background(), in, configinput.Values{Overrides: []configinput.Override{{Flag: "--dpf-release", Path: "dpf.release", Value: "26.8.0-rc.1"}}})
		Expect(err).NotTo(HaveOccurred())
		resolved, err := config.DecodeResolved(findFile(b, bundle.ResolvedConfigPath))
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.Release).To(Equal("26.8.0-rc.1"))
		out := filepath.Join(work, "empty-release")
		_, err = Run(context.Background(), Request{UserConfig: configPath, OutputPath: out, Overrides: configinput.Values{Overrides: []configinput.Override{{Flag: "--dpf-release", Path: "dpf.release", Value: ""}}}})
		Expect(err).To(HaveOccurred())
		_, err = os.Stat(out)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("does not publish on cancellation or incomplete chart input", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out := filepath.Join(work, "cancelled")
		_, err := Run(ctx, Request{UserConfig: configPath, OutputPath: out})
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		_, err = os.Stat(out)
		Expect(os.IsNotExist(err)).To(BeTrue())

		Expect(os.Remove(filepath.Join(chartDir, sentry.Catalog()[0].Filename))).To(Succeed())
		out = filepath.Join(work, "wrong-chart-input")
		_, err = Run(context.Background(), Request{UserConfig: configPath, OutputPath: out})
		Expect(err).To(HaveOccurred())
		_, err = os.Stat(out)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("keeps an identical publication unchanged and preserves a destination with different intent", func() {
		out := filepath.Join(work, "published")
		first, err := Run(context.Background(), Request{UserConfig: configPath, OutputPath: out})
		Expect(err).NotTo(HaveOccurred())
		second, err := Run(context.Background(), Request{UserConfig: configPath, OutputPath: out})
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Unchanged).To(BeTrue())
		original, err := os.ReadFile(filepath.Join(out, bundle.IndexPath))
		Expect(err).NotTo(HaveOccurred())

		changed := in
		changed.Site.DPF.Name = stringPtr("other-sentry")
		changedPath := filepath.Join(work, "changed.yaml")
		saveInput(changed, changedPath)
		_, err = Run(context.Background(), Request{UserConfig: changedPath, OutputPath: out})
		Expect(err).To(HaveOccurred())
		current, err := os.ReadFile(filepath.Join(out, bundle.IndexPath))
		Expect(err).NotTo(HaveOccurred())
		Expect(current).To(Equal(original))
		Expect(first.BundleDigest).To(Equal(second.BundleDigest))
	})

	It("allows moved source paths with stable intent and rejects changed intent", func() {
		b, err := Build(context.Background(), in, configinput.Values{})
		Expect(err).NotTo(HaveOccurred())
		moved := in
		moved.Site.Management.KubeconfigRef.File = "new-location/management.kubeconfig"
		movedPath := filepath.Join(work, "moved", "site.yaml")
		saveInput(moved, movedPath)
		resolved, err := Configuration(b, movedPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.KubeconfigRef.File).To(Equal(filepath.Join(filepath.Dir(movedPath), "new-location/management.kubeconfig")))

		changed := moved
		changed.Site.DPF.Name = stringPtr("different-sentry")
		changedPath := filepath.Join(work, "changed-uid.yaml")
		saveInput(changed, changedPath)
		_, err = Configuration(b, changedPath)
		Expect(err).To(MatchError(ContainSubstring("run generate into a new bundle directory")))
	})
})

func generationInput(base, charts string) config.Input {
	const psid = "MT_0000000010"
	observed := inventory.Observation{State: "observed", Values: []string{psid}}
	dpu := inventory.Device{ID: "dpu-1", SerialNumber: "BF4-SERIAL-1", Model: "B4240", ReaderAdapter: "bluefield-card-v1", ObservationState: "observed", BMCEndpoint: "https://192.0.2.21", Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}}, Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{SerialNumber: "BF4-SERIAL-1", PSID: observed}}}}}
	host := inventory.Device{ID: "host-1", Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{SerialNumber: "BF4-SERIAL-1", PSID: observed}}}}}
	site := config.Site{}
	site.DPF.Release = stringPtr("26.8.0")
	site.Management.KubeconfigRef.File = "missing/management.kubeconfig"
	site.Management.Context = stringPtr("management-context")
	site.Management.ClusterUID = stringPtr("management-uid-1")
	site.Management.KubernetesVersion = stringPtr("v1.35.4")
	passwordFile := filepath.Join(base, "private", "password")
	site.Credentials = map[string]config.Credential{"remote": {Username: "operator", PasswordRef: config.PasswordRef{File: &passwordFile}}}
	site.Discovery.Hosts = []config.Endpoint{{CredentialRef: "remote", SSH: &config.SSH{Address: "192.0.2.12", KnownHostsFile: filepath.Join(base, "private", "known_hosts")}}}
	site.Provisioning = &config.Provisioning{Devices: []config.DeviceBinding{{ID: "dpu-1", DPUDevice: "dpu-device-1", DPUNode: "dpu-node-1"}}, DPUCluster: config.ClusterBinding{KubernetesVersion: "v1.35.6", Name: "tenant-one", Namespace: "dpf-operator-system", ClusterUID: "tenant-cluster-uid", ResourceUID: "resource-uid-1", Selector: map[string]string{"site.example.com/tenant": "sentry"}}, Software: config.Software{OSISO: artifact("dpf-os.iso"), PLDM: map[string]config.Artifact{psid: artifact("bf4.pldm")}}}
	site.Sentry = &config.Sentry{ChartDirectory: charts, IngressCIDR: "10.70.0.0/24", IngressIP: "10.70.0.20", ImagePullSecret: "sentry-pull", MITMSecret: "sentry-mitm", InferenceSecret: "sentry-inference", PolicyServerAddress: "192.0.2.40"}
	site.Inventory = &inventory.Inventory{FormatVersion: inventory.FormatVersion, Management: &inventory.Management{SystemNamespaceUID: "management-uid-1", KubernetesVersion: "v1.35.4"}, DPUs: []inventory.Device{dpu}, Hosts: []inventory.Device{host}, Associations: []inventory.Association{{DPUID: "dpu-1", HostID: "host-1", SerialNumber: "BF4-SERIAL-1"}}}
	return config.Input{Site: site, Path: filepath.Join(base, "site.yaml")}
}

func artifact(name string) config.Artifact {
	return config.Artifact{URL: "https://artifacts.example.invalid/" + name, SHA256: strings.Repeat("a", 64)}
}
func stringPtr(s string) *string { return &s }
func saveInput(in config.Input, path string) {
	data, err := yaml.Marshal(in.Site)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
	Expect(os.WriteFile(path, data, 0o600)).To(Succeed())
}
func findFile(b *bundle.Bundle, path string) []byte {
	for _, f := range b.Files() {
		if f.Path == path {
			return f.Data
		}
	}
	Fail("bundle file not found: " + path)
	return nil
}
