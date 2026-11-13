// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type dpfProgressBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *dpfProgressBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *dpfProgressBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func TestDPFCLIProgressWhileRemoteRequestIsPending(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%t", quiet), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"message":"private-response-canary"}`))
			}))
			defer server.Close()
			defer unblock()
			dir, pathErr := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, pathErr)
			seed := filepath.Join(dir, "site.yaml")
			source := fmt.Sprintf("credentials:\n  reader: {username: admin, passwordRef: {env: DPF_PROGRESS_TEST_PASSWORD}}\ndiscovery:\n  tls: {insecureSkipVerify: true}\n  hosts: [{name: worker-dpu, bmcEndpoint: '%s', credentialRef: reader}]\n  probes: {mode: inventory}\n", server.URL)
			require.NoError(t, os.WriteFile(seed, []byte(source), 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestTargetCLIHelperProcess$", "--", "discover", "--target", "dpf", "--user-config", seed, "--save-cluster-config", filepath.Join(dir, "inventory.yaml"), "--output", "json"}
			if quiet {
				args = append(args, "--quiet")
			}
			command := exec.CommandContext(ctx, os.Args[0], args...)
			command.Env = append(os.Environ(), "L8K_TARGET_CLI_HELPER=1", "DPF_PROGRESS_TEST_PASSWORD=password-canary")
			// Separate streams are required to verify the public JSON/progress contract.
			var stdout bytes.Buffer
			stderr := &dpfProgressBuffer{}
			command.Stdout, command.Stderr = &stdout, stderr
			done := make(chan error, 1)
			go func() { done <- command.Run() }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("CLI exited before request: %v; %s", err, stderr.String())
			case <-ctx.Done():
				t.Fatal("CLI never reached fixture")
			}
			if quiet {
				require.Empty(t, stderr.String())
			} else {
				require.Eventually(t, func() bool { return bytes.Contains([]byte(stderr.String()), []byte(`"worker-dpu"`)) }, time.Second, 10*time.Millisecond)
				require.Contains(t, stderr.String(), "[RUNNING]")
				require.Contains(t, stderr.String(), "Redfish connection/session")
				require.NotContains(t, stderr.String(), "[PASS] Publication")
			}
			unblock()
			err := <-done
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			require.Equal(t, 3, exit.ExitCode())
			var report map[string]any
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
			require.Equal(t, "dpf", report["target"])
			if quiet {
				require.Empty(t, stderr.String())
			} else {
				require.Contains(t, stderr.String(), "[FAIL]")
				require.Contains(t, stderr.String(), "HTTP_FAILED")
			}
			for _, secret := range []string{"password-canary", "private-response-canary"} {
				require.NotContains(t, stdout.String()+stderr.String(), secret)
			}
		})
	}
}
