// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	"github.com/mitchellh/copystructure"
	yamlv2 "gopkg.in/yaml.v2"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
)

// PathSet records YAML leaf paths that were explicitly supplied. A null node
// is deliberately absent: null means "unset" and remains eligible for lower
// precedence defaults.
type PathSet map[string]struct{}

// Has reports whether path was explicitly supplied.
func (p PathSet) Has(path string) bool {
	_, ok := p[path]
	return ok
}

// Add records path as explicitly supplied.
func (p PathSet) Add(path string) {
	if path != "" {
		p[path] = struct{}{}
	}
}

// DeletePrefix removes path and all of its children.
func (p PathSet) DeletePrefix(path string) {
	for candidate := range p {
		if candidate == path || strings.HasPrefix(candidate, path+".") {
			delete(p, candidate)
		}
	}
}

// Input is the raw, presence-aware form of a YAML configuration. Parsing does
// not normalize or validate values; those operations belong at the end of the
// resolver pipeline so they see the effective configuration.
type Input struct {
	Config     *LaunchKitConfig
	Present    PathSet
	SourcePath string
	SourceYAML []byte
}

// DecodeInput parses source without applying defaults or normalization.
func DecodeInput(source []byte, sourcePath string) (*Input, error) {
	var cfg LaunchKitConfig
	if err := yamlv2.Unmarshal(source, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse cluster config YAML %s: %w", displaySource(sourcePath), err)
	}

	var document yamlv3.Node
	if err := yamlv3.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("failed to inspect cluster config YAML %s: %w", displaySource(sourcePath), err)
	}
	expanded, err := expandYAMLNode(&document, map[*yamlv3.Node]bool{})
	if err != nil {
		return nil, fmt.Errorf("failed to expand cluster config YAML %s: %w", displaySource(sourcePath), err)
	}
	present := PathSet{}
	collectPresentPaths(expanded, "", present)

	return &Input{
		Config:     &cfg,
		Present:    present,
		SourcePath: sourcePath,
		SourceYAML: append([]byte(nil), source...),
	}, nil
}

// LoadInput reads a user config. An empty path represents an empty user layer;
// Resolve supplies the canonical embedded defaults as a separate, lower
// precedence layer.
func LoadInput(path string, logger logr.Logger) (*Input, error) {
	if path == "" {
		logger.Info("No user cluster configuration provided")
		return DecodeInput([]byte("{}\n"), "<no user config>")
	}
	logger.Info("Loading cluster configuration", "path", path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("cluster config file does not exist: %s", path)
		}
		return nil, fmt.Errorf("failed to read cluster config file %s: %w", path, err)
	}
	return DecodeInput(data, path)
}

// DefaultInput returns a new raw view of the canonical defaults.
func DefaultInput() (*Input, error) {
	return DecodeInput(DefaultConfigYAML(), "embedded cluster-config.yaml")
}

// CloneConfig returns a deep copy that does not share pointer, slice, or map
// storage with cfg.
func CloneConfig(cfg *LaunchKitConfig) (*LaunchKitConfig, error) {
	if cfg == nil {
		return &LaunchKitConfig{}, nil
	}
	copy, err := copystructure.Copy(cfg)
	if err != nil {
		return nil, fmt.Errorf("copy config: %w", err)
	}
	clone, ok := copy.(*LaunchKitConfig)
	if !ok {
		return nil, fmt.Errorf("copy config returned %T", copy)
	}
	return clone, nil
}

// CloneInput returns an independent presence-aware input.
func CloneInput(input *Input) (*Input, error) {
	if input == nil {
		return nil, fmt.Errorf("input must not be nil")
	}
	cfg, err := CloneConfig(input.Config)
	if err != nil {
		return nil, err
	}
	present := PathSet{}
	for path := range input.Present {
		present.Add(path)
	}
	return &Input{
		Config:     cfg,
		Present:    present,
		SourcePath: input.SourcePath,
		SourceYAML: append([]byte(nil), input.SourceYAML...),
	}, nil
}

// ApplyOverrides assigns direct CLI mappings using YAML field paths.
func ApplyOverrides(cfg *LaunchKitConfig, overrides []configinput.Override) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	for _, override := range overrides {
		if err := SetPath(cfg, override.Path, override.Value); err != nil {
			return fmt.Errorf("apply --%s to %s: %w", override.Flag, override.Path, err)
		}
	}
	return nil
}

