// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import "golang.org/x/sys/unix"

func renameNewDirectory(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
