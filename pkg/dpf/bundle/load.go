// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load validates closed on-disk membership before retaining any desired snapshot.
// It accepts only bounded regular files and rejects symlinks at every path level.
func Load(dir string) (*Bundle, error) {
	if err := noSymlinkPath(dir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open DPF bundle: %w", err)
	}
	defer root.Close()
	var files []File
	var indexBytes []byte
	total := 0
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > maxFiles*4 {
			return fmt.Errorf("DPF bundle exceeds directory entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("DPF bundle path %q is a symlink", name)
		}
		if entry.IsDir() {
			return nil
		}
		if err := validPath(name); err != nil {
			return err
		}
		if len(files) >= maxFiles+1 {
			return fmt.Errorf("DPF bundle exceeds file count limit")
		}
		data, err := readRegular(root, name)
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxBundleBytes {
			return fmt.Errorf("DPF bundle exceeds content limit")
		}
		if name == IndexPath {
			indexBytes = data
		} else {
			files = append(files, File{Path: name, Data: data})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load DPF bundle: %w", err)
	}
	if len(indexBytes) == 0 {
		return nil, fmt.Errorf("DPF bundle index is missing")
	}
	var index Index
	if err := decodeOne(indexBytes, &index); err != nil {
		return nil, fmt.Errorf("decode DPF bundle index: %w", err)
	}
	b, err := construct(index, files, true)
	if err != nil {
		return nil, err
	}
	b.indexBytes = bytes.Clone(indexBytes)
	return b, nil
}

func noSymlinkPath(filename string) error {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect DPF path %q: %w", current, err)
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("DPF path %q is a symlink", current)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func checkRootPath(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for n := range parts {
		info, err := root.Lstat(strings.Join(parts[:n+1], "/"))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("DPF bundle path %q contains a symlink", name)
		}
		if n < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("DPF bundle parent for %q is not a directory", name)
		}
	}
	return nil
}

func readRegular(root *os.Root, name string) ([]byte, error) {
	if err := checkRootPath(root, name); err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxFileBytes {
		return nil, fmt.Errorf("DPF bundle file %q is not a bounded regular file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("DPF bundle file %q changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("DPF bundle file %q exceeds size limit", name)
	}
	if err := checkRootPath(root, name); err != nil {
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("DPF bundle file %q changed while reading", name)
	}
	return data, nil
}

func (b *Bundle) persistedFiles() []File {
	files := b.Files()
	files = append(files, File{Path: IndexPath, Data: bytes.Clone(b.indexBytes)})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func sameBytes(a, b *Bundle) bool {
	left, right := a.persistedFiles(), b.persistedFiles()
	if len(left) != len(right) {
		return false
	}
	for n := range left {
		if left[n].Path != right[n].Path || !bytes.Equal(left[n].Data, right[n].Data) {
			return false
		}
	}
	return true
}
