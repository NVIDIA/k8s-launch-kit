// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"gopkg.in/yaml.v3"
)

func docaHostChoicePresence(present config.PathSet, opts options.Options) config.PathSet {
	choice := config.PathSet{}
	if present.Has("docaDriver.enable") {
		choice.Add("docaDriver.enable")
	}
	for _, override := range configflags.Infer(opts).Overrides {
		choice.Add(override.Path)
	}
	return choice
}

// Fresh discovery normally persists resolved defaults. Mixed host evidence
// requires a subsequent owner choice, so do not serialize the automatic true
// as if it were an explicit user decision on the next generation run.
func omitDOCAHostDriverChoice(data []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) > 0 {
		root := document.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "docaDriver" {
				continue
			}
			driver := root.Content[i+1]
			for j := 0; j+1 < len(driver.Content); j += 2 {
				if driver.Content[j].Value == "enable" {
					driver.Content = append(driver.Content[:j], driver.Content[j+2:]...)
					break
				}
			}
		}
	}
	var result bytes.Buffer
	encoder := yaml.NewEncoder(&result)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}
