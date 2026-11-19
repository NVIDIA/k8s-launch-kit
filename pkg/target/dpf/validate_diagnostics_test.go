// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

func TestValidationDiagnosticUsesCanonicalJSONHTMLAndText(t *testing.T) {
	result := adapterValidationResult()
	result.Fatal = validation.FatalObservation
	result.Diagnostic = &validation.Diagnostic{Stage: validation.StageRuntimeHBN, Reason: validation.ReasonProgressing, Summary: "Selected HBN application is pending.", Action: "Inspect the selected controller."}
	result.Cause = errors.New("PRIVATE-CAUSE")
	var encoded, html bytes.Buffer
	if err := writeValidationResult(&encoded, "json", result); err != nil {
		t.Fatal(err)
	}
	if err := validation.RenderHTML(&html, result, validation.ReportMetadata{}); err != nil {
		t.Fatal(err)
	}
	var decoded validation.Result
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Diagnostic == nil || *decoded.Diagnostic != *result.Diagnostic {
		t.Fatal("JSON diagnostic differs from canonical result")
	}
	if !strings.Contains(encoded.String(), "runtime-hbn-binding") {
		t.Fatal("JSON stage is not stable")
	}
	for _, text := range []string{html.String(), string(validationText(result))} {
		for _, want := range []string{"HBN workload binding", "Selected HBN application is pending.", "Inspect the selected controller."} {
			if !strings.Contains(text, want) {
				t.Fatalf("output lacks %q", want)
			}
		}
		if strings.Contains(text, "PRIVATE-CAUSE") {
			t.Fatal("output exposed private error cause")
		}
	}
}
