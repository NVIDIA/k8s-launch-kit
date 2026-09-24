// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/rest"
)

var _ = ginkgo.Describe("deployment preparation", func() {
	const ncp = "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n"
	spy := func(calls *int) helmValuesInstaller {
		return func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
			*calls = *calls + 1
			return nil
		}
	}
	installOpts := DeployOptions{RestConfig: &rest.Config{}, NetworkOperator: &config.NetworkOperatorConfig{HelmRepoURL: "https://example.invalid", Version: "v1"}}

	ginkgo.It("rejects multiple distinct NCPs before any client is required", func() {
		artifacts, err := bundle.FromFiles([]bundle.File{
			{Name: "10-policy.yaml", Content: ncp},
			{Name: "11-policy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: other\n"},
			{Name: "values.yaml", Content: "operator: {}\n"},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		calls := 0
		err = applyBundleWithInstaller(context.Background(), nil, artifacts, installOpts, spy(&calls))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(calls).To(gomega.BeZero())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("multiple NicClusterPolicy"))
		gomega.Expect(apperrors.ExitCodeFromError(err)).To(gomega.Equal(apperrors.ExitValidation))
	})

	ginkgo.It("rejects an invalid final document without a partial preflight inventory", func() {
		dir := ginkgo.GinkgoT().TempDir()
		gomega.Expect(os.WriteFile(filepath.Join(dir, "10-policy.yaml"), []byte(ncp), 0o600)).To(gomega.Succeed())
		gomega.Expect(os.WriteFile(filepath.Join(dir, "20-broken.yaml"), []byte("apiVersion: [broken"), 0o600)).To(gomega.Succeed())
		_, err := preflight.ScanGeneratedManifests(dir)
		gomega.Expect(err).To(gomega.HaveOccurred())
		calls := 0
		err = applyManifestsFromDirWithInstaller(context.Background(), nil, dir, installOpts, spy(&calls))
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(calls).To(gomega.BeZero())
		gomega.Expect(apperrors.ExitCodeFromError(err)).To(gomega.Equal(apperrors.ExitValidation))
	})

	ginkgo.It("projects only deployment documents with their served versions", func() {
		artifacts, err := bundle.FromFiles([]bundle.File{
			{Name: "10-policy.yaml", Content: ncp},
			{Name: "60-example.yaml", Content: "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: example\n  namespace: test\n"},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		refs, err := preflight.GeneratedManifestRefs(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(refs).To(gomega.HaveLen(1))
		gomega.Expect(refs[0].GVK.Version).To(gomega.Equal("v1alpha1"))
		_, err = preflight.GeneratedManifestRefs(nil)
		gomega.Expect(err).To(gomega.HaveOccurred())
	})
})
