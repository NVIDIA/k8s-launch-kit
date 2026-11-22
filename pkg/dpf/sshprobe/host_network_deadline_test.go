// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
)

type hostNetworkSSHProtocol struct {
	snapshotDelay time.Duration
	actionDelay   time.Duration
	actionStarted chan struct{}
	configure     atomic.Int32
	inspect       atomic.Int32
	passwordReads atomic.Int32
	invalid       atomic.Bool
	response      string
	status        uint32
}

// Exercise the actual public adapter and SSH deadlines. Backend filesystem and
// netlink correctness remain covered by the isolated Ubuntu backend suite.
func (p *hostNetworkSSHProtocol) serve(ch ssh.Channel, command string) bool {
	if strings.Contains(command, `printf '%s' "$SSH_CONNECTION"`) {
		_, _ = ch.Write([]byte("192.0.2.10 40000 192.0.2.20 22"))
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return true
	}
	end := strings.LastIndex(command, " -- ")
	if end < 0 {
		p.invalid.Store(true)
		return false
	}
	parts := strings.Fields(command[end+4:])
	if len(parts) != 2 {
		p.invalid.Store(true)
		return false
	}
	data, err := base64.StdEncoding.DecodeString(parts[1])
	var payload hostNetworkBackendRequest
	if err != nil || json.Unmarshal(data, &payload) != nil {
		p.invalid.Store(true)
		return false
	}
	if strings.Contains(command, "/usr/bin/sudo -S -p '' ") {
		input, readErr := io.ReadAll(ch)
		if readErr != nil || string(input) != "test-password\n" {
			p.invalid.Store(true)
		}
		p.passwordReads.Add(1)
	} else if !strings.Contains(command, "/usr/bin/sudo -n ") {
		p.invalid.Store(true)
	}
	var output []byte
	status := uint32(0)
	if parts[0] == "snapshot" {
		time.Sleep(p.snapshotDelay)
		output, _ = json.Marshal(hostNetworkTestSnapshot())
	} else {
		switch parts[0] {
		case "configure":
			p.configure.Add(1)
		case "inspect":
			p.inspect.Add(1)
		default:
			p.invalid.Store(true)
		}
		if p.actionStarted != nil {
			close(p.actionStarted)
		}
		time.Sleep(p.actionDelay)
		output, _ = json.Marshal(HostNetworkReceipt{RequestID: payload.RequestID, State: "complete", RuntimeVerified: true, FileVerified: true, VerifiedRepeat: parts[0] == "inspect"})
		if p.response != "" {
			output = []byte(p.response)
		}
		status = p.status
	}
	_, _ = ch.Write(output)
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	return true
}

func hostNetworkSSHTarget(t *testing.T, protocol *hostNetworkSSHProtocol, password bool) config.Target {
	t.Helper()
	RegisterTestingT(t)
	server := startServer()
	t.Cleanup(server.close)
	server.mu.Lock()
	server.onCommand = protocol.serve
	server.mu.Unlock()
	var access *config.SSHAccess
	if password {
		var err error
		access, err = resolvedTestAccess(server, server.auth().knownHosts)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		dir := t.TempDir()
		block, err := ssh.MarshalPrivateKey(server.private, "host-network-test")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "key"), pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), server.auth().knownHosts, 0600); err != nil {
			t.Fatal(err)
		}
		document := "credentials:\n  reader: {username: reader, privateKeyRef: {file: key}}\ndiscovery:\n  hosts:\n    - credentialRef: reader\n      ssh: {address: " + server.listener.Addr().String() + ", knownHostsFile: known_hosts, sudo: true}\n  probes: {mode: inventory}\n"
		input, err := config.Decode([]byte(document), filepath.Join(dir, "site.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := config.Resolve(input)
		if err != nil {
			t.Fatal(err)
		}
		access = resolved.Targets[0].SSH
	}
	return config.Target{Role: "hosts", Endpoint: "ssh://" + server.listener.Addr().String(), SSH: access}
}

func TestHostNetworkPublicTransactionUsesExistingOperationDeadline(t *testing.T) {
	for _, password := range []bool{false, true} {
		for _, action := range []string{"configure", "inspect"} {
			name := "key/" + action
			if password {
				name = "password/" + action
			}
			t.Run(name, func(t *testing.T) {
				protocol := &hostNetworkSSHProtocol{snapshotDelay: 550 * time.Millisecond, actionDelay: 1100 * time.Millisecond}
				target := hostNetworkSSHTarget(t, protocol, password)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				started := time.Now()
				var receipt HostNetworkReceipt
				var err error
				if action == "configure" {
					receipt, err = ConfigureHostWorkloadNetwork(ctx, target, hostNetworkTestRequest(), time.Second)
				} else {
					receipt, err = InspectHostWorkloadNetwork(ctx, target, hostNetworkTestRequest(), time.Second)
				}
				if err != nil || receipt.State != "complete" || protocol.invalid.Load() || time.Since(started) < 1500*time.Millisecond {
					t.Fatalf("compound operation clipped to request timeout: receipt=%+v err=%v", receipt, err)
				}
				if action == "inspect" && (protocol.configure.Load() != 0 || protocol.inspect.Load() != 1 || !receipt.VerifiedRepeat) || action == "configure" && (protocol.configure.Load() != 1 || protocol.inspect.Load() != 0) {
					t.Fatal("transaction repeated or inspect selected configure")
				}
				if password && protocol.passwordReads.Load() != 2 {
					t.Fatal("password did not stay on the two sudo stdin streams")
				}
			})
		}
	}
}

