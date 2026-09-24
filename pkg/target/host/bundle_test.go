// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = ginkgo.Describe("Host artifact snapshot", func() {
	ginkgo.It("reports a malformed final document before creating a client", func() {
		dir := ginkgo.GinkgoT().TempDir()
		gomega.Expect(os.WriteFile(filepath.Join(dir, "90-broken.yaml"), []byte("apiVersion: [bad"), 0o600)).To(gomega.Succeed())
		report := filepath.Join(ginkgo.GinkgoT().TempDir(), "partial.html")
		created := false
		runner := validateRunner{newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
			created = true
			return nil, nil, nil
		}}
		err := runner.Run(context.Background(), ValidateRequest{Kubeconfig: "test-kubeconfig", DeploymentFiles: dir, ReportPath: report, OutputFormat: "text"})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(apperrors.ExitCodeFromError(err)).To(gomega.Equal(apperrors.ExitValidation))
		gomega.Expect(created).To(gomega.BeFalse())
		contents, readErr := os.ReadFile(report)
		gomega.Expect(readErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(contents)).To(gomega.ContainSubstring("90-broken.yaml"))
	})

	ginkgo.It("passes the loaded workload to connectivity after the source file is removed", func() {
		dir := ginkgo.GinkgoT().TempDir()
		workloadPath := filepath.Join(dir, "60-example-daemonset.yaml")
		gomega.Expect(os.WriteFile(workloadPath, []byte(userConnectivityDaemonSet), 0o600)).To(gomega.Succeed())
		cfgPath := filepath.Join(ginkgo.GinkgoT().TempDir(), "cluster-config.yaml")
		configYAML := "profile:\n  routing: " + config.RoutingSourceBased + "\nvalidation:\n  gpuDirect:\n    enabled: false\n"
		gomega.Expect(os.WriteFile(cfgPath, []byte(configYAML), 0o600)).To(gomega.Succeed())
		called := false
		runner := validateRunner{
			newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
				gomega.Expect(os.Remove(workloadPath)).To(gomega.Succeed())
				return nil, &rest.Config{}, nil
			},
			runConnectivityMatrix: func(_ context.Context, _ ctrlclient.Client, _ *rest.Config, _ ui.Output, artifacts *bundle.Bundle, _ connectivity.Options) (*connectivity.MatrixResult, error) {
				called = true
				gomega.Expect(artifacts.Documents()).To(gomega.HaveLen(1))
				gomega.Expect(artifacts.Documents()[0].Source().File).To(gomega.Equal("60-example-daemonset.yaml"))
				return &connectivity.MatrixResult{
					PingResults: []connectivity.PingResult{
						{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true},
						{Test: connectivity.PingTest{Kind: connectivity.RDMAPingSameRail}, OK: true},
						{Test: connectivity.PingTest{Kind: connectivity.RDMABwSameRail}, OK: true},
					}, Summary: connectivity.MatrixSummary{TotalTests: 3, Passed: 3},
				}, nil
			},
		}
		err := runner.Run(context.Background(), ValidateRequest{Kubeconfig: "test-kubeconfig", DeploymentFiles: dir, UserConfig: cfgPath, ReportPath: "-", OutputFormat: "text"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(called).To(gomega.BeTrue())
	})
})
