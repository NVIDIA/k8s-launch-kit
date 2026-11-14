//go:build windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package artifacts

import "golang.org/x/sys/windows"

func replaceFile(source, dest string) error {
	s, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(dest)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(s, d, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
