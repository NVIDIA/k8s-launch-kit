//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestValidateSignalChild(t *testing.T) {
	if os.Getenv("L8K_DPF_VALIDATE_SIGNAL_CHILD") != "1" {
		return
	}
	request := ValidateRequest{ValidationRequest: app.ValidationRequest{Inventory: "inventory", DeploymentFiles: "bundle"}, Stdout: os.Stdout, Stderr: os.Stderr}
	operation, err := NewValidateAdapter(request, func(ctx context.Context, _ app.ValidationRequest) validation.Result {
		_, _ = fmt.Fprintln(os.Stderr, "VALIDATION_READY")
		<-ctx.Done()
		return validation.PreparationFailure(validation.ModeFull, validation.FatalCanceled, ctx.Err())
	}).Bind(target.Invocation{Target: target.DPF, Phase: target.Validate, Output: target.OutputOptions{Format: "json", Quiet: true}})
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := apperrors.IsExitStatus(operation.Run(context.Background())); !ok || code != 4 {
		t.Fatalf("cancellation exit = %d, %t", code, ok)
	}
	os.Exit(0)
}

func TestValidatePublishesPartialResultAfterOSInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestValidateSignalChild$")
	command.Env = append(os.Environ(), "L8K_DPF_VALIDATE_SIGNAL_CHILD=1")
	var stdout bytes.Buffer
	command.Stdout = &stdout
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil || line != "VALIDATION_READY\n" {
		_ = command.Wait()
		t.Fatalf("validation did not reach observation: %q, %v", line, err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	var result validation.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("partial JSON: %v", err)
	}
	if result.Fatal != validation.FatalCanceled || result.ExitCode() != 4 || len(result.Checks) != 1 {
		t.Fatalf("partial cancellation result = %#v", result)
	}
}
