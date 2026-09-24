// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package configflags binds tagged options fields to pflag and converts the
// flags explicitly supplied by a caller into resolver inputs.
package configflags

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

// Scope identifies the command on which a config-backed flag is available.
type Scope string

const (
	ScopeRoot     Scope = "root"
	ScopeGenerate Scope = "generate"
	ScopeDiscover Scope = "discover"
)

// Definition is the schema derived from one tagged options.Options field.
type Definition struct {
	FieldName   string
	FlagName    string
	ValueType   string
	ConfigPaths []string
	Resolver    string
	Scopes      []Scope
	Usage       string
}

// Definitions returns every tagged config-backed flag definition.
func Definitions() []Definition {
	typeOfOptions := reflect.TypeOf(options.Options{})
	definitions := make([]Definition, 0)
	for i := 0; i < typeOfOptions.NumField(); i++ {
		field := typeOfOptions.Field(i)
		flagName := field.Tag.Get("flag")
		if flagName == "" {
			continue
		}
		definitions = append(definitions, Definition{
			FieldName:   field.Name,
			FlagName:    flagName,
			ValueType:   valueType(field.Type),
			ConfigPaths: splitTag(field.Tag.Get("config")),
			Resolver:    field.Tag.Get("resolve"),
			Scopes:      parseScopes(field.Tag.Get("scopes")),
			Usage:       field.Tag.Get("usage"),
		})
	}
	return definitions
}

// ConfigPathsForFlag returns the YAML paths declared for flagName.
func ConfigPathsForFlag(flagName string) []string {
	flagName = strings.TrimPrefix(flagName, "--")
	for _, definition := range Definitions() {
		if definition.FlagName == flagName {
			return append([]string(nil), definition.ConfigPaths...)
		}
	}
	return nil
}

// ValidateDefinitions checks the complete tagged schema. Bind calls it before
// registering any flags so a malformed addition fails at command startup
// instead of waiting until that flag is used.
func ValidateDefinitions() error {
	definitions := Definitions()
	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if _, exists := seen[definition.FlagName]; exists {
			return fmt.Errorf("duplicate config flag --%s", definition.FlagName)
		}
		seen[definition.FlagName] = struct{}{}
		if len(definition.ConfigPaths) == 0 {
			return fmt.Errorf("config flag --%s has no config path", definition.FlagName)
		}
		if len(definition.Scopes) == 0 {
			return fmt.Errorf("config flag --%s has no command scope", definition.FlagName)
		}
		if definition.Usage == "" {
			return fmt.Errorf("config flag --%s has no usage text", definition.FlagName)
		}
		for _, scope := range definition.Scopes {
			switch scope {
			case ScopeRoot, ScopeGenerate, ScopeDiscover:
			default:
				return fmt.Errorf("config flag --%s has unknown command scope %q", definition.FlagName, scope)
			}
		}
		if definition.Resolver != "" && !configinput.ValidResolver(definition.Resolver) {
			return fmt.Errorf("config flag --%s has unknown resolver %q", definition.FlagName, definition.Resolver)
		}
		for _, path := range definition.ConfigPaths {
			destinationType, err := configPathType(path)
			if err != nil {
				return fmt.Errorf("config flag --%s: %w", definition.FlagName, err)
			}
			if definition.Resolver == "" && !mappingTypesMatch(definition.ValueType, destinationType) {
				return fmt.Errorf("config flag --%s type %s cannot map to %s (%s)",
					definition.FlagName, definition.ValueType, path, destinationType)
			}
		}
	}
	return nil
}

// Bind registers every tagged config-backed field available in scope. Adding
// a supported scalar field to options.Options is sufficient to register it;
// no command-specific setter or resolver mapping is needed.
func Bind(flags *pflag.FlagSet, opts *options.Options, scope Scope) error {
	if flags == nil {
		return fmt.Errorf("flag set must not be nil")
	}
	if opts == nil {
		return fmt.Errorf("options must not be nil")
	}
	if err := ValidateDefinitions(); err != nil {
		return err
	}

	value := reflect.ValueOf(opts).Elem()
	typeOfOptions := value.Type()
	for i := 0; i < typeOfOptions.NumField(); i++ {
		structField := typeOfOptions.Field(i)
		definition, ok := definitionForField(structField)
		if !ok || !containsScope(definition.Scopes, scope) {
			continue
		}
		if flags.Lookup(definition.FlagName) != nil {
			return fmt.Errorf("config flag --%s is already registered", definition.FlagName)
		}
		if err := bindField(flags, definition, value.Field(i)); err != nil {
			return err
		}
	}
	return nil
}

