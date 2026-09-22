// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	// EffectiveConfigRelativePath lives outside the manifest directory so YAML
	// scanners cannot mistake resolver metadata for a Kubernetes resource.
	EffectiveConfigRelativePath = ".l8k/resolved-config.yaml"
	effectiveConfigAPIVersion   = "launchkit.nvidia.com/v1alpha1"
	effectiveConfigKind         = "ResolvedConfig"
)

type effectiveConfigEnvelope struct {
	APIVersion       string           `yaml:"apiVersion"`
	Kind             string           `yaml:"kind"`
	EmptyCollections []string         `yaml:"emptyCollections,omitempty"`
	Config           *LaunchKitConfig `yaml:"config"`
}

// EffectiveConfigPath returns the metadata path for a deployment bundle root.
func EffectiveConfigPath(deploymentRoot string) string {
	return filepath.Join(deploymentRoot, EffectiveConfigRelativePath)
}

// IsEffectiveConfigPath reports whether path uses the reserved bundle metadata
// location. It also permits callers to pass that sidecar explicitly.
func IsEffectiveConfigPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean == EffectiveConfigRelativePath ||
		len(clean) > len(EffectiveConfigRelativePath) &&
			clean[len(clean)-len(EffectiveConfigRelativePath)-1:] == "/"+EffectiveConfigRelativePath
}

// WriteEffectiveConfig atomically writes the exact configuration used to
// render a deployment bundle.
func WriteEffectiveConfig(deploymentRoot string, cfg *LaunchKitConfig) (string, error) {
	if deploymentRoot == "" {
		return "", fmt.Errorf("deployment root must not be empty")
	}
	if cfg == nil {
		return "", fmt.Errorf("effective config must not be nil")
	}
	data, err := yaml.Marshal(effectiveConfigEnvelope{
		APIVersion:       effectiveConfigAPIVersion,
		Kind:             effectiveConfigKind,
		EmptyCollections: emptyCollectionPaths(reflect.ValueOf(cfg), ""),
		Config:           cfg,
	})
	if err != nil {
		return "", fmt.Errorf("marshal effective config: %w", err)
	}

	path := EffectiveConfigPath(deploymentRoot)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create effective config directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".resolved-config-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create effective config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write effective config temporary file: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set effective config permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close effective config temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("publish effective config: %w", err)
	}
	return path, nil
}

// LoadEffectiveConfig reads a resolved bundle without applying defaults,
// catalog expansion, or normalization a second time.
func LoadEffectiveConfig(path string) (*LaunchKitConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read effective config %s: %w", path, err)
	}
	var envelope effectiveConfigEnvelope
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse effective config %s: %w", path, err)
	}
	if envelope.APIVersion != effectiveConfigAPIVersion || envelope.Kind != effectiveConfigKind {
		return nil, fmt.Errorf("unsupported effective config %s %s", envelope.APIVersion, envelope.Kind)
	}
	if envelope.Config == nil {
		return nil, fmt.Errorf("effective config %s has no config", path)
	}
	for _, emptyPath := range envelope.EmptyCollections {
		if err := restoreEmptyCollection(envelope.Config, emptyPath); err != nil {
			return nil, fmt.Errorf("restore effective config %s field %s: %w", path, emptyPath, err)
		}
	}
	return envelope.Config, nil
}

func emptyCollectionPaths(value reflect.Value, path string) []string {
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Slice || value.Kind() == reflect.Map {
		if !value.IsNil() && value.Len() == 0 && path != "" {
			return []string{path}
		}
		return nil
	}
	if value.Kind() != reflect.Struct || isYAMLScalar(value) {
		return nil
	}
	var paths []string
	for i := 0; i < value.NumField(); i++ {
		name, persisted := yamlFieldName(value.Type().Field(i))
		if !persisted {
			continue
		}
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		paths = append(paths, emptyCollectionPaths(value.Field(i), childPath)...)
	}
	return paths
}

func restoreEmptyCollection(cfg *LaunchKitConfig, path string) error {
	parts := strings.Split(path, ".")
	current := reflect.ValueOf(cfg)
	for index, part := range parts {
		for current.Kind() == reflect.Pointer {
			if current.IsNil() {
				current.Set(reflect.New(current.Type().Elem()))
			}
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return fmt.Errorf("%q traverses non-struct %s", strings.Join(parts[:index], "."), current.Type())
		}
		field, ok := yamlField(current, part)
		if !ok {
			return fmt.Errorf("unknown config path %q", path)
		}
		if index == len(parts)-1 {
			switch field.Kind() {
			case reflect.Slice:
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			case reflect.Map:
				field.Set(reflect.MakeMap(field.Type()))
			default:
				return fmt.Errorf("field is %s, not a collection", field.Kind())
			}
			return nil
		}
		current = field
	}
	return nil
}
