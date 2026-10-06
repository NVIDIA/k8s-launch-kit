// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDospcxArchiveRejectsInvalidLayoutAndEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []*tar.Header
		want    string
	}{
		{"empty", nil, "missing required directory"},
		{"missing files", []*tar.Header{{Name: "data/features/", Typeflag: tar.TypeDir}}, "missing required directory"},
		{"traversal", []*tar.Header{{Name: "data/../outside", Typeflag: tar.TypeReg}}, "invalid path"},
		{"outside root", []*tar.Header{{Name: "outside", Typeflag: tar.TypeReg}}, "invalid path"},
		{"duplicate", []*tar.Header{{Name: "data", Typeflag: tar.TypeDir}, {Name: "data/", Typeflag: tar.TypeDir}}, "duplicate path"},
		{"symlink", []*tar.Header{{Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}}, "unsupported tar entry"},
		{"file parent", []*tar.Header{{Name: "data", Typeflag: tar.TypeReg}, {Name: "data/file", Typeflag: tar.TypeReg}}, "file as its parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer
			gz := gzip.NewWriter(&buffer)
			tw := tar.NewWriter(gz)
			for _, header := range tc.entries {
				require.NoError(t, tw.WriteHeader(header))
			}
			require.NoError(t, tw.Close())
			require.NoError(t, gz.Close())
			require.ErrorContains(t, validateDospcxArchive(buffer.Bytes()), tc.want)
		})
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	_, err := gz.Write([]byte("plain text is not a tar archive"))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.ErrorContains(t, validateDospcxArchive(buffer.Bytes()), "invalid doSPCX tar archive")
	require.ErrorContains(t, validateDospcxArchive(make([]byte, (1<<20)+1)), "compressed size limit")
}

func TestDospcxArchiveChecksChecksumAndRequiredFiles(t *testing.T) {
	raw, err := os.ReadFile("testdata/dospcx-configmap.yaml")
	require.NoError(t, err)
	cm, _, err := parseSpectrumXProfileConfigMap(string(raw))
	require.NoError(t, err)
	archive, err := base64.StdEncoding.DecodeString(cm.BinaryData[DospcxArchiveKey])
	require.NoError(t, err)
	require.NoError(t, validateDospcxArchive(archive))
	archive[len(archive)-8] ^= 1
	require.Error(t, validateDospcxArchive(archive))

	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"data", "data/features", "data/profiles", "data/templates"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir}))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.ErrorContains(t, validateDospcxArchive(buffer.Bytes()), "missing required file")
}

func TestDospcxRendererRejectsLegacyAndNilProfiles(t *testing.T) {
	_, err := RenderDospcxConfigMap(nil, "operator")
	require.Error(t, err)
	_, err = RenderDospcxConfigMap(&ProfileSpectrumX{SPCXVersion: "RA2.3", Profile: "useSoftwareCCAlgorithm: true"}, "operator")
	require.Error(t, err)
}
