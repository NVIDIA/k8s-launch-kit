// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"k8s.io/apimachinery/pkg/types"
)

const (
	rebootHostUUID = "12345678-1234-1234-1234-123456789abc"
	rebootOldID    = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	rebootNewID    = "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff"
	rebootCardSN   = "CARD000123"
)

type fakeExternalSession struct {
	identity    sshprobe.HostIdentity
	observeErr  error
	onObserve   func(context.Context)
	rebootErr   error
	onReboot    func()
	observed    int
	rebootCount int
	closed      bool
}

func (s *fakeExternalSession) ObserveHost(ctx context.Context, _ string) (sshprobe.HostIdentity, error) {
	s.observed++
	if s.onObserve != nil {
		s.onObserve(ctx)
	}
	return s.identity, s.observeErr
}
func (s *fakeExternalSession) RebootSystemLevel(context.Context) error {
	s.rebootCount++
	if s.onReboot != nil {
		s.onReboot()
	}
	return s.rebootErr
}
func (s *fakeExternalSession) Close() error { s.closed = true; return nil }

func rebootConfig(t *testing.T) config.Request {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("host-key snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("L8K_REBOOT_TEST_PASSWORD", "not-recorded")
	passwordEnv := "L8K_REBOOT_TEST_PASSWORD"
	sudo := true
	input := config.Input{Path: filepath.Join(dir, "site.yaml")}
	input.Site.Credentials = map[string]config.Credential{"host": {Username: "operator", PasswordRef: config.PasswordRef{Env: &passwordEnv}}}
	input.Site.Discovery.Probes.Mode = stringPtr("inventory")
	input.Site.Discovery.Hosts = []config.Endpoint{{CredentialRef: "host", SSH: &config.SSH{Address: "host.example:22", KnownHostsFile: "known_hosts", Sudo: &sudo}}}
	input.Site.Inventory = &inventory.Inventory{
		FormatVersion: inventory.FormatVersion,
		Hosts: []inventory.Device{{
			ID: "host-identity", InputRef: "/discovery/hosts/0", SSHEndpoint: "ssh://host.example:22", SystemUUID: rebootHostUUID, ObservationState: "observed",
			Adapters: []inventory.Adapter{{Key: "adapter-key", PCIAddress: "0000:21:00.0", SerialNumber: rebootCardSN, IdentityState: "known"}},
		}},
		DPUs:         []inventory.Device{{ID: "dpu-identity", SerialNumber: rebootCardSN, ObservationState: "observed"}},
		Associations: []inventory.Association{{DPUID: "dpu-identity", HostID: "host-identity", HostAdapterKey: "adapter-key", SerialNumber: rebootCardSN}},
	}
	request, err := config.Resolve(input)
	if err != nil {
		t.Fatalf("resolve reboot test config: %v", err)
	}
	return request
}

func stringPtr(value string) *string { return &value }

func activationRequest(method string) deployment.ActivationRequest {
	return deployment.ActivationRequest{
		HostInputRef: "/discovery/hosts/0", HostEndpoint: "ssh://host.example:22", HostSystemUUID: rebootHostUUID,
		HostAdapterKey: "adapter-key", HostAdapterSerial: rebootCardSN, ResetSafetyExpiresAt: time.Now().Add(time.Minute),
		DPUNodes: []deployment.ActivationNode{{NodeName: "dpu-node-card000123", NodeUID: types.UID("node-uid"), RebootMethod: method,
			SelectedDPUs: []deployment.ActivationDPU{{SerialNumber: rebootCardSN, DPUName: "card000123", DPUUID: types.UID("dpu-device-uid")}},
		}},
	}
}

func newTestRebooter(t *testing.T, request config.Request, wait, poll time.Duration, open externalSessionOpener) *externalRebooter {
	t.Helper()
	return &externalRebooter{request: request, commandTimeout: 100 * time.Millisecond, rebootWait: wait, pollInterval: poll, openSession: open}
}

func TestExternalRebooterBindsIdentityAndRecoversWithoutReplaying(t *testing.T) {
	request := rebootConfig(t)
	var opens int
	var action *fakeExternalSession
	rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
		switch opens {
		case 1:
			action = &fakeExternalSession{identity: identity, rebootErr: errors.New("ssh channel closed after command")}
			return action, nil
		case 2:
			identity.BootID = rebootNewID
		}
		return &fakeExternalSession{identity: identity}, nil
	})
	got, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
	if err != nil {
		t.Fatalf("RebootExternal() error = %v", err)
	}
	if opens != 2 || action == nil || action.observed != 1 || action.rebootCount != 1 || !action.closed {
		t.Fatalf("opens=%d action=%#v; want one identity-bound action session, one post-read and no replay", opens, action)
	}
	if got.HostSystemUUID != rebootHostUUID || got.HostAdapterSerial != rebootCardSN || got.PreviousBootID != rebootOldID || got.CurrentBootID != rebootNewID || !got.Reconnected || got.KubernetesNodeReady != nil {
		t.Fatalf("unexpected verified reboot observation: %#v", got)
	}
}

