// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"fmt"
	"io"

	yaml "gopkg.in/yaml.v3"
)

const launchKitVersionAnnotation = "nvidia.kubernetes-launch-kit.version"

type annotationPostRenderer struct {
	version string
}

func (r annotationPostRenderer) Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error) {
	if renderedManifests == nil {
		return nil, fmt.Errorf("rendered manifests must not be nil")
	}
	annotated, err := annotateResources(renderedManifests.Bytes(), r.version)
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(annotated), nil
}

// annotateResources adds the Launch Kit version annotation to every resource
// in a YAML stream while preserving existing annotations.
func annotateResources(stream []byte, version string) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(stream))
	documents := []yaml.Node{}
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode YAML document %d: %w", len(documents)+1, err)
		}
		if len(document.Content) == 0 ||
			(document.Content[0].Kind == yaml.ScalarNode && document.Content[0].Tag == "!!null") {
			continue
		}
		documents = append(documents, document)
	}

	for i := range documents {
		root := documents[i].Content[0]
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("YAML document %d must contain a Kubernetes resource mapping", i+1)
		}
		metadata, ok := yamlMappingValue(root, "metadata")
		if !ok || metadata.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("YAML document %d must contain a metadata mapping", i+1)
		}
		annotations, ok := yamlMappingValue(metadata, "annotations")
		if !ok {
			annotations = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			metadata.Content = append(metadata.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "annotations"},
				annotations,
			)
		} else if annotations.Kind == yaml.ScalarNode && annotations.Tag == "!!null" {
			*annotations = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		} else if annotations.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("YAML document %d metadata.annotations must be a mapping", i+1)
		}
		setYAMLMappingString(annotations, launchKitVersionAnnotation, version)
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for i := range documents {
		if err := encoder.Encode(&documents[i]); err != nil {
			return nil, fmt.Errorf("encode annotated YAML document %d: %w", i+1, err)
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close annotated YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

func yamlMappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; mapping != nil && mapping.Kind == yaml.MappingNode && i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], true
		}
	}
	return nil, false
}

func setYAMLMappingString(mapping *yaml.Node, key, value string) {
	if current, ok := yamlMappingValue(mapping, key); ok {
		*current = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle},
	)
}
