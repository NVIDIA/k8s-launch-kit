// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

// Package validationreport renders the common document around a target's report body.
package validationreport

import (
	"embed"
	"fmt"
	"html/template"
	"time"
)

//go:embed report.html.tmpl
var shellTemplate string

// Page carries the presentation fields shared by validation reports.
type Page struct {
	Title       string
	Heading     string
	GeneratedAt time.Time
	Version     string
	Metadata    []Field
	Outcome     OutcomeView
}

// FormattedTime displays the report timestamp in UTC.
func (p Page) FormattedTime() string {
	return p.GeneratedAt.UTC().Format("2006-01-02 15:04:05 UTC")
}

// Field is a labeled metadata value in the report header.
type Field struct {
	Label string
	Value string
}

// OutcomeView describes a verdict already evaluated by the caller.
type OutcomeView struct {
	Label   string
	Style   OutcomeStyle
	Reasons []string
	Notes   []string
}

// OutcomeStyle selects a report banner color.
type OutcomeStyle uint8

const (
	OutcomeWarning OutcomeStyle = iota
	OutcomeSuccess
	OutcomeFailure
)

// String returns the CSS class suffix. Unrecognized styles use the warning color.
func (s OutcomeStyle) String() string {
	switch s {
	case OutcomeSuccess:
		return "pass"
	case OutcomeFailure:
		return "fail"
	default:
		return "warning"
	}
}

// View supplies the shell's Page and passes a typed Body to the target template.
type View[T any] struct {
	Page Page
	Body T
}

// NewTemplate loads the embedded target template and common document. The
// target file defines "body" and may define "target-style" for static CSS.
// Execute the returned template with a View containing the target's body data.
func NewTemplate(target embed.FS, filename string, funcs template.FuncMap) (*template.Template, error) {
	tmpl, err := template.New("validation-report").Funcs(funcs).ParseFS(target, filename)
	if err != nil {
		return nil, fmt.Errorf("parse target report template: %w", err)
	}
	if tmpl.Lookup("body") == nil {
		return nil, fmt.Errorf("report template %q must define body", filename)
	}
	tmpl, err = tmpl.Parse(shellTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse validation report shell: %w", err)
	}
	return tmpl, nil
}
