// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	"gopkg.in/yaml.v3"
)

var yamlErrorLine = regexp.MustCompile(`\bline ([0-9]+):`)
var yamlUnknownField = regexp.MustCompile(`\bfield (\S+) not found`)

// yamlDiagnostic deliberately omits the decoder's raw error: YAML type errors
// can include credential values. Only source coordinates and a field path are
// reported. Expanded nodes retain the source positions of aliases and merges.
func yamlDiagnostic(message, fieldPath string, node *yaml.Node) error {
	if fieldPath == "" {
		fieldPath = "/"
	}
	location := ""
	if node != nil && node.Line > 0 {
		location = fmt.Sprintf(" (line %d, column %d)", node.Line, node.Column)
	}
	return fmt.Errorf("%s at %s%s", message, strconv.QuoteToASCII(fieldPath), location)
}

func yamlSyntaxDiagnostic(err error) error {
	line := diagnosticLine(err)
	if line > 0 {
		return fmt.Errorf("invalid YAML at / (line %d); check the document syntax", line)
	}
	return fmt.Errorf("invalid YAML at /; check the document syntax")
}

func diagnosticLine(err error) int {
	if err == nil {
		return 0
	}
	match := yamlErrorLine.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}

// strictDiagnostic maps a decoder position in the normalized YAML back through
// its field path to the original expanded source. No source text or value is
// included in the diagnostic. Matching the unknown key disambiguates flow YAML.
func strictDiagnostic(err error, normalized []byte, source *yaml.Node) error {
	// Flow collections can put several keys on one decoder line. Reformat a
	// private copy only on failure, preserving source styles/coordinates and
	// using distinct block lines for reliable type-error paths.
	if block, encodeErr := yamlinput.Encode(diagnosticBlock(source)); encodeErr == nil {
		var site Site
		decoder := yaml.NewDecoder(bytes.NewReader(block))
		decoder.KnownFields(true)
		if blockErr := decoder.Decode(&site); blockErr != nil {
			err, normalized = blockErr, block
		}
	}
	message := "invalid type in DPF configuration"
	var normalizedDoc yaml.Node
	if yaml.Unmarshal(normalized, &normalizedDoc) != nil {
		return yamlDiagnostic(message, "", source)
	}
	field := ""
	if match := yamlUnknownField.FindStringSubmatch(err.Error()); len(match) == 2 {
		field = match[1]
		message = "unknown field in DPF configuration"
	}
	fieldPath, node := diagnosticLocation(&normalizedDoc, source, diagnosticLine(err), field, "")
	return yamlDiagnostic(message, fieldPath, node)
}

// Encode/decode preserves the expanded tree's order and shape. Walk the two
// trees together to retain original coordinates without maintaining a second
// path index or exposing any scalar value in a diagnostic.
func diagnosticLocation(normalized, source *yaml.Node, line int, field, fieldPath string) (string, *yaml.Node) {
	if normalized == nil || source == nil || line <= 0 || normalized.Kind != source.Kind || len(normalized.Content) != len(source.Content) {
		return "", nil
	}
	switch normalized.Kind {
	case yaml.DocumentNode:
		if len(normalized.Content) == 1 {
			return diagnosticLocation(normalized.Content[0], source.Content[0], line, field, fieldPath)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(normalized.Content); i += 2 {
			key, value := normalized.Content[i], normalized.Content[i+1]
			path := fieldPath + "/" + strings.ReplaceAll(strings.ReplaceAll(key.Value, "~", "~0"), "/", "~1")
			if key.Line == line && (field == "" || key.Value == field) {
				return path, source.Content[i]
			}
			if foundPath, found := diagnosticLocation(value, source.Content[i+1], line, field, path); found != nil {
				return foundPath, found
			}
		}
	case yaml.SequenceNode:
		for i, value := range normalized.Content {
			if foundPath, found := diagnosticLocation(value, source.Content[i], line, field, fieldPath+"/"+strconv.Itoa(i)); found != nil {
				return foundPath, found
			}
		}
	}
	return "", nil
}

// diagnosticBlock changes presentation only; Decode returns the untouched source
// tree for source-preserving configuration refreshes. Aliases are already expanded.
func diagnosticBlock(source *yaml.Node) *yaml.Node {
	if source == nil {
		return nil
	}
	copy := *source
	copy.Style &^= yaml.FlowStyle
	copy.Content = make([]*yaml.Node, len(source.Content))
	for i, child := range source.Content {
		copy.Content[i] = diagnosticBlock(child)
	}
	return &copy
}
