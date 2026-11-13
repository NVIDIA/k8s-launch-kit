// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	shared "github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"gopkg.in/yaml.v3"
)

const (
	maxFileBytes   = 16 << 20
	maxBundleBytes = 128 << 20
	maxFiles       = 4096
)

// New validates complete membership, fills omitted file hashes and freezes copies.
// Explicit mismatching hashes or a mismatching BundleDigest are rejected.
func New(index Index, files []File) (*Bundle, error) {
	b, err := construct(index, files, false)
	if err != nil {
		return nil, err
	}
	b.indexBytes, err = yaml.Marshal(b.index)
	if err != nil {
		return nil, fmt.Errorf("encode DPF bundle index: %w", err)
	}
	return b, nil
}

func construct(index Index, files []File, requireHashes bool) (*Bundle, error) {
	index = copyIndex(index)
	if index.FormatVersion != FormatVersion || index.Target != Target || index.Phase != Phase {
		return nil, fmt.Errorf("unsupported DPF bundle format/target/phase %q/%q/%q", index.FormatVersion, index.Target, index.Phase)
	}
	if index.Profile == "" || index.Release == "" || index.Renderer == "" {
		return nil, fmt.Errorf("DPF bundle profile, release and renderer are required")
	}
	if len(index.SelectedDevices) == 0 {
		return nil, fmt.Errorf("DPF bundle has no selected devices")
	}
	if err := uniqueStrings("selected device", index.SelectedDevices); err != nil {
		return nil, err
	}
	if len(files) > maxFiles || len(index.Files) > maxFiles {
		return nil, fmt.Errorf("DPF bundle exceeds file count limit")
	}
	fileMap := make(map[string][]byte, len(files))
	total := 0
	for _, file := range files {
		if err := validPath(file.Path); err != nil {
			return nil, err
		}
		if file.Path == IndexPath {
			return nil, fmt.Errorf("bundle index cannot index itself")
		}
		if _, exists := fileMap[file.Path]; exists {
			return nil, fmt.Errorf("duplicate DPF bundle file %q", file.Path)
		}
		total += len(file.Data)
		if len(file.Data) > maxFileBytes || total > maxBundleBytes {
			return nil, fmt.Errorf("DPF bundle exceeds bounded file/content limits at %q", file.Path)
		}
		fileMap[file.Path] = bytes.Clone(file.Data)
	}
	records := make(map[string]FileRecord, len(index.Files))
	for n, record := range index.Files {
		if err := validPath(record.Path); err != nil {
			return nil, err
		}
		if !validRole(record.Role) {
			return nil, fmt.Errorf("unsupported file role %q for %q", record.Role, record.Path)
		}
		if _, exists := records[record.Path]; exists {
			return nil, fmt.Errorf("duplicate indexed file %q", record.Path)
		}
		data, exists := fileMap[record.Path]
		if !exists {
			return nil, fmt.Errorf("indexed file %q is missing", record.Path)
		}
		actual := digest(data)
		if record.SHA256 == "" && !requireHashes {
			record.SHA256 = actual
		}
		if record.SHA256 != actual {
			return nil, fmt.Errorf("checksum mismatch for %q", record.Path)
		}
		index.Files[n] = record
		records[record.Path] = record
	}
	if len(records) != len(fileMap) {
		return nil, fmt.Errorf("DPF bundle contains unindexed files")
	}
	for filename := range records {
		for parent := path.Dir(filename); parent != "."; parent = path.Dir(parent) {
			if _, exists := records[parent]; exists {
				return nil, fmt.Errorf("indexed file %q is also a parent directory of %q", parent, filename)
			}
		}
	}
	resolved, exists := records[ResolvedConfigPath]
	if !exists || resolved.Role != ResolvedConfig {
		return nil, fmt.Errorf("DPF bundle requires indexed resolved configuration at %s", ResolvedConfigPath)
	}
	for _, record := range records {
		if record.Role == ResolvedConfig && record.Path != ResolvedConfigPath {
			return nil, fmt.Errorf("resolved configuration has wrong path %q", record.Path)
		}
	}
	if err := validateResolved(fileMap[ResolvedConfigPath]); err != nil {
		return nil, err
	}
	sort.Slice(index.Files, func(i, j int) bool { return index.Files[i].Path < index.Files[j].Path })
	b := &Bundle{index: index}
	for _, record := range index.Files {
		b.files = append(b.files, File{Path: record.Path, Data: fileMap[record.Path]})
	}
	if err := b.validateStages(records, fileMap); err != nil {
		return nil, err
	}
	actual, err := indexDigest(b.index)
	if err != nil {
		return nil, err
	}
	if index.BundleDigest != "" && index.BundleDigest != actual {
		return nil, fmt.Errorf("DPF bundle digest mismatch")
	}
	if requireHashes && index.BundleDigest == "" {
		return nil, fmt.Errorf("DPF bundle digest is missing")
	}
	b.index.BundleDigest = actual
	return b, nil
}

