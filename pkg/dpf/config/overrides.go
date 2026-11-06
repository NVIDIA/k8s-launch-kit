// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	yaml "gopkg.in/yaml.v3"
)

// WithOverrides patches a detached YAML tree using the shared flag metadata's
// paths. The normal strict DPF decoder validates the result. Source bytes and
// the caller's document are retained unchanged.
func WithOverrides(in Input, values configinput.Values) (Input, error) {
	if len(values.Requests) > 0 {
		return Input{}, fmt.Errorf("host configuration resolvers are not valid for DPF")
	}
	if values.Empty() {
		return in, nil
	}
	doc, err := yamlinput.Expand(in.Document)
	if err != nil {
		return Input{}, err
	}
	if doc == nil || len(doc.Content) != 1 {
		return Input{}, fmt.Errorf("missing DPF source document")
	}
	for _, o := range values.Overrides {
		parts := strings.Split(o.Path, ".")
		if len(parts) < 2 {
			return Input{}, fmt.Errorf("invalid DPF override path %q", o.Path)
		}
		n := doc.Content[0]
		for _, key := range parts[:len(parts)-1] {
			if key == "" || n.Kind != yaml.MappingNode {
				return Input{}, fmt.Errorf("invalid DPF override path %q", o.Path)
			}
			var child *yaml.Node
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Value == key {
					child = n.Content[i+1]
					break
				}
			}
			if child == nil || child.Tag == "!!null" {
				child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				yamlinput.Set(n, key, child)
			}
			n = child
		}
		if n.Kind != yaml.MappingNode || parts[len(parts)-1] == "" {
			return Input{}, fmt.Errorf("invalid DPF override path %q", o.Path)
		}
		var value yaml.Node
		if err = value.Encode(o.Value); err != nil {
			return Input{}, err
		}
		yamlinput.Set(n, parts[len(parts)-1], &value)
	}
	b, err := yamlinput.Encode(doc)
	if err != nil {
		return Input{}, err
	}
	out, err := Decode(b, in.Path)
	if err != nil {
		return Input{}, err
	}
	out.Source = append([]byte(nil), in.Source...)
	return out, nil
}
