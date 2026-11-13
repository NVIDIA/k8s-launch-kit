// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// writeJSONReport prepares the complete JSON document before writing. Invalid
// inventory must not suppress the failure report or leave partial JSON stdout.
// The caller still receives the encoding error and returns a failing exit code.
func writeJSONReport(w io.Writer, report inventory.Report) error {
	data, encodingErr := json.Marshal(report)
	if encodingErr != nil {
		failure := discovery.NewReport()
		failure.Execution = "failed"
		failure.Discovery = report.Discovery
		failure.Readiness = report.Readiness
		failure.Artifacts = report.Artifacts
		failure.Errors = append(slices.Clone(report.Errors), inventory.Problem{
			Code: "RESULT_ENCODING_FAILED", Message: encodingErr.Error(), ExitCode: 1,
		})
		var err error
		data, err = json.Marshal(failure)
		if err != nil {
			return fmt.Errorf("encode failure report: %w", err)
		}
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write discovery report: %w", err)
	}
	return encodingErr
}
