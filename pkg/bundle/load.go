// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type fileKind uint8

const (
	ignoredFile fileKind = iota
	resourceFile
	valuesFile
)

func classifyFile(name string) (fileKind, Role, error) {
	if name == "cluster-config.yaml" || name == "cluster-config.yml" {
		return ignoredFile, Deployment, nil
	}
	if strings.EqualFold(name, "values.yaml") || strings.EqualFold(name, "values.yml") {
		if name != "values.yaml" {
			return ignoredFile, Deployment, fmt.Errorf("rename %s to values.yaml", name)
		}
		return valuesFile, Deployment, nil
	}
	if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
		return ignoredFile, Deployment, nil
	}
	if IsExampleFilename(name) {
		return resourceFile, Validation, nil
	}
	return resourceFile, Deployment, nil
}

func Load(fsys fs.FS) (*Bundle, error) {
	if fsys == nil {
		return nil, fmt.Errorf("load bundle: filesystem is nil")
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read artifact directory: %w", err)
	}
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		kind, _, classifyErr := classifyFile(entry.Name())
		if classifyErr != nil {
			return nil, &Error{Source: Source{File: entry.Name()}, Reason: "classify artifact", Err: classifyErr}
		}
		if kind == ignoredFile {
			continue
		}
		content, readErr := fs.ReadFile(fsys, entry.Name())
		if readErr != nil {
			return nil, &Error{Source: Source{File: entry.Name()}, Reason: "read artifact", Err: readErr}
		}
		files = append(files, File{Name: entry.Name(), Content: string(content)})
	}
	return FromFiles(files)
}

func FromFiles(files []File) (*Bundle, error) {
	ordered := append([]File(nil), files...)
	seenNames := make(map[string]struct{}, len(ordered))
	for _, file := range ordered {
		if file.Name == "" || file.Name == "." || file.Name == ".." || filepath.IsAbs(file.Name) || strings.ContainsAny(file.Name, `/\`) {
			return nil, &Error{Source: Source{File: file.Name}, Reason: "artifact name must be a flat basename"}
		}
		if _, exists := seenNames[file.Name]; exists {
			return nil, &Error{Source: Source{File: file.Name}, Reason: "duplicate filename"}
		}
		seenNames[file.Name] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	b := &Bundle{}
	seenObjects := map[ObjectRef]Source{}
	for _, file := range ordered {
		kind, role, err := classifyFile(file.Name)
		if err != nil {
			return nil, &Error{Source: Source{File: file.Name}, Reason: "classify artifact", Err: err}
		}
		if kind == ignoredFile {
			continue
		}
		b.files = append(b.files, file)
		if kind == valuesFile {
			values, parseErr := parseValues(file.Content, file.Name)
			if parseErr != nil {
				return nil, parseErr
			}
			b.values = &helmValues{content: file.Content, values: values}
			continue
		}
		docs, parseErr := decodeResources(file, role)
		if parseErr != nil {
			return nil, parseErr
		}
		for _, doc := range docs {
			ref := doc.Ref()
			if first, exists := seenObjects[ref]; exists {
				return nil, &Error{Source: doc.Source(), Reason: fmt.Sprintf("duplicate resource %s/%s in namespace %q (first declared at %s document %d)", ref.Kind, ref.Name, ref.Namespace, first.File, first.Document)}
			}
			seenObjects[ref] = doc.Source()
			b.documents = append(b.documents, doc)
		}
	}
	return b, nil
}