// SetPath assigns value to a struct field selected through yaml tags. Pointer
// parents and scalar pointer fields are allocated on demand. Values must be
// assignable to the destination type and are deep-copied before assignment.
func SetPath(cfg *LaunchKitConfig, path string, value any) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	parts := strings.Split(path, ".")
	if len(parts) == 0 || parts[0] == "" {
		return fmt.Errorf("config path must not be empty")
	}
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
			if err := assignValue(field, value); err != nil {
				return err
			}
			markStructBoolPresence(current, field)
			return nil
		}
		current = field
	}
	return nil
}

// MergeDefaults fills omitted zero-valued fields from defaults. Explicit YAML
// paths and values already supplied by hardware or CLI resolution are kept.
// clusterConfig is never defaulted because hardware inventory must come from
// discovery, --for, or the user's file.
func MergeDefaults(cfg, defaults *LaunchKitConfig, present PathSet) error {
	if cfg == nil || defaults == nil {
		return fmt.Errorf("config and defaults must not be nil")
	}
	copy, err := CloneConfig(defaults)
	if err != nil {
		return err
	}
	return mergeDefaultValue(reflect.ValueOf(cfg).Elem(), reflect.ValueOf(copy).Elem(), "", present)
}

// NormalizeResolvedConfig materializes computed values and validates sections
// whose rules are independent of the selected deployment profile. Call it once
// after precedence resolution; calling it while layers are still being merged
// can reject a partial layer that a higher-precedence layer would complete.
func NormalizeResolvedConfig(cfg *LaunchKitConfig, source string) error {
	return NormalizeResolvedConfigWithPresence(cfg, source, nil)
}

// NormalizeResolvedConfigWithPresence normalizes a resolved configuration
// without replacing explicit zero values that final validation must reject.
func NormalizeResolvedConfigWithPresence(cfg *LaunchKitConfig, source string, present PathSet) error {
	if cfg == nil {
		return fmt.Errorf("config must not be nil")
	}
	if cfg.Profile != nil && cfg.Profile.SpectrumX != nil {
		if err := NormalizeSpectrumXProfileConfig(cfg.Profile.SpectrumX); err != nil {
			return fmt.Errorf("invalid spectrum-x profile config in %s: %w", displaySource(source), err)
		}
	}
	if err := NormalizeMaintenance(cfg); err != nil {
		return fmt.Errorf("invalid maintenance config in %s: %w", displaySource(source), err)
	}
	if err := validateDOCADriverConfig(cfg.DOCADriver); err != nil {
		return fmt.Errorf("invalid docaDriver config in %s: %w", displaySource(source), err)
	}
	ApplyNvIpamDefaultsWithPresence(cfg, present)
	if cfg.NvIpam != nil && cfg.NvIpam.PerNodeBlockSize == 0 && present.Has("nvIpam.perNodeBlockSize") {
		return fmt.Errorf("invalid nvIpam config in %s: nvIpam.perNodeBlockSize must be > 0, got 0",
			displaySource(source))
	}
	if err := validateNvIpam(cfg.NvIpam); err != nil {
		return fmt.Errorf("invalid nvIpam config in %s: %w", displaySource(source), err)
	}
	cfg.Validation = NormalizeValidationConfigWithPresence(cfg.Validation, present)
	if err := ValidateValidationConfig(cfg.Validation); err != nil {
		return fmt.Errorf("invalid validation config in %s: %w", displaySource(source), err)
	}
	if cfg.NvIpam != nil && len(cfg.NvIpam.Subnets) > 0 {
		if err := ApplyReservedExclusions(
			cfg.NvIpam.Subnets, cfg.NvIpam.ReserveFirstIPs, cfg.NvIpam.ReserveLastIPs); err != nil {
			return fmt.Errorf("invalid nvIpam config in %s: %w", displaySource(source), err)
		}
	}
	return nil
}

