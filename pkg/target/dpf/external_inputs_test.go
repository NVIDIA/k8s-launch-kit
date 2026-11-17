// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("DPF public input workflow", func() {
	It("generates the maintained exact BF4 recipe from public inventory without an overrides file or access secrets", func() {
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(root, "discovered.yaml")
		data := publicFixtureBytes(generationSite())
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
		input, err := config.LoadInputs(path, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(config.VerifyRecordedConfiguration(input)).To(Succeed())
		Expect(input.Site.Provisioning.Flavor).To(BeNil())
		Expect(input.Site.Provisioning.Software.OSISO.URL).To(BeEmpty())
		var source map[string]any
		Expect(yaml.Unmarshal(data, &source)).To(Succeed())
		Expect(source).NotTo(HaveKey("provisioning"))
		Expect(source).NotTo(HaveKey("flavor"))
		Expect(source).NotTo(HaveKey("software"))
		charts := map[string]string{
			"https://helm.ngc.nvidia.com/nvidia/doca/charts/dpf-operator-v26.8.0.tgz": "../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz",
			"https://helm.ngc.nvidia.com/nvidia/doca/charts/doca-hbn-3.5.0.tgz":       "../../dpf/generation/testdata/doca-hbn-3.5.0.tgz",
		}
		fetched := []string{}
		var stdout, stderr bytes.Buffer
		// Only chart transport is replaced. Preparation uses the production
		// public parser, recorded-intent gate and maintained release resolver.
		driver := generateDriver{
			request: GenerateRequest{Inventory: path, UserConfig: "", UseCase: nil, OutputPath: filepath.Join(root, "bundle"), Deploy: false, Stdout: &stdout, Stderr: &stderr},
			prepare: nil,
			fetchChart: func(_ context.Context, address string) ([]byte, error) {
				file, ok := charts[address]
				Expect(ok).To(BeTrue(), "only the selected release's chart coordinates may be requested")
				fetched = append(fetched, address)
				return os.ReadFile(file)
			},
		}
		operation, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "json", Quiet: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(operation.Run(context.Background())).To(Succeed())
		Expect(fetched).To(ConsistOf(
			"https://helm.ngc.nvidia.com/nvidia/doca/charts/dpf-operator-v26.8.0.tgz",
			"https://helm.ngc.nvidia.com/nvidia/doca/charts/doca-hbn-3.5.0.tgz",
		))
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["state"]).To(Equal("generated"))
		Expect(result["deploymentReadiness"]).To(Equal("not-evaluated"))
		retained, err := bundle.Load(filepath.Join(root, "bundle"))
		Expect(err).NotTo(HaveOccurred())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		intent := plan.Intent()
		Expect(intent.Release).To(Equal("26.8.0"))
		Expect(intent.Devices).To(HaveLen(1))
		Expect(intent.Devices[0].PSID).To(Equal("MT_0000001774"))
		Expect(intent.Software.OSISO.URL).To(HavePrefix("https://nbu-nfs.gtm.nvidia.com/"))
		Expect(intent.Software.OSISO.SHA256).To(Equal("bc69d99904300e09483f9fb9cb5187562fa770308acb4f6f139717b2105b3272"))
		Expect(intent.Software.PLDM).To(HaveLen(1))
		Expect(intent.Software.PLDM["MT_0000001774"].URL).To(ContainSubstring("/Prod/MT_0000001774/"))
		Expect(intent.Software.PLDM["MT_0000001774"].SHA256).To(Equal("ac3be7fc6e84ad678325834e42fdc34392e7ab7dcccea2346fd58fe851c9122a"))
		provisioning, err := plan.ResolveProvisioning(input.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(provisioning.Flavor).To(HaveKey("nvconfig"))
		Expect(provisioning.Flavor).To(HaveKey("ovs"))
		retainedSource, err := config.LoadInputs(filepath.Join(root, "bundle", ".l8k", "source-config.yaml"), "")
		Expect(err).NotTo(HaveOccurred())
		Expect(retainedSource.Site.DPF.Release).To(HaveValue(Equal("26.8.0")))
		Expect(retainedSource.Site.Provisioning.Flavor).To(BeNil())
		Expect(retainedSource.Site.Provisioning.Software.OSISO.URL).To(BeEmpty())
		after, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(data))
	})
	DescribeTable("rejects hardware outside the exact maintained cohort before chart downloads", func(model, psid, reason string) {
		root := GinkgoT().TempDir()
		path := filepath.Join(root, "discovered.yaml")
		// The publication boundary itself rejects contradictory model/PSID
		// evidence, so alter wire input to exercise the public loading boundary.
		var wire map[string]any
		Expect(yaml.Unmarshal(publicFixtureBytes(generationSite()), &wire)).To(Succeed())
		device := wire["inventory"].(map[string]any)["dpus"].([]any)[0].(map[string]any)
		device["model"], device["psid"] = model, psid
		data, err := yaml.Marshal(wire)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
		calls := 0
		driver := generateDriver{request: GenerateRequest{Inventory: path, OutputPath: filepath.Join(root, "bundle"), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, fetchChart: func(context.Context, string) ([]byte, error) { calls++; return nil, nil }}
		operation, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
		Expect(err).NotTo(HaveOccurred())
		Expect(operation.Run(context.Background())).To(MatchError(ContainSubstring(reason)))
		Expect(calls).To(BeZero())
		_, err = os.Stat(filepath.Join(root, "bundle"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	},
		Entry("a different BF4 model lacks supported observed PSID evidence", "B4241", "MT_0000001774", "invalid external input type"),
		Entry("a different PSID on the selected model", "B4240", "MT_0000001775", "DPF_PSID_ARTIFACT_NOT_CATALOGED"),
	)
	It("requires rediscovery when overrides change checked intent before attempting catalog generation", func() {
		root := GinkgoT().TempDir()
		path, overrides := filepath.Join(root, "discovered.yaml"), filepath.Join(root, "overrides.yaml")
		Expect(os.WriteFile(path, publicFixtureBytes(generationSite()), 0600)).To(Succeed())
		Expect(os.WriteFile(overrides, []byte("useCase: hbn-bgp\nhbn: {networking: {fabricVRF: changed}}\n"), 0600)).To(Succeed())
		_, err := app.PrepareGeneration(app.GenerationRequest{Inventory: path, UserConfig: overrides})
		Expect(err).To(MatchError(ContainSubstring("discovery")))
	})
	It("restores retained allowed settings for a fresh access seed without importing bundle access or implementation", func() {
		original := deploymentSite()
		loaded, retained := generatedUseCaseSite(original)
		var seed map[string]any
		Expect(yaml.Unmarshal(publicFixtureBytes(loaded), &seed)).To(Succeed())
		delete(seed, "configuration")
		seed["management"] = map[string]any{"kubeconfigRef": map[string]any{"file": "./new-access.kubeconfig"}}
		data, err := yaml.Marshal(seed)
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(GinkgoT().TempDir(), "inventory.yaml")
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())
		current, err := config.LoadInputs(path, "")
		Expect(err).NotTo(HaveOccurred())
		plan, err := generation.LoadPlan(retained)
		Expect(err).NotTo(HaveOccurred())
		effective, err := app.WithRetainedConfiguration(current, plan)
		Expect(err).NotTo(HaveOccurred())
		Expect(effective.Site.Management.KubeconfigRef.File).To(Equal("./new-access.kubeconfig"))
		Expect(effective.Source).To(Equal(data))
		Expect(effective.Site.Provisioning.Flavor).To(BeNil())
		Expect(effective.Site.Provisioning.Software.OSISO.URL).To(BeEmpty())
		Expect(effective.Site.DPF.UseCase).To(HaveValue(Equal("hbn-bgp")))
	})
})