// Collect records only flags that pflag reports as changed. This presence bit
// is what lets explicit false, zero, and empty values override YAML values.
func Collect(flags *pflag.FlagSet, opts *options.Options) error {
	if flags == nil {
		return fmt.Errorf("flag set must not be nil")
	}
	if opts == nil {
		return fmt.Errorf("options must not be nil")
	}

	opts.ConfigInputs = configinput.Values{}
	value := reflect.ValueOf(opts).Elem()
	typeOfOptions := value.Type()
	for i := 0; i < typeOfOptions.NumField(); i++ {
		structField := typeOfOptions.Field(i)
		definition, ok := definitionForField(structField)
		if !ok {
			continue
		}
		flag := flags.Lookup(definition.FlagName)
		if flag == nil || !flag.Changed {
			continue
		}

		field := value.Field(i)
		explicitValue, err := fieldInterface(field)
		if err != nil {
			return fmt.Errorf("collect --%s: %w", definition.FlagName, err)
		}
		appendInput(&opts.ConfigInputs, definition, explicitValue)
		markPresenceCompanion(value, structField.Name)
		if definition.Resolver == "spectrum-x" {
			opts.SpectrumX = true
		}
	}
	return nil
}

// Infer preserves the public Go API for callers that construct Options
// directly. CLI code should use Collect because only pflag can distinguish an
// omitted scalar from an explicit zero value.
func Infer(opts options.Options) configinput.Values {
	if !opts.ConfigInputs.Empty() {
		return opts.ConfigInputs
	}

	var inputs configinput.Values
	value := reflect.ValueOf(opts)
	typeOfOptions := value.Type()
	for i := 0; i < typeOfOptions.NumField(); i++ {
		structField := typeOfOptions.Field(i)
		definition, ok := definitionForField(structField)
		if !ok || !fieldWasSet(value, structField, i) {
			continue
		}
		explicitValue, err := fieldInterface(value.Field(i))
		if err != nil {
			continue
		}
		appendInput(&inputs, definition, explicitValue)
	}
	return inputs
}

func definitionForField(field reflect.StructField) (Definition, bool) {
	flagName := field.Tag.Get("flag")
	if flagName == "" {
		return Definition{}, false
	}
	return Definition{
		FieldName:   field.Name,
		FlagName:    flagName,
		ValueType:   valueType(field.Type),
		ConfigPaths: splitTag(field.Tag.Get("config")),
		Resolver:    field.Tag.Get("resolve"),
		Scopes:      parseScopes(field.Tag.Get("scopes")),
		Usage:       field.Tag.Get("usage"),
	}, true
}

func valueType(valueType reflect.Type) string {
	if valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	switch valueType.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int:
		return "int"
	case reflect.Slice:
		if valueType.Elem().Kind() == reflect.String {
			return "[]string"
		}
	}
	return valueType.String()
}

func bindField(flags *pflag.FlagSet, definition Definition, field reflect.Value) error {
	switch field.Kind() {
	case reflect.String:
		flags.StringVar(field.Addr().Interface().(*string), definition.FlagName, field.String(), definition.Usage)
	case reflect.Bool:
		flags.BoolVar(field.Addr().Interface().(*bool), definition.FlagName, field.Bool(), definition.Usage)
	case reflect.Int:
		flags.IntVar(field.Addr().Interface().(*int), definition.FlagName, int(field.Int()), definition.Usage)
	case reflect.Slice:
		if field.Type().Elem().Kind() != reflect.String {
			return unsupportedType(definition, field.Type())
		}
		flags.StringSliceVar(field.Addr().Interface().(*[]string), definition.FlagName, nil, definition.Usage)
	case reflect.Pointer:
		if field.Type().Elem().Kind() != reflect.Bool {
			return unsupportedType(definition, field.Type())
		}
		flags.Var(&optionalBoolValue{field: field}, definition.FlagName, definition.Usage)
		flag := flags.Lookup(definition.FlagName)
		flag.NoOptDefVal = "true"
	default:
		return unsupportedType(definition, field.Type())
	}
	return nil
}

