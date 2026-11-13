// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestBundle(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPF immutable bundle")
}

func fixture() (Index, []File) {
	return Index{
			FormatVersion: FormatVersion, Target: Target, Phase: Phase,
			Profile: "dpf-sentry", Release: "v26.8.0-rc.1", Renderer: "fixture-v1",
			SelectedDevices: []string{"device-a"}, BundleDigest: "",
			Files:      []FileRecord{{Path: ResolvedConfigPath, Role: ResolvedConfig, SHA256: ""}, {Path: "system/object.yaml", Role: Manifest, SHA256: ""}},
			Stages:     []Stage{{ID: "system", Destination: Management, Namespace: "dpf-system", Files: []string{"system/object.yaml"}, ProjectionFiles: nil, DependsOn: nil, Requires: nil}},
			References: nil, ExternalPrerequisites: nil,
		}, []File{
			{Path: ResolvedConfigPath, Data: []byte("formatVersion: v1alpha1\ntarget: dpf\nconfig:\n  explicitFalse: false\n  explicitEmpty: []\n")},
			{Path: "system/object.yaml", Data: []byte("# exact comment\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: example-config, namespace: dpf-system}\ndata: {value: original}\n")},
		}
}

var _ = Describe("validated immutable membership", func() {
	It("versions resolved metadata independently of the bundle index", func() {
		for _, version := range []string{"v1alpha1", "v1alpha2", "v99"} {
			index, files := fixture()
			files[0].Data = []byte(strings.Replace(string(files[0].Data), "v1alpha1", version, 1))
			b, err := New(index, files)
			if version == "v99" {
				Expect(err).To(MatchError(ContainSubstring("unsupported DPF resolved configuration envelope")))
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(b.Index().FormatVersion).To(Equal("v1alpha1"))
			}
		}
	})

	It("retains exact bytes and isolates constructor and consumer mutations", func() {
		index, files := fixture()
		b, err := New(index, files)
		Expect(err).NotTo(HaveOccurred())
		original := append([]byte(nil), files[1].Data...)
		files[1].Data[0] = '!'
		index.Stages[0].Files[0] = "changed"
		copy := b.Files()
		copy[1].Data[0] = '!'
		copyIndex := b.Index()
		copyIndex.Stages[0].Files[0] = "changed"
		stage, ok := b.Stage("system")
		Expect(ok).To(BeTrue())
		doc := stage.Documents()[0]
		object := doc.ObjectCopy()
		object.SetName("mutated")
		Expect(doc.ObjectCopy().GetName()).To(Equal("example-config"))
		Expect(b.Files()[1].Data).To(Equal(original))
		Expect(b.Index().Stages[0].Files).To(Equal([]string{"system/object.yaml"}))
		Expect(doc.Source().File).To(Equal("system/object.yaml"))
		Expect(doc.Source().Document).To(Equal(1))
	})

	DescribeTable("rejects incomplete or unsupported input", func(mutate func(*Index, *[]File), message string) {
		index, files := fixture()
		mutate(&index, &files)
		_, err := New(index, files)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(message))
	},
		Entry("unknown envelope", func(i *Index, _ *[]File) { i.Target = "host" }, "unsupported"),
		Entry("unknown version", func(i *Index, _ *[]File) { i.FormatVersion = "v99" }, "unsupported"),
		Entry("no selection", func(i *Index, _ *[]File) { i.SelectedDevices = nil }, "no selected devices"),
		Entry("repeated selection", func(i *Index, _ *[]File) { i.SelectedDevices = []string{"device-a", "device-a"} }, "duplicate selected device"),
		Entry("unknown destination", func(i *Index, _ *[]File) { i.Stages[0].Destination = "ambient" }, "destination"),
		Entry("traversal", func(i *Index, f *[]File) { i.Files[1].Path = "../object.yaml"; (*f)[1].Path = "../object.yaml" }, "invalid relative"),
		Entry("path alias", func(i *Index, f *[]File) { i.Files[1].Path = "system/../object.yaml"; (*f)[1].Path = i.Files[1].Path }, "invalid relative"),
		Entry("duplicate path", func(_ *Index, f *[]File) { *f = append(*f, (*f)[1]) }, "duplicate DPF bundle file"),
		Entry("unindexed bytes", func(_ *Index, f *[]File) { *f = append(*f, File{Path: "extra.txt", Data: []byte("extra")}) }, "unindexed"),
		Entry("missing file", func(_ *Index, f *[]File) { *f = (*f)[:1] }, "missing"),
		Entry("wrong checksum", func(i *Index, _ *[]File) { i.Files[1].SHA256 = "sha256:wrong" }, "checksum"),
		Entry("wrong bundle checksum", func(i *Index, _ *[]File) { i.BundleDigest = "sha256:wrong" }, "bundle digest"),
		Entry("wrong sidecar", func(_ *Index, f *[]File) {
			(*f)[0].Data = []byte("formatVersion: v1alpha1\ntarget: host\nconfig: {}\n")
		}, "envelope"),
		Entry("forward stage", func(i *Index, _ *[]File) { i.Stages[0].DependsOn = []string{"later"} }, "earlier stage"),
		Entry("self dependency", func(i *Index, _ *[]File) { i.Stages[0].DependsOn = []string{"system"} }, "earlier stage"),
		Entry("missing prerequisite declaration", func(i *Index, _ *[]File) { i.Stages[0].Requires = []string{"credential-ready"} }, "undeclared external"),
		Entry("missing namespace", func(_ *Index, f *[]File) {
			(*f)[1].Data = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: test}\n")
		}, "explicit namespace"),
		Entry("wrong namespace", func(i *Index, _ *[]File) { i.Stages[0].Namespace = "elsewhere" }, "explicit namespace"),
		Entry("unknown kind", func(_ *Index, f *[]File) {
			(*f)[1].Data = []byte("apiVersion: unknown.io/v1\nkind: Unknown\nmetadata: {name: test, namespace: dpf-system}\n")
		}, "unsupported resource kind"),
		Entry("namespace on cluster kind", func(_ *Index, f *[]File) {
			(*f)[1].Data = []byte("apiVersion: v1\nkind: Namespace\nmetadata: {name: test, namespace: dpf-system}\n")
		}, "cluster-scoped"),
		Entry("malformed final document", func(_ *Index, f *[]File) { (*f)[1].Data = append((*f)[1].Data, []byte("---\nmetadata: [broken\n")...) }, "document 2"),
	)

	It("permits same resource names on different destination clusters but rejects same-cluster duplicates across API versions", func() {
		index, files := fixture()
		files = append(files, File{Path: "tenant/object.yaml", Data: append([]byte(nil), files[1].Data...)})
		index.Files = append(index.Files, FileRecord{Path: "tenant/object.yaml", Role: Manifest, SHA256: ""})
		index.Stages = append(index.Stages, Stage{ID: "tenant", Destination: DPU, Namespace: "dpf-system", Files: []string{"tenant/object.yaml"}, DependsOn: []string{"system"}})
		_, err := New(index, files)
		Expect(err).NotTo(HaveOccurred())
		index.Stages[1].Destination = Management
		files[2].Data = []byte(strings.ReplaceAll(string(files[2].Data), "apiVersion: v1", "apiVersion: v2"))
		_, err = New(index, files)
		Expect(err).To(MatchError(ContainSubstring("duplicate resource")))
	})

	It("checks projection identities while keeping them out of apply documents", func() {
		index, files := fixture()
		index.Files = append(index.Files, FileRecord{Path: ".l8k/helm/component/projection.yaml", Role: Projection})
		index.Stages[0].ProjectionFiles = []string{".l8k/helm/component/projection.yaml"}
		files = append(files, File{Path: ".l8k/helm/component/projection.yaml", Data: []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: chart-account, namespace: dpf-system}\n")})
		b, err := New(index, files)
		Expect(err).NotTo(HaveOccurred())
		stage, _ := b.Stage("system")
		Expect(stage.Documents()).To(HaveLen(1))
		Expect(stage.Projections()).To(HaveLen(1))
		Expect(stage.Projections()[0].IsProjection()).To(BeTrue())
		files[2].Data = append([]byte(nil), files[1].Data...)
		_, err = New(index, files)
		Expect(err).To(MatchError(ContainSubstring("duplicate resource")))
	})

	It("validates declared internal references and separates external prerequisites", func() {
		index, files := fixture()
		ref := ResourceRef{Destination: Management, Group: "", Kind: "ConfigMap", Namespace: "dpf-system", Name: "example-config"}
		index.References = []Reference{{From: ref, To: ref}}
		index.ExternalPrerequisites = []string{"credential-ready"}
		index.Stages[0].Requires = []string{"credential-ready"}
		_, err := New(index, files)
		Expect(err).NotTo(HaveOccurred())
		index.References[0].To.Name = "absent"
		_, err = New(index, files)
		Expect(err).To(MatchError(ContainSubstring("target")))
		index.References[0].To = ref
		index.References[0].To.Destination = DPU
		_, err = New(index, files)
		Expect(err).To(MatchError(ContainSubstring("crosses cluster")))
	})
})

