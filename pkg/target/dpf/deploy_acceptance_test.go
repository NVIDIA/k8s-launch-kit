// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

func TestDeploymentAcceptanceOutputRetainsActionableFullChecks(t *testing.T) {
	result := deployment.RunResult{State: deployment.RunStateFailed, ReportPath: "/reports/deployment.jsonl", LastCompletedStage: "host-workload-network", Validation: &validation.Result{
		Mode: validation.ModeFull, Verdict: validation.VerdictFailed, Report: validation.ReportDisposition{Status: validation.ReportPublished, Path: "/reports/validation.html"}, Cause: errors.New("private-acceptance-cause"),
		Checks: []validation.CheckResult{{CheckSpec: validation.CheckSpec{ID: "tenant/kube-proxy/configuration", Subject: "Tenant kube-proxy configuration", Required: true}, Outcome: validation.Outcome{State: validation.Failed, Reason: validation.ReasonHealth, Summary: "The clusterCIDR is empty.", Action: "Restore the retained tenant pod CIDR configuration."}}},
	}}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := writeDeploymentResult(DeployRequest{Stdout: &stdout, Stderr: &stderr}, format, result, errors.New("private-run-cause"))
			output := stdout.String()
			if err == nil || !strings.Contains(output, "clusterCIDR") || !strings.Contains(output, "Restore the retained") || !strings.Contains(output, "/reports/validation.html") || strings.Contains(output, "private-acceptance-cause") || strings.Contains(output, "private-run-cause") {
				t.Fatalf("incomplete or unsafe output: %s err=%v", output, err)
			}
			if format == "json" {
				decoder := json.NewDecoder(&stdout)
				var document map[string]json.RawMessage
				if err := decoder.Decode(&document); err != nil || document["validation"] == nil {
					t.Fatal("missing canonical validation document")
				}
				if decoder.Decode(&document) != io.EOF {
					t.Fatal("deployment emitted more than one JSON document")
				}
			}
		})
	}
}