type optionalBoolValue struct {
	field reflect.Value
}

func (value *optionalBoolValue) Set(raw string) error {
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("invalid boolean %q: %w", raw, err)
	}
	value.field.Set(reflect.ValueOf(&parsed))
	return nil
}

func (value *optionalBoolValue) String() string {
	if !value.field.IsValid() || value.field.IsNil() {
		return "false"
	}
	return strconv.FormatBool(value.field.Elem().Bool())
}

func (*optionalBoolValue) Type() string {
	return "bool"
}

func (*optionalBoolValue) IsBoolFlag() bool {
	return true
}

func unsupportedType(definition Definition, valueType reflect.Type) error {
	return fmt.Errorf("config flag --%s uses unsupported field type %s", definition.FlagName, valueType)
}

func appendInput(inputs *configinput.Values, definition Definition, value any) {
	if definition.Resolver != "" {
		inputs.Requests = append(inputs.Requests, configinput.Request{
			Flag:  definition.FlagName,
			Kind:  definition.Resolver,
			Value: value,
		})
		return
	}
	for _, path := range definition.ConfigPaths {
		inputs.Overrides = append(inputs.Overrides, configinput.Override{
			Flag:  definition.FlagName,
			Path:  path,
			Value: cloneValue(value),
		})
	}
}

func fieldInterface(field reflect.Value) (any, error) {
	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return nil, fmt.Errorf("explicit pointer value is nil")
		}
		return field.Elem().Interface(), nil
	}
	return cloneValue(field.Interface()), nil
}

func cloneValue(value any) any {
	if values, ok := value.([]string); ok {
		clone := make([]string, len(values))
		copy(clone, values)
		return clone
	}
	return value
}

func fieldWasSet(value reflect.Value, field reflect.StructField, index int) bool {
	current := value.Field(index)
	if companion := value.FieldByName(field.Name + "Set"); companion.IsValid() && companion.Kind() == reflect.Bool {
		return companion.Bool()
	}
	if field.Tag.Get("resolve") == "spectrum-x" {
		return value.FieldByName("SpectrumX").Bool() || current.String() != ""
	}
	switch current.Kind() {
	case reflect.String:
		return current.String() != ""
	case reflect.Bool:
		return current.Bool()
	case reflect.Int:
		return current.Int() != 0
	case reflect.Slice:
		return current.Len() > 0
	case reflect.Pointer:
		return !current.IsNil()
	default:
		return false
	}
}

func markPresenceCompanion(value reflect.Value, fieldName string) {
	companion := value.FieldByName(fieldName + "Set")
	if companion.IsValid() && companion.CanSet() && companion.Kind() == reflect.Bool {
		companion.SetBool(true)
	}
}

func splitTag(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseScopes(value string) []Scope {
	parts := splitTag(value)
	out := make([]Scope, 0, len(parts))
	for _, part := range parts {
		out = append(out, Scope(part))
	}
	return out
}

func containsScope(scopes []Scope, want Scope) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func configPathType(path string) (reflect.Type, error) {
	if path == "" {
		return nil, fmt.Errorf("config path must not be empty")
	}
	current := reflect.TypeOf(config.LaunchKitConfig{})
	for _, part := range strings.Split(path, ".") {
		for current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return nil, fmt.Errorf("config path %q traverses non-struct %s", path, current)
		}
		field, ok := taggedField(current, part)
		if !ok {
			return nil, fmt.Errorf("config path %q does not exist", path)
		}
		current = field.Type
	}
	return current, nil
}

func mappingTypesMatch(valueType string, destination reflect.Type) bool {
	for destination.Kind() == reflect.Pointer {
		destination = destination.Elem()
	}
	switch valueType {
	case "string":
		return destination.Kind() == reflect.String
	case "bool":
		return destination.Kind() == reflect.Bool
	case "int":
		return destination.Kind() == reflect.Int
	case "[]string":
		return destination.Kind() == reflect.Slice && destination.Elem().Kind() == reflect.String
	default:
		return false
	}
}

func taggedField(structType reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		tagName := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if tagName == "-" {
			continue
		}
		if tagName == "" {
			tagName = field.Name
		}
		if tagName == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}
