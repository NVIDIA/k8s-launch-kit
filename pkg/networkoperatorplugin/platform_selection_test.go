// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/rest"
)

var _ = ginkgo.Describe("explicit Host platform selection", func() {
	ginkgo.It("selects once from resolved config without coupling Kubernetes to Helm ownership", func() {
		for _, flavor := range []string{"", config.FlavorK8s, config.FlavorOCP} {
			for _, external := range []bool{false, true} {
				cfg := &config.LaunchKitConfig{Flavor: flavor, NetworkOperator: &config.NetworkOperatorConfig{SkipHelmChart: external}}
				plugin := &NetworkOperatorPlugin{LaunchKitConfig: cfg, NetworkOperator: cfg.NetworkOperator}
				opts, err := plugin.deployOptions()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expected := hostplatform.Kubernetes
				if flavor == config.FlavorOCP {
					expected = hostplatform.OpenShift
				}
				gomega.Expect(opts.Platform).To(gomega.Equal(expected))
				gomega.Expect(opts.SkipHelmChart).To(gomega.Equal(external))
				cfg.Flavor = "changed-after-selection"
				gomega.Expect(opts.Platform).To(gomega.Equal(expected))
				gomega.Expect(opts.Platform.Validate()).To(gomega.Succeed())
			}
		}
		_, err := (&NetworkOperatorPlugin{LaunchKitConfig: &config.LaunchKitConfig{Flavor: "unknown"}}).deployOptions()
		gomega.Expect(err).To(gomega.HaveOccurred())
		_, err = hostplatform.Select("unknown")
		gomega.Expect(err).To(gomega.HaveOccurred())
	})
	ginkgo.It("rejects invalid selections before client reads or Helm effects", func() {
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-policy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, tc := range []struct {
			platform hostplatform.Selection
			flavor   string
		}{
			{"", config.FlavorK8s}, {"unknown", config.FlavorK8s},
		} {
			cfg := &config.LaunchKitConfig{Flavor: tc.flavor}
			effects := 0
			install := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
				effects++
				return nil
			}
			// A nil client ensures that no cluster read or write is attempted.
			err := applyBundleWithInstaller(context.Background(), nil, artifacts, DeployOptions{Platform: tc.platform, Config: cfg}, install)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("platform selection"))
			results, err := ValidateBundle(context.Background(), nil, artifacts, cfg, tc.platform)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(results).To(gomega.BeNil())
			gomega.Expect(effects).To(gomega.BeZero())
		}
	})
	ginkgo.It("uses explicit OpenShift deployment selection independently of serialized flavor", func() {
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}\n"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, flavor := range []string{config.FlavorK8s, "unknown", ""} {
			err := ApplyBundle(context.Background(), nil, artifacts, DeployOptions{Platform: hostplatform.OpenShift, Config: &config.LaunchKitConfig{Flavor: flavor}})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("OpenShift deployment contains values.yaml"))
		}
	})
	ginkgo.It("requires config before an OpenShift deployment uses its explicit selection", func() {
		err := ApplyBundle(context.Background(), nil, nil, DeployOptions{Platform: hostplatform.OpenShift})
		gomega.Expect(err).To(gomega.MatchError("OpenShift deployment requires a cluster config"))
	})
})
