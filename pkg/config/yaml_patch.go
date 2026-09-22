// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	yaml3 "gopkg.in/yaml.v3"
)

// expandYAMLNode produces an independent tree with aliases and merge keys
// expanded. YAML merges are shallow: an explicit mapping replaces the entire
// inherited mapping at that key. Earlier mappings in a merge sequence win.
// Sharing this expansion keeps presence tracking and source patches consistent.
func expandYAMLNode(node *yaml3.Node, active map[*yaml3.Node]bool) (*yaml3.Node, error) {
	if node == nil {
		return nil, fmt.Errorf("missing YAML node")
	}
	if active[node] {
		return nil, fmt.Errorf("cyclic YAML alias")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml3.AliasNode {
		return expandYAMLNode(node.Alias, active)
	}
	copy := *node
	copy.Anchor = ""
	copy.Content = nil
	for _, child := range node.Content {
		expanded, err := expandYAMLNode(child, active)
		if err != nil {
			return nil, err
		}
		copy.Content = append(copy.Content, expanded)
	}
	if copy.Kind != yaml3.MappingNode {
		return &copy, nil
	}
	entries := copy.Content
	copy.Content = nil
	for i := 0; i+1 < len(entries); i += 2 {
		if entries[i].Tag != "!!merge" {
			continue
		}
		mappings := []*yaml3.Node{entries[i+1]}
		if entries[i+1].Kind == yaml3.SequenceNode {
			mappings = entries[i+1].Content
		}
		for _, mapping := range mappings {
			if mapping.Kind != yaml3.MappingNode {
				return nil, fmt.Errorf("YAML merge value must be a mapping or sequence of mappings")
			}
			for j := 0; j+1 < len(mapping.Content); j += 2 {
				putYAMLField(&copy, mapping.Content[j], mapping.Content[j+1], false)
			}
		}
	}
	for i := 0; i+1 < len(entries); i += 2 {
		if entries[i].Tag != "!!merge" {
			putYAMLField(&copy, entries[i], entries[i+1], true)
		}
	}
	return &copy, nil
}

func putYAMLField(mapping, key, value *yaml3.Node, replace bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key.Value {
			if replace {
				mapping.Content[i], mapping.Content[i+1] = key, value
			}
			return
		}
	}
	mapping.Content = append(mapping.Content, key, value)
}

// PatchConfigYAML updates only the requested YAML paths, preserving all other
// source values, omissions, unknown keys, and comments. Aliases are expanded so
// editing a field cannot also change another field sharing its anchor.
func PatchConfigYAML(source []byte, cfg *LaunchKitConfig, paths []string, banner string) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	var document yaml3.Node
	if err := yaml3.Unmarshal(source, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		document = yaml3.Node{Kind: yaml3.DocumentNode, Content: []*yaml3.Node{{Kind: yaml3.MappingNode, Tag: "!!map"}}}
	}
	expanded, err := expandYAMLNode(&document, map[*yaml3.Node]bool{})
	if err != nil {
		return nil, err
	}
	if err := patchConfigPaths(expanded, cfg, paths); err != nil {
		return nil, err
	}
	if banner != "" {
		expanded.HeadComment = banner
	}
	var out bytes.Buffer
	encoder := yaml3.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(expanded); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func patchConfigPaths(document *yaml3.Node, cfg *LaunchKitConfig, paths []string) error {
	for _, path := range paths {
		value, err := configValueAtPath(reflect.ValueOf(cfg), path)
		if err != nil {
			return err
		}
		var replacement yaml3.Node
		if err := replacement.Encode(value.Interface()); err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
		patchYAMLPath(document.Content[0], strings.Split(path, "."), &replacement)
	}
	return nil
}

func configValueAtPath(value reflect.Value, path string) (reflect.Value, error) {
	for _, part := range strings.Split(path, ".") {
		for value.IsValid() && value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
		if !value.IsValid() || value.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("config path %q traverses an absent or non-struct field", path)
		}
		field, ok := yamlField(value, part)
		if !ok {
			return reflect.Value{}, fmt.Errorf("unknown config path %q", path)
		}
		value = field
	}
	return value, nil
}

func patchYAMLPath(node *yaml3.Node, parts []string, replacement *yaml3.Node) {
	if len(parts) == 0 {
		copyComments(node, replacement)
		*node = *replacement
		return
	}
	if node.Kind != yaml3.MappingNode {
		*node = yaml3.Node{Kind: yaml3.MappingNode, Tag: "!!map"}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == parts[0] {
			patchYAMLPath(node.Content[i+1], parts[1:], replacement)
			return
		}
	}
	value := &yaml3.Node{}
	node.Content = append(node.Content, &yaml3.Node{Kind: yaml3.ScalarNode, Tag: "!!str", Value: parts[0]}, value)
	patchYAMLPath(value, parts[1:], replacement)
}
