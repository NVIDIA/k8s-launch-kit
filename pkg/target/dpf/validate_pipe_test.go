//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestValidateClosedPipeChild(t *testing.T) {
	mode := os.Getenv("L8K_DPF_VALIDATE_PIPE_CHILD")
	if mode == "" {
		return
	}
	runner := NewValidateRunner()
	if mode != "input" {
		runner = func(context.Context, app.ValidationRequest) validation.Result {
			result := validation.Result{Mode: validation.ModeFull, Verdict: validation.VerdictPassed, Report: validation.ReportDisposition{Status: validation.ReportDisabled}, Cause: errors.New("PRIVATE-PIPE-CAUSE")}
			if mode == "validation" {
				result.Verdict = validation.VerdictFailed
			}
			return result
		}
	}
	request := ValidateRequest{ValidationRequest: app.ValidationRequest{Inventory: "/missing/inventory.yaml", DeploymentFiles: "/missing/bundle", ReportPath: "-"}, Stdout: os.Stdout, Stderr: os.Stderr}
	if mode == "restored" {
		request.Stdout = io.Discard
	}
	operation, err := NewValidateAdapter(request, runner).Bind(target.Invocation{Target: target.DPF, Phase: target.Validate, Output: target.OutputOptions{Format: os.Getenv("L8K_DPF_VALIDATE_PIPE_FORMAT"), Quiet: true}})
	if err != nil {
		t.Fatal(err)
	}
	code, ok := apperrors.IsExitStatus(operation.Run(context.Background()))
	if !ok {
		t.Fatal("validation did not return its terminal exit status")
	}
	if mode == "restored" {
		// The operation must not leave process-wide SIGPIPE suppression behind.
		_, _ = os.Stdout.Write([]byte("after validation\n"))
	}
	os.Exit(code)
}

func TestValidateClosedStdoutPreservesExitStatus(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, test := range []struct {
			name string
			code int
		}{{"input", 2}, {"validation", 4}, {"success", 1}} {
			t.Run(format+"/"+test.name, func(t *testing.T) {
				status, stderr := runValidationWithClosedPipe(t, test.name, format)
				if !status.Exited() || status.ExitStatus() != test.code {
					t.Fatalf("exit = %v, want %d; stderr: %s", status, test.code, stderr)
				}
				if !strings.Contains(stderr, "Cannot write complete DPF validation result") || strings.Contains(stderr, "PRIVATE") || strings.Contains(stderr, "broken pipe") {
					t.Fatalf("unexpected output failure diagnostic: %s", stderr)
				}
			})
		}
	}
}

func TestValidateRestoresBrokenPipeHandling(t *testing.T) {
	status, stderr := runValidationWithClosedPipe(t, "restored", "json")
	if !status.Signaled() || status.Signal() != syscall.SIGPIPE {
		t.Fatalf("post-validation write status = %v, want SIGPIPE; stderr: %s", status, stderr)
	}
}

func runValidationWithClosedPipe(t *testing.T, mode, format string) (syscall.WaitStatus, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestValidateClosedPipeChild$")
	command.Env = append(os.Environ(), "L8K_DPF_VALIDATE_PIPE_CHILD="+mode, "L8K_DPF_VALIDATE_PIPE_FORMAT="+format)
	command.Stdout = writer
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err = command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run validation child: %v; stderr: %s", err, stderr.String())
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("missing process wait status: %v", exitError)
	}
	return status, stderr.String()
}
