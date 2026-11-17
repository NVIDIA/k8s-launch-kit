// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package bundle freezes DPF inputs and ordered stages for later consumers.
// Structural validation does not establish API admission or runtime readiness.
package bundle

import (
	shared "github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	FormatVersion      = "v1alpha1"
	Target             = "dpf"
	Phase              = "generate"
	IndexPath          = ".l8k/bundle.yaml"
	ResolvedConfigPath = ".l8k/resolved-config.yaml"
)

// Destination names the cluster role, never a kubeconfig or ambient context.
type Destination string

const (
	Management Destination = "management"
	DPU        Destination = "dpu"
)

// Role determines how bytes participate in structural validation.
type Role string

const (
	Manifest       Role = "manifest"
	HelmValues     Role = "helm-values"
	Chart          Role = "chart"
	Projection     Role = "projection"
	ResolvedConfig Role = "resolved-config"
	Metadata       Role = "metadata"
)

// File is an owned relative path and its exact content. Constructors copy Data.
type File struct {
	Path string
	Data []byte
}

// FileRecord closes membership and binds every retained file except the index.
type FileRecord struct {
	Path   string `yaml:"path" json:"path"`
	Role   Role   `yaml:"role" json:"role"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// ResourceRef identifies one resource, ignoring its served API version.
type ResourceRef struct {
	Destination Destination `yaml:"destination" json:"destination"`
	Group       string      `yaml:"group" json:"group"`
	Kind        string      `yaml:"kind" json:"kind"`
	Namespace   string      `yaml:"namespace" json:"namespace"`
	Name        string      `yaml:"name" json:"name"`
}

// Reference is an internal same-cluster edge; external prerequisites use Requires.
type Reference struct {
	From ResourceRef `yaml:"from" json:"from"`
	To   ResourceRef `yaml:"to" json:"to"`
}

// Stage is a declarative ordered group, not an executable workflow language.
// ProjectionFiles are checked with manifests but must never be applied separately.
type Stage struct {
	ID              string      `yaml:"id" json:"id"`
	Destination     Destination `yaml:"destination" json:"destination"`
	Namespace       string      `yaml:"namespace" json:"namespace"`
	Files           []string    `yaml:"files" json:"files"`
	ProjectionFiles []string    `yaml:"projectionFiles,omitempty" json:"projectionFiles,omitempty"`
	DependsOn       []string    `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	Requires        []string    `yaml:"requires,omitempty" json:"requires,omitempty"`
}

// Index is the versioned DPF envelope. BundleDigest binds the canonical index
// payload and all exact file hashes, including metadata and observation bytes.
// It makes no promise of stability across refreshed observations.
type Index struct {
	FormatVersion         string       `yaml:"formatVersion" json:"formatVersion"`
	Target                string       `yaml:"target" json:"target"`
	Phase                 string       `yaml:"phase" json:"phase"`
	Profile               string       `yaml:"profile" json:"profile"`
	Release               string       `yaml:"release" json:"release"`
	Renderer              string       `yaml:"renderer" json:"renderer"`
	BundleDigest          string       `yaml:"bundleDigest" json:"bundleDigest"`
	SelectedDevices       []string     `yaml:"selectedDevices" json:"selectedDevices"`
	Files                 []FileRecord `yaml:"files" json:"files"`
	Stages                []Stage      `yaml:"stages" json:"stages"`
	References            []Reference  `yaml:"references,omitempty" json:"references,omitempty"`
	ExternalPrerequisites []string     `yaml:"externalPrerequisites,omitempty" json:"externalPrerequisites,omitempty"`
}

// Bundle retains one immutable, validated snapshot.
type Bundle struct {
	index      Index
	files      []File
	indexBytes []byte
	stages     []StageView
}

// Document carries source diagnostics and an isolated Kubernetes view.
type Document struct {
	ref        ResourceRef
	source     shared.Source
	projection bool
	object     *unstructured.Unstructured
}

func (d Document) Ref() ResourceRef      { return d.ref }
func (d Document) Source() shared.Source { return d.source }
func (d Document) IsProjection() bool    { return d.projection }
func (d Document) ObjectCopy() *unstructured.Unstructured {
	if d.object == nil {
		return nil
	}
	return d.object.DeepCopy()
}

// StageView separates apply documents from validation-only projections.
type StageView struct {
	stage       Stage
	documents   []Document
	projections []Document
}

func (s StageView) Stage() Stage            { return copyStage(s.stage) }
func (s StageView) Documents() []Document   { return append([]Document(nil), s.documents...) }
func (s StageView) Projections() []Document { return append([]Document(nil), s.projections...) }

// Publication identifies a fully validated published snapshot.
type Publication struct {
	Path      string
	Unchanged bool
}

func (b *Bundle) Index() Index {
	if b == nil {
		return Index{}
	}
	return copyIndex(b.index)
}

func (b *Bundle) Files() []File {
	if b == nil {
		return nil
	}
	return copyFiles(b.files)
}

func (b *Bundle) Stage(id string) (StageView, bool) {
	if b != nil {
		for _, stage := range b.stages {
			if stage.stage.ID == id {
				return stage, true
			}
		}
	}
	return StageView{}, false
}

func copyStage(s Stage) Stage {
	s.Files = append([]string(nil), s.Files...)
	s.ProjectionFiles = append([]string(nil), s.ProjectionFiles...)
	s.DependsOn = append([]string(nil), s.DependsOn...)
	s.Requires = append([]string(nil), s.Requires...)
	return s
}

func copyIndex(i Index) Index {
	i.SelectedDevices = append([]string(nil), i.SelectedDevices...)
	i.Files = append([]FileRecord(nil), i.Files...)
	i.Stages = append([]Stage(nil), i.Stages...)
	for n := range i.Stages {
		i.Stages[n] = copyStage(i.Stages[n])
	}
	i.References = append([]Reference(nil), i.References...)
	i.ExternalPrerequisites = append([]string(nil), i.ExternalPrerequisites...)
	return i
}

func copyFiles(files []File) []File {
	out := make([]File, len(files))
	for n, file := range files {
		out[n] = File{Path: file.Path, Data: append([]byte(nil), file.Data...)}
	}
	return out
}
