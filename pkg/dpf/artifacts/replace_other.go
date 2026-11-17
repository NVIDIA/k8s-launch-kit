//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package artifacts

import "os"

func replaceFile(source, dest string) error { return os.Rename(source, dest) }
