// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

func rawDocuments(content string, name string, visit func([]byte, Source) error) error {
	reader := kubeyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(content)))
	for ordinal := 1; ; ordinal++ {
		raw, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &Error{Source: Source{File: name, Document: ordinal}, Reason: "read YAML document", Err: err}
		}
		if err := visit(raw, Source{File: name, Document: ordinal}); err != nil {
			return err
		}
	}
}

// documentRoot validates syntax without converting scalar values. The semantic
// decoder receives the original bytes so Helm and Kubernetes keep their prior
// YAML interpretation.
func documentRoot(raw []byte, source Source) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err == io.EOF {
		return nil, nil
	} else if err != nil {
		return nil, &Error{Source: source, Reason: "decode YAML", Err: err}
	}
	var next yaml.Node
	if err := decoder.Decode(&next); err != io.EOF {
		if err == nil {
			return nil, &Error{Source: source, Reason: "multiple YAML documents in one chunk"}
		}
		return nil, &Error{Source: source, Reason: "trailing YAML content", Err: err}
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return doc.Content[0], nil
}

func decodeResources(file File, role Role) ([]Document, error) {
	var documents []Document
	err := rawDocuments(file.Content, file.Name, func(raw []byte, source Source) error {
		root, err := documentRoot(raw, source)
		if err != nil {
			return err
		}
		if root == nil || root.Tag == "!!null" {
			return nil
		}
		if root.Kind != yaml.MappingNode {
			return &Error{Source: source, Reason: "resource document must be an object mapping"}
		}
		obj := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal(raw, obj); err != nil {
			return &Error{Source: source, Reason: "decode Kubernetes object", Err: err}
		}
		apiVersion, ok, err := unstructured.NestedString(obj.Object, "apiVersion")
		if err != nil || !ok || apiVersion == "" {
			return &Error{Source: source, Reason: "apiVersion must be a nonempty string", Err: err}
		}
		kind, ok, err := unstructured.NestedString(obj.Object, "kind")
		if err != nil || !ok || kind == "" {
			return &Error{Source: source, Reason: "kind must be a nonempty string", Err: err}
		}
		metadata, ok, err := unstructured.NestedMap(obj.Object, "metadata")
		if err != nil || !ok {
			return &Error{Source: source, Reason: "metadata must be an object mapping", Err: err}
		}
		name, ok, err := unstructured.NestedString(metadata, "name")
		if err != nil || !ok || name == "" {
			reason := "metadata.name must be a nonempty string"
			if kind == "List" {
				reason += "; provide individual resource documents instead of a bare List"
			}
			return &Error{Source: source, Reason: reason, Err: err}
		}
		if _, exists := metadata["namespace"]; exists {
			if _, ok := metadata["namespace"].(string); !ok {
				return &Error{Source: source, Reason: "metadata.namespace must be a string"}
			}
		}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil || gv.Version == "" {
			return &Error{Source: source, Reason: fmt.Sprintf("invalid apiVersion %q", apiVersion), Err: err}
		}
		obj.SetGroupVersionKind(gv.WithKind(kind))
		documents = append(documents, Document{source: source, role: role, object: obj})
		return nil
	})
	return documents, err
}

func ParseValues(data []byte) (map[string]any, error) {
	return parseValues(string(data), "values.yaml")
}

func parseValues(content, name string) (map[string]any, error) {
	values := map[string]any{}
	seen := false
	err := rawDocuments(content, name, func(raw []byte, source Source) error {
		root, err := documentRoot(raw, source)
		if err != nil {
			return err
		}
		if root == nil || (root.Tag == "!!null" && root.Value == "" && root.Style == 0 && root.Anchor == "") {
			return nil
		}
		if seen {
			return &Error{Source: source, Reason: "values.yaml has multiple substantive documents"}
		}
		seen = true
		if root.Tag == "!!null" {
			return nil
		}
		if root.Kind != yaml.MappingNode {
			return &Error{Source: source, Reason: "values.yaml must contain an object mapping"}
		}
		if err := sigsyaml.Unmarshal(raw, &values); err != nil {
			return &Error{Source: source, Reason: "decode Helm values", Err: err}
		}
		if values == nil {
			values = map[string]any{}
		}
		return nil
	})
	return values, err
}