func TestExternalRebooterRejectsUnsupportedMethodAndMismatchedTargetBeforeEffects(t *testing.T) {
	request := rebootConfig(t)
	cases := []struct {
		name   string
		mutate func(*deployment.ActivationRequest)
		method string
	}{
		{name: "power cycle cannot be approximated by host reboot", method: "PowerCycle"},
		{name: "source reference must select configured endpoint", method: "SystemLevelReset", mutate: func(r *deployment.ActivationRequest) { r.HostInputRef = "/discovery/hosts/1" }},
		{name: "endpoint must remain exact", method: "SystemLevelReset", mutate: func(r *deployment.ActivationRequest) { r.HostEndpoint = "ssh://other.example:22" }},
		{name: "adapter key must remain exact", method: "SystemLevelReset", mutate: func(r *deployment.ActivationRequest) { r.HostAdapterKey = "other-adapter" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rebooter := newTestRebooter(t, request, 100*time.Millisecond, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
				calls++
				return &fakeExternalSession{}, nil
			})
			activation := activationRequest(tc.method)
			if tc.mutate != nil {
				tc.mutate(&activation)
			}
			if _, err := rebooter.RebootExternal(context.Background(), activation); err == nil {
				t.Fatal("RebootExternal() succeeded for invalid method or binding")
			}
			if calls != 0 {
				t.Fatalf("opened %d SSH sessions before rejecting the request", calls)
			}
		})
	}
}

func TestExternalRebooterRejectsChangedIdentityAndNeverRepeatsUncertainAction(t *testing.T) {
	request := rebootConfig(t)
	var opens int
	var action *fakeExternalSession
	rebooter := newTestRebooter(t, request, 30*time.Millisecond, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
		if opens == 1 {
			identity.SystemUUID = "12345678-1234-1234-1234-123456789abd"
			action = &fakeExternalSession{identity: identity, rebootErr: errors.New("ambiguous transport failure")}
			return action, nil
		}
		return &fakeExternalSession{identity: identity}, nil
	})
	if _, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset")); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("mismatched pre-reboot host identity error = %v", err)
	}
	if opens != 1 || action == nil || action.rebootCount != 0 {
		t.Fatalf("pre-reboot identity mismatch had effects: opens=%d action=%#v", opens, action)
	}

	opens = 0
	var actionCalls int
	rebooter.openSession = func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
		if opens == 1 {
			return &fakeExternalSession{identity: identity, rebootErr: errors.New("ambiguous transport failure"), onReboot: func() { actionCalls++ }}, nil
		}
		return &fakeExternalSession{identity: identity}, nil
	}
	if _, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset")); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("unchanged boot ID should time out: %v", err)
	}
	if actionCalls != 1 {
		t.Fatalf("uncertain reboot action replayed %d times", actionCalls)
	}
}

