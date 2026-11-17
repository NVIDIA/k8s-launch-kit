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
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func canceledDiscoverySeed(root string) (string, []byte) {
	GinkgoHelper()
	GinkgoT().Setenv("L8K_DISCOVERY_OPERATIONS_PASSWORD", "private-not-published")
	source := []byte(`credentials:
  reader: {username: reader, passwordRef: {env: L8K_DISCOVERY_OPERATIONS_PASSWORD}}
discovery:
  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: reader, tls: {insecureSkipVerify: true}}]
  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: reader}]
`)
	path := filepath.Join(root, "inventory.yaml")
	Expect(os.WriteFile(path, source, 0600)).To(Succeed())
	return path, source
}

// publicationConflictWriter simulates a concurrent source edit at the observed
// publication boundary, without replacing application collection/publication.
type publicationConflictWriter struct {
	bytes.Buffer
	source string
	edited bool
	err    error
}

func (w *publicationConflictWriter) Write(data []byte) (int, error) {
	if !w.edited && bytes.Contains(data, []byte("[RUNNING] Publication:")) {
		w.edited = true
		w.err = os.WriteFile(w.source, []byte("# concurrent source replacement\n"), 0600)
	}
	return w.Buffer.Write(data)
}

var _ = Describe("native application operation wiring", func() {
	It("publishes interrupted discovery evidence, preserves input and projects text or one JSON result with quiet/TLS progress", func() {
		for _, format := range []string{"text", "json"} {
			for _, quiet := range []bool{false, true} {
				root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
				Expect(err).NotTo(HaveOccurred())
				source, before := canceledDiscoverySeed(root)
				output := filepath.Join(root, "discovered.yaml")
				var stdout, stderr bytes.Buffer
				op, err := NewDiscoverAdapter(Request{Inventory: source, OutputPath: output, ExplicitOutput: true, Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: format, Quiet: quiet}})
				Expect(err).NotTo(HaveOccurred())
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				Expect(apperrors.ExitCodeFromError(op.Run(ctx))).NotTo(BeZero())
				if format == "json" {
					var result map[string]any
					Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
					Expect(result["execution"]).To(Equal("cancelled"))
				} else {
					Expect(stdout.String()).To(HavePrefix("DPF discovery cancelled (inventory)\n"))
				}
				Expect(stdout.String()).NotTo(ContainSubstring("private-not-published"))
				if quiet {
					Expect(stderr.String()).To(BeEmpty())
				} else {
					Expect(stderr.String()).To(ContainSubstring("Warning: BMC TLS certificate verification disabled for https://192.0.2.1\n"))
					Expect(stderr.String()).To(ContainSubstring("[RUNNING] Publication"))
					Expect(stderr.String()).To(ContainSubstring("[PASS] Publication: partial discovery artifacts saved"))
				}
				reports, err := filepath.Glob(filepath.Join(root, "discovered.discovery", "*", "report.json"))
				Expect(err).NotTo(HaveOccurred())
				Expect(reports).To(HaveLen(1))
				data, err := os.ReadFile(reports[0])
				Expect(err).NotTo(HaveOccurred())
				var saved map[string]any
				Expect(json.Unmarshal(data, &saved)).To(Succeed())
				Expect(saved["execution"]).To(Equal("cancelled"))
				Expect(string(data)).NotTo(ContainSubstring("PUBLICATION_FAILED"))
				partial, err := os.ReadFile(filepath.Join(filepath.Dir(reports[0]), "partial.yaml"))
				Expect(err).NotTo(HaveOccurred())
				Expect(partial).NotTo(BeEmpty())
				after, err := os.ReadFile(source)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				_, err = os.Stat(output)
				Expect(os.IsNotExist(err)).To(BeTrue())
			}
		}
	})

	It("retains a failed publication in final and fallback reports after a concurrent source edit", func() {
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		source, _ := canceledDiscoverySeed(root)
		output := filepath.Join(root, "discovered.yaml")
		var stdout bytes.Buffer
		stderr := &publicationConflictWriter{source: source}
		op, err := NewDiscoverAdapter(Request{Inventory: source, OutputPath: output, ExplicitOutput: true, Stdout: &stdout, Stderr: stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: "json"}})
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(apperrors.ExitCodeFromError(op.Run(ctx))).To(Equal(1))
		Expect(stderr.edited).To(BeTrue())
		Expect(stderr.err).NotTo(HaveOccurred())
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["execution"]).To(Equal("failed"))
		Expect(stdout.String()).To(ContainSubstring("PUBLICATION_FAILED"))
		Expect(stderr.String()).To(ContainSubstring("[FAIL] Publication incomplete"))
		reports, err := filepath.Glob(filepath.Join(root, "discovered.discovery", "*", "report.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reports).To(HaveLen(1))
		saved, err := os.ReadFile(reports[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(string(saved)).To(ContainSubstring("PUBLICATION_FAILED"))
		_, err = os.Stat(output)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("keeps generation text, progress and publication intact through the real app input/render path", func() {
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		source := filepath.Join(root, "inventory.yaml")
		Expect(os.WriteFile(source, publicFixtureBytes(generationSite()), 0600)).To(Succeed())
		output := filepath.Join(root, "bundle")
		var stdout, stderr bytes.Buffer
		d := generateDriver{request: GenerateRequest{Inventory: source, OutputPath: output, Stdout: &stdout, Stderr: &stderr}, fetchChart: func(_ context.Context, address string) ([]byte, error) {
			if strings.HasSuffix(address, "dpf-operator-v26.8.0.tgz") {
				return os.ReadFile("../../dpf/bootstrap/testdata/operator/dpf-operator-v26.8.0.tgz")
			}
			return os.ReadFile("../../dpf/generation/testdata/doca-hbn-3.5.0.tgz")
		}}
		op, err := d.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "text"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(context.Background())).To(Succeed())
		Expect(stdout.String()).To(Equal("HBN-BGP manifests: " + output + "\nDeployment readiness: not evaluated by generation; deployment requires fresh prerequisite checks.\n"))
		Expect(stderr.String()).To(ContainSubstring("[RUNNING] Generation: resolving profile"))
		Expect(stderr.String()).To(ContainSubstring("[RUNNING] Charts:"))
		Expect(stderr.String()).To(ContainSubstring("[PASS] Generation: 7 stages saved to " + output))
		_, err = bundle.Load(output)
		Expect(err).NotTo(HaveOccurred())
		stderr.Reset()
		d.request.Stdout = failedWriter{}
		op, err = d.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate, Output: target.OutputOptions{Format: "json", Quiet: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(op.Run(context.Background())).To(HaveOccurred())
		Expect(stderr.String()).To(BeEmpty())
		_, err = bundle.Load(output)
		Expect(err).NotTo(HaveOccurred())
	})

	It("preserves deployment pre-run classification and emits no fabricated result before bundle or plan validation", func() {
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		_, retained := generatedUseCaseSite(deploymentSite())
		index := retained.Index()
		index.Profile, index.BundleDigest = "unsupported", ""
		unsupported, err := bundle.New(index, retained.Files())
		Expect(err).NotTo(HaveOccurred())
		invalidPlan := filepath.Join(root, "invalid-plan")
		_, err = unsupported.Publish(context.Background(), invalidPlan)
		Expect(err).NotTo(HaveOccurred())
		for _, test := range []struct{ path, message, suggestion string }{
			{filepath.Join(root, "missing"), "invalid DPF deployment bundle", "Regenerate the complete bundle and retain its metadata"},
			{invalidPlan, "invalid retained DPF deployment plan", "Regenerate and review the bundle before opening access"},
		} {
			for _, format := range []string{"json", "text"} {
				var stdout, stderr bytes.Buffer
				op, err := NewDeployAdapter(DeployRequest{Inventory: filepath.Join(root, "must-not-open"), DeploymentFiles: test.path, Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Deploy, Output: target.OutputOptions{Format: format}})
				Expect(err).NotTo(HaveOccurred())
				failure, ok := op.Run(context.Background()).(*apperrors.StructuredError)
				Expect(ok).To(BeTrue())
				Expect(failure.Message).To(Equal(test.message))
				Expect(failure.Suggestion).To(Equal(test.suggestion))
				Expect(failure.ExitCode).To(Equal(2))
				Expect(stdout.Len()).To(BeZero())
				Expect(stderr.String()).To(Equal("[RUNNING] Deployment: validating the retained bundle and site access\n"))
			}
		}
		reports, err := filepath.Glob(filepath.Join(root, "l8k-dpf-deploy-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reports).To(BeEmpty())
	})

	It("sanitizes preparation progress after app loading while retaining JSON's original escaped problem", func() {
		root := GinkgoT().TempDir()
		var stdout, stderr bytes.Buffer
		op, err := NewDiscoverAdapter(Request{Inventory: filepath.Join(root, "missing\n\x1b-input"), Stdout: &stdout, Stderr: &stderr}).Bind(target.Invocation{Target: target.DPF, Phase: target.Discover, Output: target.OutputOptions{Format: "json"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(apperrors.ExitCodeFromError(op.Run(context.Background()))).To(Equal(2))
		Expect(stderr.String()).NotTo(ContainSubstring("\x1b"))
		Expect(strings.Count(stderr.String(), "\n")).To(Equal(2))
		var result map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &result)).To(Succeed())
		Expect(result["execution"]).To(Equal("failed"))
		problems := result["errors"].([]any)
		Expect(problems[0].(map[string]any)["message"]).To(ContainSubstring("missing\n\x1b-input"))
	})
})
