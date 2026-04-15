// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Output represents the user interface for CLI messages
type Output interface {
	// Info displays an informational message
	Info(format string, args ...interface{})
	// Success displays a success message
	Success(format string, args ...interface{})
	// Warning displays a warning message
	Warning(format string, args ...interface{})
	// Error displays an error message
	Error(format string, args ...interface{})
	// StartProgress starts a progress indicator for a long-running operation
	StartProgress(message string) Progress
	// Header displays a header banner
	Header(text string)
	// Section displays a section header
	Section(text string)
	// Confirm displays a yes/no prompt and returns the user's choice.
	// Returns true if the user confirms, false otherwise.
	// In non-interactive contexts (e.g., silent output), returns false.
	Confirm(prompt string) (bool, error)
}

// Progress represents a long-running operation with progress updates
type Progress interface {
	// Update changes the progress message
	Update(message string)
	// Success marks the progress as successful
	Success(message string)
	// Fail marks the progress as failed
	Fail(message string)
}

// StandardOutput implements Output for standard terminal output
type StandardOutput struct {
	writer       io.Writer
	isTTY        bool
	colorEnabled bool
	autoConfirm  bool
}

// OutputOptions configures StandardOutput behavior.
type OutputOptions struct {
	Writer      io.Writer
	AutoConfirm bool
}

// New creates a standard output handler writing to stdout
func New() Output {
	return NewWithWriter(os.Stdout)
}

// NewWithWriter creates a standard output handler with a custom writer
func NewWithWriter(w io.Writer) Output {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}

	return &StandardOutput{
		writer:       w,
		isTTY:        isTTY,
		colorEnabled: isTTY, // Enable colors only for TTY
	}
}

// NewWithOptions creates a standard output handler with the given options.
func NewWithOptions(opts OutputOptions) Output {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	return &StandardOutput{
		writer:       w,
		isTTY:        isTTY,
		colorEnabled: isTTY,
		autoConfirm:  opts.AutoConfirm,
	}
}

// NewSilent creates a silent output handler that discards all output
func NewSilent() Output {
	return NewWithWriter(io.Discard)
}

// Info displays an informational message
func (o *StandardOutput) Info(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(o.writer, format+"\n", args...)
}

// Success displays a success message
func (o *StandardOutput) Success(format string, args ...interface{}) {
	symbol := "✓"
	if o.colorEnabled {
		// Green checkmark
		_, _ = fmt.Fprintf(o.writer, "\033[32m%s\033[0m ", symbol)
	} else {
		_, _ = fmt.Fprintf(o.writer, "%s ", symbol)
	}
	_, _ = fmt.Fprintf(o.writer, format+"\n", args...)
}

// Warning displays a warning message
func (o *StandardOutput) Warning(format string, args ...interface{}) {
	symbol := "⚠"
	if o.colorEnabled {
		// Yellow warning
		_, _ = fmt.Fprintf(o.writer, "\033[33m%s\033[0m ", symbol)
	} else {
		_, _ = fmt.Fprintf(o.writer, "%s ", symbol)
	}
	_, _ = fmt.Fprintf(o.writer, format+"\n", args...)
}

// Error displays an error message
func (o *StandardOutput) Error(format string, args ...interface{}) {
	symbol := "✗"
	if o.colorEnabled {
		// Red X
		_, _ = fmt.Fprintf(o.writer, "\033[31m%s\033[0m ", symbol)
	} else {
		_, _ = fmt.Fprintf(o.writer, "%s ", symbol)
	}
	_, _ = fmt.Fprintf(o.writer, format+"\n", args...)
}

// StartProgress starts a progress indicator
func (o *StandardOutput) StartProgress(message string) Progress {
	return newProgress(o, message)
}

// Header displays a header banner
func (o *StandardOutput) Header(text string) {
	width := 60
	if !o.isTTY {
		width = len(text) + 4
	}

	border := strings.Repeat("═", width)
	padding := (width - len(text)) / 2

	_, _ = fmt.Fprintf(o.writer, "\n%s\n", border)
	_, _ = fmt.Fprintf(o.writer, "%s%s\n", strings.Repeat(" ", padding), text)
	_, _ = fmt.Fprintf(o.writer, "%s\n\n", border)
}

// Confirm displays a yes/no prompt and waits for user input.
// Returns true immediately when AutoConfirm is set (--yes flag or JSON mode).
func (o *StandardOutput) Confirm(prompt string) (bool, error) {
	if o.autoConfirm {
		return true, nil
	}
	if !o.isTTY {
		return false, nil
	}
	_, _ = fmt.Fprintf(o.writer, "%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("failed to read user input: %w", err)
	}
	input = strings.TrimSpace(strings.ToLower(input))
	return input == "y" || input == "yes", nil
}

// Section displays a section header
func (o *StandardOutput) Section(text string) {
	if o.colorEnabled {
		// Bold text
		_, _ = fmt.Fprintf(o.writer, "\n\033[1m%s\033[0m\n", text)
	} else {
		_, _ = fmt.Fprintf(o.writer, "\n%s\n", text)
	}

	// Underline with dashes
	_, _ = fmt.Fprintf(o.writer, "%s\n\n", strings.Repeat("─", len(text)))
}
