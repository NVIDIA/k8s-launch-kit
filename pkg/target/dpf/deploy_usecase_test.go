// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func generatedUseCaseSite(input config.Input) (config.Input, *bundle.Bundle) {
	root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
	Expect(err).NotTo(HaveOccurred())
	source := filepath.Join(root, "site.yaml")
	data, err := yaml.Marshal(input.Site)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(source, data, 0600)).To(Succeed())
	var stdout, stderr bytes.Buffer
	driver := generateDriver{request: GenerateRequest{Inventory: source, UseCase: stringPtr("hbn-bgp"), OutputPath: filepath.Join(root, "bundle"), Stdout: &stdout, Stderr: &stderr}, prepare: prepareFixtureGeneration, fetchChart: func(_ context.Context, address string) ([]byte, error) {
		if strings.HasSuffix(address, "dpf-operator-v26.8.0.tgz") {
			return os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
		}
		return os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
	}}
	op, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "json", Quiet: true}})
	Expect(err).NotTo(HaveOccurred())
	Expect(op.Run(context.Background())).To(Succeed())
	retained, err := bundle.Load(filepath.Join(root, "bundle"))
	Expect(err).NotTo(HaveOccurred())
	loaded, err := config.Load(source)
	Expect(err).NotTo(HaveOccurred())
	Expect(loaded.Source).To(Equal(data))
	return loaded, retained
}

type noDeploymentTransport struct{ calls *int }

func (t noDeploymentTransport) RoundTrip(*http.Request) (*http.Response, error) {
	*t.calls++
	return nil, context.Canceled
}

