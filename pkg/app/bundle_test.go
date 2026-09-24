// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type bundleTestPlugin struct {
	files       map[string]string
	received    *bundle.Bundle
	legacyCalls int
}

func (*bundleTestPlugin) GetName() string                                                { return "test" }
func (*bundleTestPlugin) GetVersion() string                                             { return "test" }
func (*bundleTestPlugin) ProfileConfiguredInCmd(options.Options) bool                    { return true }
func (*bundleTestPlugin) BuildProfileFromOptions(options.Options, *config.Profile) error { return nil }
func (*bundleTestPlugin) DiscoverClusterConfig(context.Context, client.Client, *config.LaunchKitConfig) error {
	return nil
}
func (p *bundleTestPlugin) GenerateProfileDeploymentFiles(*profiles.Profile, *config.LaunchKitConfig) (map[string]string, error) {
	return p.files, nil
}
func (p *bundleTestPlugin) DeployProfile(context.Context, *profiles.Profile, client.Client, string) error {
	p.legacyCalls++
	return nil
}
func (p *bundleTestPlugin) DeployBundle(_ context.Context, _ client.Client, artifacts *bundle.Bundle) error {
	p.received = artifacts
	return nil
}

var _ = ginkgo.Describe("generation snapshot", func() {
	const valid = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: generated\n"

	ginkgo.It("checks rendered artifacts before replacing an existing output directory", func() {
		root := ginkgo.GinkgoT().TempDir()
		output := filepath.Join(root, "test")
		gomega.Expect(os.MkdirAll(output, 0o755)).To(gomega.Succeed())
		sentinel := filepath.Join(output, "keep.txt")
		gomega.Expect(os.WriteFile(sentinel, []byte("old"), 0o600)).To(gomega.Succeed())
		plugin := &bundleTestPlugin{files: map[string]string{"10-good.yaml": valid, "20-broken.yaml": "apiVersion: [bad"}}
		launcher := New(options.Options{SaveDeploymentFiles: root})
		launcher.ui = ui.NewSilent()
		launcher.plugins["test"] = plugin
		profile := &profiles.Profile{Plugin: "test", Name: "fixture"}
		err := launcher.generateDeploymentFiles(profile, &config.LaunchKitConfig{})
		gomega.Expect(err).To(gomega.HaveOccurred())
		contents, err := os.ReadFile(sentinel)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(contents)).To(gomega.Equal("old"))
		gomega.Expect(launcher.generatedBundles).NotTo(gomega.HaveKey(generatedProfileKey{Plugin: "test", Name: "fixture"}))
	})

	ginkgo.It("deploys the retained snapshot even after saved files change", func() {
		root := ginkgo.GinkgoT().TempDir()
		plugin := &bundleTestPlugin{files: map[string]string{"10-generated.yaml": valid}}
		launcher := New(options.Options{SaveDeploymentFiles: root, Deploy: true})
		launcher.ui = ui.NewSilent()
		launcher.context = context.Background()
		launcher.plugins["test"] = plugin
		profile := &profiles.Profile{Plugin: "test", Name: "fixture"}
		gomega.Expect(launcher.generateDeploymentFiles(profile, &config.LaunchKitConfig{})).To(gomega.Succeed())
		written, err := os.ReadFile(filepath.Join(root, "test", "10-generated.yaml"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(written)).To(gomega.Equal(valid))
		gomega.Expect(os.Remove(filepath.Join(root, "test", "10-generated.yaml"))).To(gomega.Succeed())
		gomega.Expect(launcher.deployConfigurationProfile(profile)).To(gomega.Succeed())
		gomega.Expect(plugin.legacyCalls).To(gomega.BeZero())
		gomega.Expect(plugin.received).NotTo(gomega.BeNil())
		gomega.Expect(plugin.received.Documents()[0].Ref().Name).To(gomega.Equal("generated"))
	})

	ginkgo.It("does not reload disk when a capable plugin has no retained bundle", func() {
		plugin := &bundleTestPlugin{}
		launcher := New(options.Options{SaveDeploymentFiles: ginkgo.GinkgoT().TempDir(), Deploy: true})
		launcher.ui = ui.NewSilent()
		launcher.context = context.Background()
		launcher.plugins["test"] = plugin
		err := launcher.deployConfigurationProfile(&profiles.Profile{Plugin: "test", Name: "fixture"})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("snapshot is missing"))
		gomega.Expect(plugin.legacyCalls).To(gomega.BeZero())
	})
})