func collectPresentPaths(node *yamlv3.Node, prefix string, present PathSet) {
	if node == nil {
		return
	}
	if node.Kind == yamlv3.DocumentNode {
		for _, child := range node.Content {
			collectPresentPaths(child, prefix, present)
		}
		return
	}
	if node.Kind != yamlv3.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if isNullNode(value) {
			present.DeletePrefix(path)
			continue
		}
		switch value.Kind {
		case yamlv3.MappingNode:
			if len(value.Content) == 0 {
				present.Add(path)
			} else {
				collectPresentPaths(value, path, present)
			}
		default:
			present.Add(path)
		}
	}
}

func isNullNode(node *yamlv3.Node) bool {
	return node == nil || (node.Kind == yamlv3.ScalarNode && node.Tag == "!!null")
}

func mergeDefaultValue(target, fallback reflect.Value, path string, present PathSet) error {
	if !target.IsValid() || !fallback.IsValid() {
		return nil
	}
	if target.Kind() == reflect.Pointer || fallback.Kind() == reflect.Pointer {
		if fallback.Kind() != reflect.Pointer || fallback.IsNil() {
			return nil
		}
		if target.Kind() != reflect.Pointer {
			return fmt.Errorf("default type mismatch at %s", path)
		}
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		return mergeDefaultValue(target.Elem(), fallback.Elem(), path, present)
	}
	if target.Kind() == reflect.Struct && !isYAMLScalar(target) {
		for i := 0; i < target.NumField(); i++ {
			structField := target.Type().Field(i)
			name, persisted := yamlFieldName(structField)
			if !persisted || name == "clusterConfig" {
				continue
			}
			childPath := name
			if path != "" {
				childPath = path + "." + name
			}
			if err := mergeDefaultValue(target.Field(i), fallback.Field(i), childPath, present); err != nil {
				return err
			}
		}
		return nil
	}
	if present.Has(path) || !target.IsZero() || fallback.IsZero() {
		return nil
	}
	target.Set(fallback)
	return nil
}

func isYAMLScalar(value reflect.Value) bool {
	marker := reflect.TypeOf((*yamlScalar)(nil)).Elem()
	return value.Type().Implements(marker) || (value.CanAddr() && value.Addr().Type().Implements(marker))
}

// yamlScalar marks struct-backed values that merge as a single YAML leaf.
// MarshalYAML alone is insufficient: mapping types such as Profile also use
// custom marshaling to preserve presence metadata.
type yamlScalar interface {
	yamlScalar()
}

func yamlField(value reflect.Value, name string) (reflect.Value, bool) {
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		yamlName, persisted := yamlFieldName(field)
		if persisted && yamlName == name {
			return value.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func yamlFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("yaml")
	name := strings.Split(tag, ",")[0]
	if name == "-" {
		return "", false
	}
	if name == "" {
		name = field.Name
	}
	return name, true
}

func assignValue(destination reflect.Value, value any) error {
	if !destination.CanSet() {
		return fmt.Errorf("destination %s cannot be set", destination.Type())
	}
	source := reflect.ValueOf(value)
	if !source.IsValid() {
		return fmt.Errorf("value must not be nil")
	}
	if destination.Kind() == reflect.Pointer && source.Type().AssignableTo(destination.Type().Elem()) {
		if destination.IsNil() {
			destination.Set(reflect.New(destination.Type().Elem()))
		}
		return assignValue(destination.Elem(), value)
	}
	if source.Type().AssignableTo(destination.Type()) {
		copy, err := copystructure.Copy(value)
		if err != nil {
			return fmt.Errorf("copy override: %w", err)
		}
		destination.Set(reflect.ValueOf(copy))
		return nil
	}
	return fmt.Errorf("cannot assign %s to %s", source.Type(), destination.Type())
}

func markStructBoolPresence(parent, assigned reflect.Value) {
	if assigned.Kind() != reflect.Bool {
		return
	}
	for i := 0; i < parent.NumField(); i++ {
		if parent.Field(i).Addr().Pointer() != assigned.Addr().Pointer() {
			continue
		}
		companion := parent.FieldByName(parent.Type().Field(i).Name + "Set")
		if companion.IsValid() && companion.CanSet() && companion.Kind() == reflect.Bool {
			companion.SetBool(true)
		}
		return
	}
}

func displaySource(path string) string {
	if path == "" {
		return "<memory>"
	}
	return path
}
