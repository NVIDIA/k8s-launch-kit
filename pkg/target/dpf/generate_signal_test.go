//go:build !windows

// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

const (
	generationSignalChildEnv  = "L8K_DPF_GENERATE_SIGNAL_CHILD"
	generationSignalSiteEnv   = "L8K_DPF_GENERATE_SIGNAL_SITE"
	generationSignalOutputEnv = "L8K_DPF_GENERATE_SIGNAL_OUTPUT"
	generationSignalMarkerEnv = "L8K_DPF_GENERATE_SIGNAL_MARKER"
)

type generationSignalChildResult struct {
	output []byte
	err    error
}

// TestGenerateSignalChild is invoked only by TestGenerateStopsOnOSInterrupt in
// a subprocess. Its injected chart fetch blocks until NotifyContext observes
// the child's OS interrupt, without performing network or filesystem reads.
func TestGenerateSignalChild(t *testing.T) {
	if os.Getenv(generationSignalChildEnv) != "1" {
		return
	}
	site := os.Getenv(generationSignalSiteEnv)
	output := os.Getenv(generationSignalOutputEnv)
	marker := os.Getenv(generationSignalMarkerEnv)
	if site == "" || output == "" || marker == "" {
		t.Fatal("child test requires site, output, and marker paths")
	}
	fetchCount := 0
	driver := generateDriver{
		request: GenerateRequest{UserConfig: site, OutputPath: output, Stdout: &strings.Builder{}, Stderr: &strings.Builder{}},
		fetchChart: func(ctx context.Context, _ string) ([]byte, error) {
			fetchCount++
			markerTemp := marker + ".tmp"
			if err := os.WriteFile(markerTemp, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				return nil, fmt.Errorf("write child start marker: %w", err)
			}
			if err := os.Rename(markerTemp, marker); err != nil {
				return nil, fmt.Errorf("publish child start marker: %w", err)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	operation, err := driver.Bind(target.Invocation{Target: target.DPF, Phase: target.Generate})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	err = operation.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Run() error = %v, want cancellation from OS signal", err)
	}
	if fetchCount != 1 {
		t.Fatalf("chart fetch count = %d, want exactly one interrupted fetch", fetchCount)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("generation output exists after cancellation: stat error = %v", err)
	}
}

func TestGenerateStopsOnOSInterrupt(t *testing.T) {
	gomega.RegisterTestingT(t)
	root := t.TempDir()
	input := generationSite()
	siteBytes, err := yaml.Marshal(input.Site)
	if err != nil {
		t.Fatal(err)
	}
	sitePath := filepath.Join(root, "site.yaml")
	if err := os.WriteFile(sitePath, siteBytes, 0600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "bundle")
	markerPath := filepath.Join(root, "fetch-started")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGenerateSignalChild$")
	command.Env = append(os.Environ(),
		generationSignalChildEnv+"=1",
		generationSignalSiteEnv+"="+sitePath,
		generationSignalOutputEnv+"="+outputPath,
		generationSignalMarkerEnv+"="+markerPath,
	)
	wait := make(chan generationSignalChildResult, 1)
	go func() {
		output, err := command.CombinedOutput()
		wait <- generationSignalChildResult{output: output, err: err}
	}()

	deadline := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	var childPID int
	finished := false
	defer func() {
		if !finished {
			cancel()
			killGenerationChild(childPID)
			_ = waitForGenerationChild(wait)
		}
	}()
	for {
		if marker, err := os.ReadFile(markerPath); err == nil {
			pid, parseErr := strconv.Atoi(string(marker))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("child marker PID %q is invalid", marker)
			}
			childPID = pid
			process, err := os.FindProcess(pid)
			if err != nil {
				t.Fatalf("find generation child process: %v", err)
			}
			if err := process.Signal(os.Interrupt); err != nil {
				t.Fatalf("signal generation child: %v", err)
			}
			break
		}
		select {
		case result := <-wait:
			finished = true
			t.Fatalf("child exited before chart fetch started: %v\n%s", result.err, result.output)
		case <-ctx.Done():
			cancel()
			killGenerationChild(childPID)
			result := waitForGenerationChild(wait)
			finished = true
			t.Fatalf("timed out waiting for chart fetch marker; child result: %v\n%s", result.err, result.output)
		case <-deadline.C:
		}
	}

	select {
	case result := <-wait:
		finished = true
		if result.err != nil {
			t.Fatalf("signal child exit = %v\n%s", result.err, result.output)
		}
	case <-ctx.Done():
		cancel()
		killGenerationChild(childPID)
		result := waitForGenerationChild(wait)
		finished = true
		t.Fatalf("timed out waiting for cancellation-aware child exit: %v\n%s", result.err, result.output)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("generation output exists after signal cancellation: stat error = %v", err)
	}
}

func killGenerationChild(pid int) {
	if pid <= 0 {
		return
	}
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func waitForGenerationChild(wait <-chan generationSignalChildResult) generationSignalChildResult {
	select {
	case result := <-wait:
		return result
	case <-time.After(2 * time.Second):
		return generationSignalChildResult{err: fmt.Errorf("child process did not exit after cancellation/kill")}
	}
}
