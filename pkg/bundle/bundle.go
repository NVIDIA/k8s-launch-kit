// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

// Package bundle holds a validated, immutable snapshot of generated artifacts.
package bundle

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

type Role uint8

const (
	Deployment Role = iota
	Validation
)

type File struct {
	Name    string
	Content string
}

type Source struct {
	File     string
	Document int
}

type ObjectRef struct {
	Group, Kind, Namespace, Name string
}

type Document struct {
	source Source
	role   Role
	object *unstructured.Unstructured
}

type helmValues struct {
	content string
	values  map[string]any
}

type Bundle struct {
	files     []File
	documents []Document
	values    *helmValues
}

func (b *Bundle) Files() []File {
	if b == nil {
		return nil
	}
	return append([]File(nil), b.files...)
}

func (b *Bundle) Documents() []Document {
	if b == nil {
		return nil
	}
	return append([]Document(nil), b.documents...)
}

func (b *Bundle) ValuesCopy() (map[string]any, bool) {
	if b == nil || b.values == nil {
		return nil, false
	}
	return runtime.DeepCopyJSON(b.values.values), true
}

func (b *Bundle) ValuesContent() (string, bool) {
	if b == nil || b.values == nil {
		return "", false
	}
	return b.values.content, true
}

func (d Document) Source() Source { return d.source }
func (d Document) Role() Role     { return d.role }

func (d Document) ObjectCopy() *unstructured.Unstructured {
	if d.object == nil {
		return nil
	}
	return d.object.DeepCopy()
}

func (d Document) Ref() ObjectRef {
	if d.object == nil {
		return ObjectRef{}
	}
	gvk := d.object.GroupVersionKind()
	return ObjectRef{Group: gvk.Group, Kind: gvk.Kind, Namespace: d.object.GetNamespace(), Name: d.object.GetName()}
}

func IsExampleFilename(name string) bool {
	return strings.Contains(strings.ToLower(name), "example")
}

type Error struct {
	Source Source
	Reason string
	Err    error
}

func (e *Error) Error() string {
	location := e.Source.File
	if e.Source.Document > 0 {
		location = fmt.Sprintf("%s document %d", location, e.Source.Document)
	}
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", location, e.Reason)
	}
	return fmt.Sprintf("%s: %s: %v", location, e.Reason, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
