// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF report publication", func() {
	var base, bundle, target string

	BeforeEach(func() {
		base = GinkgoT().TempDir()
		bundle = filepath.Join(base, "retained")
		Expect(os.Mkdir(bundle, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(bundle, "intent.yaml"), []byte("retained intent"), 0o600)).To(Succeed())
		target = filepath.Join(base, "report.html")
	})

	It("publishes each default report privately beside the retained bundle", func() {
		var prior string
		for range 2 {
			destination, err := validation.PrepareReportDestination(bundle, "", nil)
			Expect(err).NotTo(HaveOccurred())
			report, err := destination.Publish(writeReport)
			Expect(err).NotTo(HaveOccurred())
			Expect(destination.Close()).To(Succeed())
			Expect(report.Status).To(Equal(validation.ReportPublished))
			Expect(report.Path).NotTo(Equal(prior))
			prior = report.Path
			Expect(filepath.Base(report.Path)).To(Equal("validation.html"))
			parent := filepath.Dir(report.Path)
			resolvedBase, err := filepath.EvalSymlinks(base)
			Expect(err).NotTo(HaveOccurred())
			Expect(filepath.Dir(parent)).To(Equal(resolvedBase))
			Expect(filepath.Base(parent)).To(HavePrefix("retained-validation-"))
			Expect(fileContents(report.Path)).To(Equal("<p>report</p>"))
			if runtime.GOOS != "windows" {
				fileInfo, err := os.Stat(report.Path)
				Expect(err).NotTo(HaveOccurred())
				Expect(fileInfo.Mode().Perm()).To(Equal(os.FileMode(0o600)))
				parentInfo, err := os.Stat(parent)
				Expect(err).NotTo(HaveOccurred())
				Expect(parentInfo.Mode().Perm()).To(Equal(os.FileMode(0o700)))
			}
		}
		Expect(directoryNames(bundle)).To(Equal([]string{"intent.yaml"}))
		Expect(fileContents(filepath.Join(bundle, "intent.yaml"))).To(Equal("retained intent"))
	})

	It("skips all destination checks and writing when disabled", func() {
		destination, err := validation.PrepareReportDestination("", "-", []string{"missing"})
		Expect(err).NotTo(HaveOccurred())
		report, err := destination.Publish(func(io.Writer) error {
			Fail("disabled publication called the renderer")
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportDisabled}))
		Expect(destination.Close()).To(Succeed())
	})

	It("preserves the previous report on render failure and replaces it on retry", func() {
		Expect(os.WriteFile(target, []byte("previous complete report"), 0o600)).To(Succeed())
		destination, err := validation.PrepareReportDestination(bundle, target, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(destination.Close)
		renderErr := errors.New("private renderer cause")
		report, err := destination.Publish(func(w io.Writer) error {
			Expect(io.WriteString(w, "partial report")).Error().NotTo(HaveOccurred())
			return renderErr
		})
		Expect(err).To(MatchError(renderErr))
		Expect(err.Error()).NotTo(ContainSubstring(renderErr.Error()))
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportFailed}))
		Expect(fileContents(target)).To(Equal("previous complete report"))
		Expect(directoryNames(base)).To(ConsistOf("retained", "report.html"))

		report, err = destination.Publish(writeReport)
		Expect(err).NotTo(HaveOccurred())
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportPublished, Path: target}))
		Expect(fileContents(target)).To(Equal("<p>report</p>"))
		Expect(directoryNames(base)).To(ConsistOf("retained", "report.html"))
	})

	It("rejects an explicit missing report parent without creating it", func() {
		missing := filepath.Join(base, "missing")
		destination, err := validation.PrepareReportDestination(bundle, filepath.Join(missing, "report.html"), nil)
		Expect(err).To(MatchError(ContainSubstring("parent directory must already exist")))
		Expect(destination).To(BeNil())
		Expect(os.Lstat(missing)).Error().To(MatchError(os.ErrNotExist))
	})

	It("rejects a report beneath the retained bundle through a symlink alias", func() {
		if runtime.GOOS == "windows" {
			Skip("symlinks require Windows privileges")
		}
		alias := filepath.Join(base, "bundle-alias")
		Expect(os.Symlink(bundle, alias)).To(Succeed())
		for _, dir := range []string{bundle, alias} {
			destination, err := validation.PrepareReportDestination(bundle, filepath.Join(dir, "report.html"), nil)
			Expect(err).To(MatchError(ContainSubstring("outside the deployment bundle")))
			Expect(destination).To(BeNil())
		}
		Expect(directoryNames(bundle)).To(Equal([]string{"intent.yaml"}))
	})

	It("rejects an output symlink or a nonregular destination", func() {
		Expect(os.Mkdir(target, 0o700)).To(Succeed())
		_, err := validation.PrepareReportDestination(bundle, target, nil)
		Expect(err).To(MatchError(ContainSubstring("regular file")))
		if runtime.GOOS == "windows" {
			return
		}
		Expect(os.Remove(target)).To(Succeed())
		Expect(os.Symlink(filepath.Join(bundle, "intent.yaml"), target)).To(Succeed())
		_, err = validation.PrepareReportDestination(bundle, target, nil)
		Expect(err).To(MatchError(ContainSubstring("regular file")))
		Expect(fileContents(filepath.Join(bundle, "intent.yaml"))).To(Equal("retained intent"))
	})

	It("rejects protected files directly, through hardlinks, and after replacement", func() {
		protected := filepath.Join(base, "private-access-file")
		Expect(os.WriteFile(protected, []byte("private material"), 0o600)).To(Succeed())
		_, err := validation.PrepareReportDestination(bundle, protected, []string{protected})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))

		Expect(os.Link(protected, target)).To(Succeed())
		_, err = validation.PrepareReportDestination(bundle, target, []string{protected})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
		Expect(os.Remove(target)).To(Succeed())

		destination, err := validation.PrepareReportDestination(bundle, target, []string{protected})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(destination.Close)
		Expect(os.Link(protected, target)).To(Succeed())
		report, err := destination.Publish(writeReport)
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportFailed}))
		Expect(fileContents(target)).To(Equal("private material"))
		Expect(fileContents(protected)).To(Equal("private material"))

		Expect(os.Remove(target)).To(Succeed())
		report, err = destination.Publish(func(w io.Writer) error {
			if err := writeReport(w); err != nil {
				return err
			}
			return os.Link(protected, target)
		})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportFailed}))
		Expect(fileContents(target)).To(Equal("private material"))
		Expect(directoryNames(base)).To(ConsistOf("retained", "private-access-file", "report.html"))
	})

	It("rejects writing the target of a dangling protected symlink", func() {
		if runtime.GOOS == "windows" {
			Skip("symlinks require Windows privileges")
		}
		protected := filepath.Join(base, "private-access-file")
		Expect(os.Symlink(filepath.Base(target), protected)).To(Succeed())
		_, err := validation.PrepareReportDestination(bundle, target, []string{protected})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
		Expect(os.Lstat(target)).Error().To(MatchError(os.ErrNotExist))
	})

	It("checks case and Unicode aliases for a protected path that is still missing", func() {
		protected := filepath.Join(base, "Café.html")
		alias := filepath.Join(base, "CAFÉ.HTML")
		_, err := validation.PrepareReportDestination(bundle, alias, []string{protected})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
	})

	It("resolves a dangling input link through its actual parent traversal", func() {
		if runtime.GOOS == "windows" {
			Skip("symlinks require Windows privileges")
		}
		inputDir := filepath.Join(base, "config")
		nested := filepath.Join(base, "nested")
		Expect(os.Mkdir(inputDir, 0o700)).To(Succeed())
		Expect(os.Mkdir(nested, 0o700)).To(Succeed())
		hop := filepath.Join(inputDir, "hop")
		Expect(os.Symlink(nested, hop)).To(Succeed())
		protected := filepath.Join(inputDir, "private-access-file")
		Expect(os.Symlink("hop/../report.html", protected)).To(Succeed())
		_, err := validation.PrepareReportDestination(bundle, target, []string{protected})
		Expect(err).To(MatchError(ContainSubstring("aliases a protected input")))
		Expect(os.Lstat(target)).Error().To(MatchError(os.ErrNotExist))
	})

	It("does not create a report in a destination moved during observation", func() {
		if runtime.GOOS == "windows" {
			Skip("symlinks require Windows privileges")
		}
		parent := filepath.Join(base, "publication")
		Expect(os.Mkdir(parent, 0o700)).To(Succeed())
		destination, err := validation.PrepareReportDestination(bundle, filepath.Join(parent, "report.html"), nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(destination.Close)

		moved := filepath.Join(bundle, "moved")
		Expect(os.Rename(parent, moved)).To(Succeed())
		Expect(os.Symlink(bundle, parent)).To(Succeed())
		report, err := destination.Publish(func(io.Writer) error {
			Fail("publication wrote after the destination parent moved")
			return nil
		})
		Expect(err).To(MatchError(ContainSubstring("parent directory changed")))
		Expect(report).To(Equal(validation.ReportDisposition{Status: validation.ReportFailed}))
		Expect(directoryNames(moved)).To(BeEmpty())
		Expect(directoryNames(bundle)).To(ConsistOf("intent.yaml", "moved"))
	})
})

func writeReport(w io.Writer) error {
	_, err := io.WriteString(w, "<p>report</p>")
	return err
}

func fileContents(path string) string {
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func directoryNames(path string) []string {
	entries, err := os.ReadDir(path)
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
