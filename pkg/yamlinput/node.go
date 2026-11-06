// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package yamlinput provides target-neutral source-preserving YAML operations.
package yamlinput

import (
	"bytes"
	"fmt"
	yaml3 "gopkg.in/yaml.v3"
)

func Expand(node *yaml3.Node) (*yaml3.Node, error) {
	budget := 200000
	return expand(node, map[*yaml3.Node]bool{}, &budget)
}
func expand(node *yaml3.Node, active map[*yaml3.Node]bool, budget *int) (*yaml3.Node, error) {
	*budget--
	if *budget < 0 {
		return nil, fmt.Errorf("expanded YAML exceeds node limit")
	}
	if node == nil {
		return nil, fmt.Errorf("missing YAML node")
	}
	if active[node] {
		return nil, fmt.Errorf("cyclic YAML alias")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml3.AliasNode {
		return expand(node.Alias, active, budget)
	}
	copy := *node
	copy.Anchor = ""
	copy.Content = nil
	for _, child := range node.Content {
		expanded, err := expand(child, active, budget)
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

func Encode(node *yaml3.Node) ([]byte, error) {
	var b bytes.Buffer
	e := yaml3.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(node); err != nil {
		return nil, err
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func Set(mapping *yaml3.Node, key string, value *yaml3.Node) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			value.HeadComment = mapping.Content[i+1].HeadComment
			value.LineComment = mapping.Content[i+1].LineComment
			value.FootComment = mapping.Content[i+1].FootComment
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml3.Node{Kind: yaml3.ScalarNode, Tag: "!!str", Value: key}, value)
}