func validPath(p string) error {
	if p == "" || p == "." || len(p) > 1024 || strings.Count(p, "/") > 32 || strings.ContainsAny(p, "\\\x00:") || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." {
		return fmt.Errorf("invalid relative DPF bundle path %q", p)
	}
	return nil
}

func validRole(role Role) bool {
	switch role {
	case Manifest, HelmValues, Chart, Projection, ResolvedConfig, Metadata:
		return true
	default:
		return false
	}
}

func uniqueStrings(label string, values []string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return fmt.Errorf("empty or duplicate %s %q", label, value)
		}
		seen[value] = true
	}
	return nil
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func indexDigest(index Index) (string, error) {
	index.BundleDigest = ""
	data, err := json.Marshal(index)
	if err != nil {
		return "", fmt.Errorf("encode DPF bundle digest payload: %w", err)
	}
	return digest(data), nil
}

func decodeOne(data []byte, value any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple YAML documents are unsupported")
		}
		return err
	}
	return nil
}

func validateResolved(data []byte) error {
	var envelope struct {
		FormatVersion string    `yaml:"formatVersion"`
		Target        string    `yaml:"target"`
		Config        yaml.Node `yaml:"config"`
		References    yaml.Node `yaml:"references"`
	}
	if err := decodeOne(data, &envelope); err != nil {
		return fmt.Errorf("decode DPF resolved configuration: %w", err)
	}
	if envelope.FormatVersion != FormatVersion || envelope.Target != Target || envelope.Config.Kind != yaml.MappingNode {
		return fmt.Errorf("unsupported DPF resolved configuration envelope")
	}
	return nil
}

func (b *Bundle) validateStages(records map[string]FileRecord, files map[string][]byte) error {
	if len(b.index.Stages) == 0 {
		return fmt.Errorf("DPF bundle has no stages")
	}
	if err := uniqueStrings("external prerequisite", b.index.ExternalPrerequisites); err != nil {
		return err
	}
	external := map[string]bool{}
	for _, id := range b.index.ExternalPrerequisites {
		external[id] = true
	}
	seenStages := map[string]bool{}
	usedFiles := map[string]bool{}
	objects := map[ResourceRef]string{}
	for _, stage := range b.index.Stages {
		if stage.ID == "" || seenStages[stage.ID] {
			return fmt.Errorf("empty or duplicate DPF stage %q", stage.ID)
		}
		if stage.Destination != Management && stage.Destination != DPU {
			return fmt.Errorf("unsupported destination %q in stage %q", stage.Destination, stage.ID)
		}
		if err := uniqueStrings("stage dependency", stage.DependsOn); err != nil {
			return err
		}
		for _, id := range stage.DependsOn {
			if !seenStages[id] {
				return fmt.Errorf("stage %q dependency %q is not an earlier stage", stage.ID, id)
			}
		}
		if err := uniqueStrings("stage prerequisite", stage.Requires); err != nil {
			return err
		}
		for _, id := range stage.Requires {
			if !external[id] {
				return fmt.Errorf("stage %q has undeclared external prerequisite %q", stage.ID, id)
			}
		}
		if len(stage.Files) == 0 && len(stage.ProjectionFiles) == 0 {
			return fmt.Errorf("stage %q has no files", stage.ID)
		}
		view := StageView{stage: copyStage(stage)}
		var resources []stageResource
		paths := append(append([]string(nil), stage.Files...), stage.ProjectionFiles...)
		if err := uniqueStrings("stage file", paths); err != nil {
			return err
		}
		for n, filename := range paths {
			record, exists := records[filename]
			if !exists {
				return fmt.Errorf("stage %q uses unindexed file %q", stage.ID, filename)
			}
			isProjection := n >= len(stage.Files)
			if isProjection != (record.Role == Projection) {
				return fmt.Errorf("stage %q has wrong projection role for %q", stage.ID, filename)
			}
			if record.Role == ResolvedConfig || record.Role == Metadata {
				return fmt.Errorf("non-executable file %q cannot belong to stage %q", filename, stage.ID)
			}
			if usedFiles[filename] {
				return fmt.Errorf("file %q belongs to multiple stages", filename)
			}
			usedFiles[filename] = true
			switch record.Role {
			case HelmValues:
				if _, err := shared.ParseValues(files[filename]); err != nil {
					return fmt.Errorf("%s: %w", filename, err)
				}
			case Manifest, Projection:
				resources = append(resources, stageResource{path: filename, data: files[filename], projection: isProjection})
			}
		}
		docs, err := parseStageResources(stage, resources)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			if first, exists := objects[doc.ref]; exists {
				return fmt.Errorf("duplicate resource %v at %s document %d (first at %s)", doc.ref, doc.source.File, doc.source.Document, first)
			}
			objects[doc.ref] = fmt.Sprintf("%s document %d", doc.source.File, doc.source.Document)
			if doc.projection {
				view.projections = append(view.projections, doc)
			} else {
				view.documents = append(view.documents, doc)
			}
		}
		b.stages = append(b.stages, view)
		seenStages[stage.ID] = true
	}
	for _, record := range records {
		if record.Role != Metadata && record.Role != ResolvedConfig && !usedFiles[record.Path] {
			return fmt.Errorf("executable file %q has no stage", record.Path)
		}
	}
	for _, reference := range b.index.References {
		if reference.From.Destination != reference.To.Destination {
			return fmt.Errorf("internal resource reference crosses cluster destinations")
		}
		if _, exists := objects[reference.From]; !exists {
			return fmt.Errorf("reference source %v is missing", reference.From)
		}
		if _, exists := objects[reference.To]; !exists {
			return fmt.Errorf("reference target %v is missing", reference.To)
		}
	}
	return nil
}