var _ = Describe("retained DPF deployment use case", func() {
	It("reuses omitted and original CLI-overridden selections without changing source or desired overrides", func() {
		for _, original := range []*string{nil, stringPtr(""), stringPtr("unknown"), stringPtr("hbn-bgp")} {
			input := deploymentSite()
			input.Site.DPF.UseCase = original
			mtu := 1400
			input.Site.DPF.HBN = &hbn.Options{WorkloadMTU: &mtu}
			input.Site.Provisioning.Resources = map[string]string{"memory": "8Gi"}
			loaded, retained := generatedUseCaseSite(input)
			before, err := yaml.Marshal(loaded.Document)
			Expect(err).NotTo(HaveOccurred())
			effective, err := withRetainedUseCase(loaded, retained)
			Expect(err).NotTo(HaveOccurred())
			Expect(*effective.Site.DPF.UseCase).To(Equal("hbn-bgp"))
			Expect(effective.Source).To(Equal(loaded.Source))
			Expect(effective.Site.DPF.HBN.WorkloadMTU).To(Equal(&mtu))
			Expect(effective.Site.DPF.Release).To(BeNil(), "restoring selection must not hydrate release defaults")
			Expect(effective.Site.Provisioning.Resources).To(Equal(input.Site.Provisioning.Resources))
			after, err := yaml.Marshal(loaded.Document)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before), "the caller's document must remain unchanged")
			Expect(loaded.Site.DPF.UseCase).To(Equal(original))
			plan, err := generation.LoadPlan(retained)
			Expect(err).NotTo(HaveOccurred())
			Expect(verifyDeploymentPlan(effective, retained, plan)).To(Succeed())
			for _, file := range retained.Files() {
				if file.Path == ".l8k/source-config.yaml" {
					Expect(file.Data).To(Equal(loaded.Source))
				}
			}
			current, err := os.ReadFile(loaded.Path)
			Expect(err).NotTo(HaveOccurred())
			Expect(current).To(Equal(loaded.Source))
		}
	})

	It("rejects new conflicting selections before access resolution or report creation", func() {
		for _, original := range []*string{nil, stringPtr("unknown"), stringPtr("hbn-bgp")} {
			input := deploymentSite()
			input.Site.DPF.UseCase = original
			loaded, _ := generatedUseCaseSite(input)
			for _, changed := range []string{"", "another-selection"} {
				loaded.Site.DPF.UseCase = &changed
				data := publicFixtureBytes(loaded)
				var err error
				Expect(os.WriteFile(loaded.Path, data, 0600)).To(Succeed())
				var stdout, stderr bytes.Buffer
				op, err := NewDeployAdapter(DeployRequest{Inventory: loaded.Path, DeploymentFiles: filepath.Join(filepath.Dir(loaded.Path), "bundle"), Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Deploy, Output: target.OutputOptions{Format: "json", Quiet: true}})
				Expect(err).NotTo(HaveOccurred())
				Expect(op.Run(context.Background())).To(MatchError(ContainSubstring("selection conflicts")))
				Expect(stdout.Len()).To(BeZero())
				reports, err := filepath.Glob(filepath.Join(filepath.Dir(loaded.Path), "l8k-dpf-deploy-*"))
				Expect(err).NotTo(HaveOccurred())
				Expect(reports).To(BeEmpty())
			}
		}
	})

	It("preserves current desired changes so retained-plan verification still rejects them", func() {
		input := deploymentSite()
		input.Site.DPF.UseCase = stringPtr("unknown")
		loaded, retained := generatedUseCaseSite(input)
		loaded.Site.Provisioning.Flavor["dpuMode"] = "nic"
		data, err := yaml.Marshal(loaded.Site)
		Expect(err).NotTo(HaveOccurred())
		current, err := config.Decode(data, loaded.Path)
		Expect(err).NotTo(HaveOccurred())
		effective, err := withRetainedUseCase(current, retained)
		Expect(err).NotTo(HaveOccurred())
		Expect(effective.Site.Provisioning.Flavor["dpuMode"]).To(Equal("nic"))
		Expect(effective.Source).To(Equal(data))
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		Expect(verifyDeploymentPlan(effective, retained, plan)).NotTo(Succeed())
	})

	It("rejects missing or unsupported bundle selections and corrupted source provenance", func() {
		input := deploymentSite()
		input.Site.DPF.UseCase = stringPtr("unknown")
		loaded, retained := generatedUseCaseSite(input)
		_, err := withRetainedUseCase(loaded, nil)
		Expect(err).To(HaveOccurred())
		index := retained.Index()
		index.Profile, index.BundleDigest = "other", ""
		unsupported, err := bundle.New(index, retained.Files())
		Expect(err).NotTo(HaveOccurred())
		_, err = withRetainedUseCase(loaded, unsupported)
		Expect(err).To(HaveOccurred())
		index = retained.Index()
		index.Profile, index.BundleDigest = "", ""
		_, err = bundle.New(index, retained.Files())
		Expect(err).To(HaveOccurred())
		// File tampering is rejected by bundle.Load before selection is restored.
		source := filepath.Join(filepath.Dir(loaded.Path), "bundle", ".l8k", "source-config.yaml")
		Expect(os.WriteFile(source, []byte("dpf: {useCase: replacement}\n"), 0600)).To(Succeed())
		_, err = bundle.Load(filepath.Dir(filepath.Dir(source)))
		Expect(err).To(HaveOccurred())
		Expect(os.Remove(source)).To(Succeed())
		_, err = bundle.Load(filepath.Dir(filepath.Dir(source)))
		Expect(err).To(HaveOccurred())
	})

	It("carries a generation-only selection through the public canceled deploy adapter without network calls", func() {
		passwordEnv := "L8K_RETAINED_USECASE_TEST_PASSWORD"
		GinkgoT().Setenv(passwordEnv, "synthetic-not-recorded")
		knownHosts := filepath.Join(GinkgoT().TempDir(), "known_hosts")
		Expect(os.WriteFile(knownHosts, []byte("synthetic host-key snapshot\n"), 0600)).To(Succeed())
		calls, transport := 0, http.DefaultTransport
		http.DefaultTransport = noDeploymentTransport{calls: &calls}
		DeferCleanup(func() { http.DefaultTransport = transport })
		for _, original := range []*string{nil, stringPtr("unknown"), stringPtr("")} {
			input := deploymentSite()
			input.Site.DPF.UseCase = original
			input.Site.Management.KubeconfigRef.File = ""
			input.Site.Discovery.Hosts[0].SSH.KnownHostsFile = knownHosts
			input.Site.Credentials = map[string]config.Credential{
				"host": {Username: "reader", PasswordRef: config.PasswordRef{Env: &passwordEnv}},
				"bmc":  {Username: "reader", PasswordRef: config.PasswordRef{Env: &passwordEnv}},
			}
			loaded, _ := generatedUseCaseSite(input)
			Expect(os.WriteFile(loaded.Path, publicFixtureBytes(loaded), 0600)).To(Succeed())
			var stdout, stderr bytes.Buffer
			op, err := NewDeployAdapter(DeployRequest{Inventory: loaded.Path, DeploymentFiles: filepath.Join(filepath.Dir(loaded.Path), "bundle"), Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Deploy, Output: target.OutputOptions{Format: "json", Quiet: true}})
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			Expect(op.Run(ctx)).To(MatchError(ContainSubstring("context canceled")))
			Expect(stdout.String()).To(ContainSubstring(`"state":"failed"`))
			Expect(stdout.String()).NotTo(ContainSubstring("synthetic-not-recorded"))
			files, err := filepath.Glob(filepath.Join(filepath.Dir(loaded.Path), "l8k-dpf-deploy-*", "*"))
			Expect(err).NotTo(HaveOccurred())
			Expect(files).To(BeEmpty(), "canceled deployment must stop before journals or effects")
		}
		Expect(calls).To(BeZero())
	})
})
