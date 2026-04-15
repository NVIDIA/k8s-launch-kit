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
	"fmt"
	"sync"
	"time"
)

// spinnerChars are the characters used for the spinner animation
var spinnerChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// standardProgress implements Progress for terminal progress indicators
type standardProgress struct {
	output    *StandardOutput
	message   string
	done      chan bool
	mu        sync.Mutex
	spinIndex int
	startTime time.Time
	stopped   bool
}

// newProgress creates a new progress indicator
func newProgress(output *StandardOutput, message string) Progress {
	p := &standardProgress{
		output:    output,
		message:   message,
		done:      make(chan bool),
		startTime: time.Now(),
		stopped:   false,
	}

	// Start the spinner in a goroutine
	go p.spin()

	return p
}

// spin runs the spinner animation
func (p *standardProgress) spin() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			if p.stopped {
				p.mu.Unlock()
				return
			}

			elapsed := time.Since(p.startTime)
			var timeStr string
			if elapsed > 30*time.Second {
				// Show elapsed time for long operations
				timeStr = fmt.Sprintf(" (%s)", formatDuration(elapsed))
			}

			if p.output.isTTY {
				// Use carriage return and clear line to update same line
				_, _ = fmt.Fprintf(p.output.writer, "\r\033[K%s %s%s", spinnerChars[p.spinIndex], p.message, timeStr)
				p.spinIndex = (p.spinIndex + 1) % len(spinnerChars)
			}
			p.mu.Unlock()
		}
	}
}

// Update changes the progress message
func (p *standardProgress) Update(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return
	}

	p.message = message

	// For non-TTY, print the update as a new line
	if !p.output.isTTY {
		_, _ = fmt.Fprintf(p.output.writer, "  %s\n", message)
	}
}

// Success marks the progress as successful
func (p *standardProgress) Success(message string) {
	p.stop()

	symbol := "✓"
	if p.output.colorEnabled {
		_, _ = fmt.Fprintf(p.output.writer, "\r\033[K\033[32m%s\033[0m %s\n", symbol, message)
	} else {
		if p.output.isTTY {
			_, _ = fmt.Fprintf(p.output.writer, "\r\033[K%s %s\n", symbol, message)
		} else {
			_, _ = fmt.Fprintf(p.output.writer, "%s %s\n", symbol, message)
		}
	}
}

// Fail marks the progress as failed
func (p *standardProgress) Fail(message string) {
	p.stop()

	symbol := "✗"
	if p.output.colorEnabled {
		_, _ = fmt.Fprintf(p.output.writer, "\r\033[K\033[31m%s\033[0m %s\n", symbol, message)
	} else {
		if p.output.isTTY {
			_, _ = fmt.Fprintf(p.output.writer, "\r\033[K%s %s\n", symbol, message)
		} else {
			_, _ = fmt.Fprintf(p.output.writer, "%s %s\n", symbol, message)
		}
	}
}

// stop stops the progress indicator
func (p *standardProgress) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return
	}

	p.stopped = true
	close(p.done)

	// Clear the spinner line in TTY mode
	if p.output.isTTY {
		// Move to beginning of line and clear
		_, _ = fmt.Fprintf(p.output.writer, "\r\033[K")
	}
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	s := d / time.Second
	m := s / 60
	s = s % 60

	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