type stageResource struct {
	path       string
	data       []byte
	projection bool
}

func parseStageResources(stage Stage, resources []stageResource) ([]Document, error) {
	// Adapt nested paths to the shared flat parser once per stage. Numeric flat
	// names preserve declared order without inheriting Host filename-role rules.
	files := make([]shared.File, 0, len(resources))
	sources := map[string]stageResource{}
	for n, resource := range resources {
		name := fmt.Sprintf("resource-%08d.yaml", n)
		files = append(files, shared.File{Name: name, Content: string(resource.data)})
		sources[name] = resource
	}
	parsed, err := shared.FromFiles(files)
	if err != nil {
		var sourceErr *shared.Error
		if errors.As(err, &sourceErr) {
			source := sourceErr.Source
			source.File = sources[source.File].path
			reason := sourceErr.Reason
			for name, resource := range sources {
				reason = strings.ReplaceAll(reason, name, resource.path)
			}
			return nil, &shared.Error{Source: source, Reason: reason, Err: sourceErr.Err}
		}
		return nil, err
	}
	var docs []Document
	counts := map[string]int{}
	for _, item := range parsed.Documents() {
		resource := sources[item.Source().File]
		counts[resource.path]++
		ref := item.Ref()
		namespaced, known := kindScope[ref.Group+"/"+ref.Kind]
		if !known {
			return nil, fmt.Errorf("%s document %d: unsupported resource kind %s/%s", resource.path, item.Source().Document, ref.Group, ref.Kind)
		}
		if namespaced && (ref.Namespace == "" || stage.Namespace != "" && ref.Namespace != stage.Namespace) {
			return nil, fmt.Errorf("%s document %d: explicit namespace must match stage %q", resource.path, item.Source().Document, stage.ID)
		}
		if !namespaced && ref.Namespace != "" {
			return nil, fmt.Errorf("%s document %d: cluster-scoped kind %s cannot have a namespace", resource.path, item.Source().Document, ref.Kind)
		}
		docs = append(docs, Document{ref: ResourceRef{Destination: stage.Destination, Group: ref.Group, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}, source: shared.Source{File: resource.path, Document: item.Source().Document}, projection: resource.projection, object: item.ObjectCopy()})
	}
	for _, resource := range resources {
		if counts[resource.path] == 0 {
			return nil, fmt.Errorf("manifest %q has no resource documents", resource.path)
		}
	}
	return docs, nil
}
