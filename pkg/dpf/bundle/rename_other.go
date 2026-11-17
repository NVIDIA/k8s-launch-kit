//go:build !darwin && !linux

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import "fmt"

func renameNewDirectory(_, _ string) error {
	return fmt.Errorf("DPF directory publication is unsupported on this platform")
}