func TestHostNetworkPublicSnapshotKeepsRequestDeadline(t *testing.T) {
	protocol := &hostNetworkSSHProtocol{snapshotDelay: 1200 * time.Millisecond}
	target := hostNetworkSSHTarget(t, protocol, true)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err := ConfigureHostWorkloadNetwork(ctx, target, hostNetworkTestRequest(), time.Second)
	if Code(err) != "SSH_TIMEOUT_OR_CANCELLED" || protocol.configure.Load() != 0 || ctx.Err() != nil {
		t.Fatalf("snapshot lost short bound or reached effects: %v", err)
	}
}

func TestHostNetworkPublicAcknowledgementKeepsUnknownFiniteCause(t *testing.T) {
	for _, mode := range []string{"deadline", "canceled", "transport", "invalid response", "wrong request"} {
		t.Run(mode, func(t *testing.T) {
			protocol := &hostNetworkSSHProtocol{actionStarted: make(chan struct{})}
			want := ""
			switch mode {
			case "deadline":
				protocol.actionDelay, want = 2500*time.Millisecond, "DEADLINE"
			case "canceled":
				protocol.actionDelay, want = 500*time.Millisecond, "CANCELED"
			case "transport":
				protocol.status, protocol.response, want = 7, "private-remote-error", "TRANSPORT_FAILED"
			case "invalid response":
				protocol.response, want = "private-invalid-json", "RESPONSE_INVALID"
			case "wrong request":
				protocol.response, want = `{"requestID":"wrong","state":"complete","runtimeVerified":true,"fileVerified":true}`, "REQUEST_ID_MISMATCH"
			}
			target := hostNetworkSSHTarget(t, protocol, true)
			limit := 3 * time.Second
			if mode == "deadline" {
				limit = 2 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			if mode == "canceled" {
				go func() {
					select {
					case <-protocol.actionStarted:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			receipt, err := InspectHostWorkloadNetwork(ctx, target, hostNetworkTestRequest(), time.Second)
			diagnostic := HostNetworkAcknowledgementDiagnostic(err)
			if err == nil || receipt.State != "unknown" || !receipt.Blocking || receipt.ReasonCode != "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN" || !strings.Contains(diagnostic, "HOST_NETWORK_ACK_CAUSE_"+want+":") || protocol.inspect.Load() != 1 || protocol.configure.Load() != 0 || protocol.invalid.Load() {
				t.Fatalf("lost unknown effect/cause: receipt=%+v cause=%q err=%v", receipt, diagnostic, err)
			}
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation/deadline identity was lost")
			}
			if strings.Contains(err.Error()+diagnostic, "private-") {
				t.Fatal("raw remote cause escaped")
			}
		})
	}
}

type hostNetworkDeadlineRemote struct {
	hostNetworkFakeRemote
	deadline time.Time
	budget   time.Duration
}

func (f *hostNetworkDeadlineRemote) RunWithInputTimeout(ctx context.Context, stage, command string, input []byte, timeout time.Duration) (string, error) {
	f.deadline, _ = ctx.Deadline()
	f.budget = timeout
	return f.hostNetworkFakeRemote.RunWithInputTimeout(ctx, stage, command, input, timeout)
}

// A context's deadline can pass before its timer publishes Err/Done. Dispatch
// must reject the actual expired deadline independently of timer notification.
type hostNetworkDelayedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c hostNetworkDelayedDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestHostNetworkBackendKeepsOriginalDeadlineAndRejectsExpiredDispatch(t *testing.T) {
	remote := &hostNetworkDeadlineRemote{hostNetworkFakeRemote: hostNetworkFakeRemote{snapshot: hostNetworkTestSnapshot()}}
	session := &hostNetworkSession{remote: remote}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if _, err := session.configure(ctx, hostNetworkTestRequest()); err != nil {
		t.Fatal(err)
	}
	if remote.deadline != deadline || remote.budget <= 0 || remote.budget > 3*time.Second {
		t.Fatal("compound backend received a renewed or detached deadline")
	}
	remote.calls = nil
	remote.budget = 0
	expired := hostNetworkDelayedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	_, err := session.run(expired, "inspect", hostNetworkBackendRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || remote.budget != 0 || len(remote.calls) != 0 {
		t.Fatal("expired deadline dispatched a backend command while Err was unpublished")
	}
}

func TestHostNetworkAcknowledgementDiagnosticsOwnFinitePrivateCauses(t *testing.T) {
	secret := errors.New("private-library-error")
	timeout := &Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: "host-network-configure"}
	err := hostNetworkAcknowledgementFailure(context.Background(), errors.Join(timeout, secret))
	if Code(err) != "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN" || !errors.Is(err, secret) || !strings.Contains(HostNetworkAcknowledgementDiagnostic(err), "HOST_NETWORK_ACK_CAUSE_COMMAND_TIMEOUT:") {
		t.Fatal("finite command timeout or private original cause was lost")
	}
	if strings.Contains(err.Error()+HostNetworkAcknowledgementDiagnostic(err), secret.Error()) {
		t.Fatal("private cause entered the safe projection")
	}
	for _, foreign := range []error{secret, &Error{Code: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", Stage: "private-library-error"}, &HostNetworkError{ReasonCode: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", Blocking: true}, &HostNetworkError{ReasonCode: "HOST_NETWORK_OWNERSHIP_CONFLICT", Blocking: true}} {
		if HostNetworkAcknowledgementDiagnostic(foreign) != "" {
			t.Fatal("unowned error acquired a specific acknowledgement cause")
		}
	}
}