func TestExternalRebooterRejectsIdentityChangeAfterReboot(t *testing.T) {
	request := rebootConfig(t)
	var opens, actionCalls int
	rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
		if opens == 1 {
			return &fakeExternalSession{identity: identity, rebootErr: errors.New("connection closed after reboot"), onReboot: func() { actionCalls++ }}, nil
		}
		if opens == 2 {
			identity.SystemUUID = "12345678-1234-1234-1234-123456789abd"
			identity.BootID = rebootNewID
		}
		return &fakeExternalSession{identity: identity}, nil
	})
	if _, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset")); err == nil || !strings.Contains(err.Error(), "different host") {
		t.Fatalf("changed post-reboot host identity error = %v", err)
	}
	if actionCalls != 1 || opens != 2 {
		t.Fatalf("opens=%d actionCalls=%d; want exactly one action and one post-reboot identity read", opens, actionCalls)
	}
}

func TestExternalRebooterHonorsCancellationDuringReboot(t *testing.T) {
	request := rebootConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opens int
	var actionCalls int
	rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
		return &fakeExternalSession{identity: identity, onReboot: func() { actionCalls++; cancel() }}, nil
	})
	if _, err := rebooter.RebootExternal(ctx, activationRequest("SystemLevelReset")); !errors.Is(err, context.Canceled) {
		t.Fatalf("RebootExternal() cancellation error = %v", err)
	}
	if opens != 1 || actionCalls != 1 {
		t.Fatalf("canceled operation opened=%d actionCalls=%d; want one identity-bound action only", opens, actionCalls)
	}
}

func TestExternalRebooterRequiresConfiguredAdapterAndDPUAssociations(t *testing.T) {
	request := rebootConfig(t)
	activation := activationRequest("SystemLevelReset")
	activation.DPUNodes[0].SelectedDPUs[0].SerialNumber = "OTHER"
	rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		t.Fatal("invalid DPU association must be rejected before SSH")
		return nil, nil
	})
	if _, err := rebooter.RebootExternal(context.Background(), activation); err == nil {
		t.Fatal("unassociated DPU was accepted")
	}
}

func TestExternalRebooterExpiresProofDuringSSHPreflightWithoutAction(t *testing.T) {
	for _, phase := range []string{"dial", "identity"} {
		t.Run(phase, func(t *testing.T) {
			request := rebootConfig(t)
			action := &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}}
			activation := activationRequest("SystemLevelReset")
			activation.ResetSafetyExpiresAt = time.Now().Add(50 * time.Millisecond)
			rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(ctx context.Context, _ string, _ *config.SSHAccess, _ time.Duration) (sshprobe.ExternalRebootSession, error) {
				if phase == "dial" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				action.onObserve = func(ctx context.Context) { <-ctx.Done() }
				return action, nil
			})
			_, err := rebooter.RebootExternal(context.Background(), activation)
			if !errors.Is(err, deployment.ErrActivationResetSafetyExpired) || action.rebootCount != 0 {
				t.Fatalf("err=%v submitted=%d", err, action.rebootCount)
			}
			if phase == "identity" && !action.closed {
				t.Fatal("expired preflight session left open")
			}
		})
	}
}

func TestExternalRebooterDispatchedTimeoutRemainsUnknownAndDoesNotReplay(t *testing.T) {
	request := rebootConfig(t)
	action := &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}, rebootErr: &sshprobe.Error{Code: "SSH_TIMEOUT_OR_CANCELLED", Stage: "external-reboot-system-level"}}
	activation := activationRequest("SystemLevelReset")
	activation.ResetSafetyExpiresAt = time.Now().Add(30 * time.Millisecond)
	action.onReboot = func() { <-time.After(time.Until(activation.ResetSafetyExpiresAt) + time.Millisecond) }
	opens := 0
	rebooter := newTestRebooter(t, request, 150*time.Millisecond, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		if opens == 1 {
			return action, nil
		}
		return &fakeExternalSession{identity: action.identity}, nil
	})
	_, err := rebooter.RebootExternal(context.Background(), activation)
	if err == nil || errors.Is(err, deployment.ErrActivationResetSafetyExpired) || !strings.Contains(err.Error(), "not confirmed") || action.rebootCount != 1 || opens < 2 {
		t.Fatalf("err=%v submitted=%d opens=%d", err, action.rebootCount, opens)
	}
}