var _ = Describe("closed disk loading and publication", func() {
	var dir string
	var b *Bundle
	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "dpf-bundle-")
		Expect(err).NotTo(HaveOccurred())
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		index, files := fixture()
		b, err = New(index, files)
		Expect(err).NotTo(HaveOccurred())
	})

	It("publishes privately, reloads identically and does not rewrite an identical existing bundle", func() {
		dest := filepath.Join(dir, "published")
		published, err := b.Publish(context.Background(), dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(published.Unchanged).To(BeFalse())
		loaded, err := Load(dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(sameBytes(b, loaded)).To(BeTrue())
		st, err := os.Stat(filepath.Join(dest, IndexPath))
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()).To(Equal(os.FileMode(0600)))
		rootInfo, err := os.Stat(dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(rootInfo.Mode().Perm()).To(Equal(os.FileMode(0700)))
		published, err = b.Publish(context.Background(), dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(published.Unchanged).To(BeTrue())
		after, err := os.Stat(filepath.Join(dest, IndexPath))
		Expect(err).NotTo(HaveOccurred())
		Expect(after.ModTime()).To(Equal(st.ModTime()))
	})

	It("rejects tampered bytes, added files, and file or directory symlinks", func() {
		dest := filepath.Join(dir, "published")
		_, err := b.Publish(context.Background(), dest)
		Expect(err).NotTo(HaveOccurred())
		manifest := filepath.Join(dest, "system/object.yaml")
		original, err := os.ReadFile(manifest)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(manifest, append(original, '\n'), 0600)).To(Succeed())
		_, err = Load(dest)
		Expect(err).To(MatchError(ContainSubstring("checksum")))
		Expect(os.WriteFile(manifest, original, 0600)).To(Succeed())
		extra := filepath.Join(dest, "extra.txt")
		Expect(os.WriteFile(extra, []byte("unindexed"), 0600)).To(Succeed())
		_, err = Load(dest)
		Expect(err).To(MatchError(ContainSubstring("unindexed")))
		Expect(os.Remove(extra)).To(Succeed())
		Expect(os.Remove(manifest)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "external"), original, 0600)).To(Succeed())
		Expect(os.Symlink(filepath.Join(dir, "external"), manifest)).To(Succeed())
		_, err = Load(dest)
		Expect(err).To(MatchError(ContainSubstring("symlink")))
		Expect(os.RemoveAll(filepath.Join(dest, "system"))).To(Succeed())
		Expect(os.Symlink(dir, filepath.Join(dest, "system"))).To(Succeed())
		_, err = Load(dest)
		Expect(err).To(MatchError(ContainSubstring("symlink")))
	})

	It("preserves existing empty, partial and differing destinations", func() {
		empty := filepath.Join(dir, "empty")
		Expect(os.Mkdir(empty, 0700)).To(Succeed())
		_, err := b.Publish(context.Background(), empty)
		Expect(err).To(HaveOccurred())
		entries, err := os.ReadDir(empty)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
		partial := filepath.Join(dir, "partial")
		Expect(os.Mkdir(partial, 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(partial, "keep.txt"), []byte("retained"), 0600)).To(Succeed())
		_, err = b.Publish(context.Background(), partial)
		Expect(err).To(HaveOccurred())
		retained, err := os.ReadFile(filepath.Join(partial, "keep.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(retained)).To(Equal("retained"))
		dest := filepath.Join(dir, "published")
		_, err = b.Publish(context.Background(), dest)
		Expect(err).NotTo(HaveOccurred())
		index, files := fixture()
		files[1].Data = append(files[1].Data, []byte("# differing observation\n")...)
		other, err := New(index, files)
		Expect(err).NotTo(HaveOccurred())
		_, err = other.Publish(context.Background(), dest)
		Expect(err).To(MatchError(ContainSubstring("differs")))
		loaded, err := Load(dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(sameBytes(b, loaded)).To(BeTrue())
	})

	It("honors cancellation while waiting on the destination lock without publishing", func() {
		dest := filepath.Join(dir, "cancelled")
		lock := flock.New(dest + ".lock")
		Expect(lock.Lock()).To(Succeed())
		defer lock.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := b.Publish(ctx, dest)
		Expect(err).To(HaveOccurred())
		_, err = os.Lstat(dest)
		Expect(os.IsNotExist(err)).To(BeTrue())
		staged, err := filepath.Glob(filepath.Join(dir, ".*.staging-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(staged).To(BeEmpty())
	})

	It("serializes concurrent publishers and returns one verified unchanged result", func() {
		dest := filepath.Join(dir, "concurrent")
		type result struct {
			publication Publication
			err         error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() { p, err := b.Publish(context.Background(), dest); results <- result{p, err} }()
		}
		unchanged := 0
		for range 2 {
			r := <-results
			Expect(r.err).NotTo(HaveOccurred())
			if r.publication.Unchanged {
				unchanged++
			}
		}
		Expect(unchanged).To(Equal(1))
		loaded, err := Load(dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(sameBytes(b, loaded)).To(BeTrue())
	})

	It("uses no-replace publication even when the destination is concurrently created empty", func() {
		from, to := filepath.Join(dir, "staging"), filepath.Join(dir, "destination")
		Expect(os.Mkdir(from, 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(from, "complete"), []byte("retained"), 0600)).To(Succeed())
		Expect(os.Mkdir(to, 0700)).To(Succeed())
		Expect(renameNewDirectory(from, to)).NotTo(Succeed())
		entries, err := os.ReadDir(to)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
		_, err = os.Stat(filepath.Join(from, "complete"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes only its own partial staging when cancellation follows the first write", func() {
		dest := filepath.Join(dir, "interrupted")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		interrupted := &cancelAfterStagedIndex{Context: ctx, cancel: cancel, parent: dir}
		_, err := b.Publish(interrupted, dest)
		Expect(err).To(MatchError(context.Canceled))
		Expect(interrupted.observed).To(BeTrue())
		_, err = os.Stat(dest)
		Expect(os.IsNotExist(err)).To(BeTrue())
		staged, err := filepath.Glob(filepath.Join(dir, ".*.staging-*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(staged).To(BeEmpty())
	})

	It("rejects oversized regular files without reading their content", func() {
		dest := filepath.Join(dir, "oversized")
		_, err := b.Publish(context.Background(), dest)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Truncate(filepath.Join(dest, "system/object.yaml"), maxFileBytes+1)).To(Succeed())
		_, err = Load(dest)
		Expect(err).To(MatchError(ContainSubstring("bounded regular file")))
	})
})

// Cancel at the first checkpoint after actual staging I/O, without timing races.
type cancelAfterStagedIndex struct {
	context.Context
	cancel   context.CancelFunc
	parent   string
	observed bool
}

func (c *cancelAfterStagedIndex) Err() error {
	staged, _ := filepath.Glob(filepath.Join(c.parent, ".*.staging-*"))
	for _, dir := range staged {
		if _, err := os.Stat(filepath.Join(dir, IndexPath)); err == nil {
			c.observed = true
			c.cancel()
		}
	}
	return c.Context.Err()
}
