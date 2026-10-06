// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"
)

// Match NIC Configuration Operator's doSPCX installation limits and layout.
// Validate in memory without extracting user-supplied archive entries.
func validateDospcxArchive(archive []byte) error {
	const (
		maxCompressed   = 1 << 20
		maxExpanded     = 32 << 20
		maxDecompressed = 40 << 20
		maxEntries      = 4096
	)
	if len(archive) > maxCompressed {
		return fmt.Errorf("doSPCX archive exceeds the %d-byte compressed size limit", maxCompressed)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("invalid doSPCX gzip archive: %w", err)
	}
	defer gz.Close()
	decompressed := &io.LimitedReader{R: gz, N: maxDecompressed + 1}
	tr := tar.NewReader(decompressed)
	seen := map[string]bool{}
	types := map[string]byte{}
	var expanded int64
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid doSPCX tar archive: %w", err)
		}
		if count >= maxEntries {
			return fmt.Errorf("doSPCX archive exceeds the %d-entry limit", maxEntries)
		}
		// git archive includes a global PAX header with commit provenance.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || strings.ContainsAny(name, "\x00\\") || path.Clean(name) != name ||
			(name != "data" && !strings.HasPrefix(name, "data/")) {
			return fmt.Errorf("doSPCX archive contains invalid path %q; entries must be under data/", h.Name)
		}
		if seen[name] {
			return fmt.Errorf("doSPCX archive contains duplicate path %q", name)
		}
		seen[name] = true
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return fmt.Errorf("doSPCX archive path %q uses unsupported tar entry type %d", name, h.Typeflag)
		}
		if previous, exists := types[name]; exists && previous != h.Typeflag {
			return fmt.Errorf("doSPCX archive path %q conflicts with a directory", name)
		}
		types[name] = h.Typeflag
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if previous, exists := types[parent]; exists && previous != tar.TypeDir {
				return fmt.Errorf("doSPCX archive path %q has a file as its parent", name)
			}
			types[parent] = tar.TypeDir
		}
		if h.Typeflag == tar.TypeReg {
			if h.Size < 0 || h.Size > maxExpanded-expanded {
				return fmt.Errorf("doSPCX archive exceeds the %d-byte expanded size limit", maxExpanded)
			}
			expanded += h.Size
			if _, err := io.CopyN(io.Discard, tr, h.Size); err != nil {
				return fmt.Errorf("invalid doSPCX archive file %q: %w", name, err)
			}
		}
	}
	if _, err := io.Copy(io.Discard, decompressed); err != nil {
		return fmt.Errorf("invalid doSPCX gzip stream: %w", err)
	}
	if decompressed.N <= 0 {
		return fmt.Errorf("doSPCX archive exceeds the %d-byte decompressed size limit", maxDecompressed)
	}
	for _, directory := range []string{"data", "data/features", "data/profiles", "data/templates"} {
		if types[directory] != tar.TypeDir {
			return fmt.Errorf("doSPCX archive is missing required directory %q", directory)
		}
	}
	for _, file := range []string{"execution-groups.yaml", "formulas.yaml", "hca-types.yaml", "phase-config.yaml", "platform-types.yaml"} {
		if types["data/"+file] != tar.TypeReg {
			return fmt.Errorf("doSPCX archive is missing required file %q", file)
		}
	}
	return nil
}
