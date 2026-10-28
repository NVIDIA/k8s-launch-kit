// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/stretchr/testify/require"
)

func TestUnverifiedExternalNetworkingReports(t *testing.T) {
	rows := []networkoperatorplugin.ValidationResult{{Kind: "ExternalSecondaryNetwork", Name: "multus", Skipped: true, Reason: "unverified: no recognized API installer; host installation may exist"}}
	verdict := aggregateVerdict(nil, rows)
	require.True(t, verdict.OK)
	require.Zero(t, verdict.SuccessCount)
	require.Equal(t, 1, verdict.SkippedCount)
	require.False(t, verdict.HasMissing)
	require.NotEmpty(t, collectVerdictWarnings(verdict))
	require.Equal(t, "UNVERIFIED", validationStatusLabel(rows[0]))
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			require.NoError(t, err)
			previous := os.Stdout
			os.Stdout = writer
			emitValidationReport(nil, rows, nil, format)
			os.Stdout = previous
			require.NoError(t, writer.Close())
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			if format == "json" {
				var out map[string]any
				require.NoError(t, json.Unmarshal(data, &out))
				require.Contains(t, string(data), `"Skipped":true`)
				require.EqualValues(t, 0, out["summary"].(map[string]any)["successManifests"])
			} else {
				require.Contains(t, string(data), "UNVERIFIED")
				require.Contains(t, string(data), "1 unverified")
			}
		})
	}
	var html bytes.Buffer
	require.NoError(t, connectivity.RenderHTML(&html, connectivity.ReportData{Manifests: rows}))
	require.Contains(t, html.String(), ">UNVERIFIED</span>")
	require.Contains(t, html.String(), "host installation may exist")
	doc := buildJUnit(junitInput{Data: connectivity.ReportData{Manifests: rows}})
	require.NotEmpty(t, doc.Suites)
	require.Equal(t, 1, doc.Suites[0].Skipped)
	require.Zero(t, doc.Suites[0].Failures)
}
