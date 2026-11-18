// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"unicode"
)

// DisplayText replaces control characters before writing untrusted text to a terminal.
func DisplayText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
