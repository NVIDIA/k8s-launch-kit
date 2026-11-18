// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

func TestExternalRebooterContinuesAmbiguousPostSubmitHandshake(t *testing.T) {
	request := rebootConfig(t)
	for _, where := range []string{"open", "identity-read"} {
		t.Run(where, func(t *testing.T) {
			opens, actions := 0, 0
			rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
				opens++
				identity := sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}
				if opens == 1 {
					return &fakeExternalSession{identity: identity, onReboot: func() { actions++ }}, nil
				}
				if opens == 2 {
					interrupted := &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "handshake"}
					if where == "open" {
						return nil, interrupted
					}
					return &fakeExternalSession{observeErr: interrupted}, nil
				}
				identity.BootID = rebootNewID
				return &fakeExternalSession{identity: identity}, nil
			})
			got, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
			if err != nil || !got.Reconnected || got.CurrentBootID != rebootNewID || opens != 3 || actions != 1 {
				t.Fatalf("observation=%+v error=%v opens=%d actions=%d", got, err, opens, actions)
			}
		})
	}
}

func TestExternalRebooterKeepsExplicitObservationFailuresFatal(t *testing.T) {
	request := rebootConfig(t)
	for _, code := range []string{"SSH_HOST_KEY_REJECTED", "SSH_TRUST_INVALID", "SSH_CONFIGURATION_INVALID", "SSH_IDENTITY_INVALID"} {
		t.Run(code, func(t *testing.T) {
			opens, actions := 0, 0
			rebooter := newTestRebooter(t, request, time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
				opens++
				if opens == 1 {
					return &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}, onReboot: func() { actions++ }}, nil
				}
				return nil, &sshprobe.Error{Code: code, Stage: "handshake"}
			})
			started := time.Now()
			_, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
			if err == nil || sshprobe.Code(err) != code || actions != 1 || opens != 2 || time.Since(started) > 500*time.Millisecond {
				t.Fatalf("error=%v opens=%d actions=%d", err, opens, actions)
			}
		})
	}
}

func TestExternalRebooterPersistentHandshakeRemainsBoundedAndCausal(t *testing.T) {
	request := rebootConfig(t)
	opens, actions := 0, 0
	cause := &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "handshake"}
	rebooter := newTestRebooter(t, request, 100*time.Millisecond, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		if opens == 1 {
			return &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}, onReboot: func() { actions++ }}, nil
		}
		return nil, cause
	})
	started := time.Now()
	_, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
	if err == nil || !errors.Is(err, cause) || actions != 1 || opens < 3 || time.Since(started) > time.Second {
		t.Fatalf("error=%v opens=%d actions=%d elapsed=%v", err, opens, actions, time.Since(started))
	}
}

func TestExternalRebooterWaitKeepsUnknownRawCausePrivate(t *testing.T) {
	const canary = "private-remote-error-canary"
	cause := errors.New(canary)
	opens, actions := 0, 0
	rebooter := newTestRebooter(t, rebootConfig(t), 20*time.Millisecond, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		if opens == 1 {
			return &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}, onReboot: func() { actions++ }}, nil
		}
		return nil, cause
	})
	_, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
	if err == nil || strings.Contains(err.Error(), canary) || !errors.Is(err, cause) || actions != 1 {
		t.Fatalf("error=%v opens=%d actions=%d", err, opens, actions)
	}
}

func TestExternalRebooterDoesNotRetryHandshakeBeforeSubmission(t *testing.T) {
	opens := 0
	cause := &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "handshake"}
	rebooter := newTestRebooter(t, rebootConfig(t), time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		return nil, cause
	})
	_, err := rebooter.RebootExternal(context.Background(), activationRequest("SystemLevelReset"))
	if err == nil || !errors.Is(err, cause) || opens != 1 {
		t.Fatalf("error=%v opens=%d", err, opens)
	}
}

func TestExternalRebooterCancellationAfterTransientReadDoesNotReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opens, actions := 0, 0
	rebooter := newTestRebooter(t, rebootConfig(t), time.Second, time.Millisecond, func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error) {
		opens++
		if opens == 1 {
			return &fakeExternalSession{identity: sshprobe.HostIdentity{SystemUUID: rebootHostUUID, AdapterSerial: rebootCardSN, BootID: rebootOldID}, onReboot: func() { actions++ }}, nil
		}
		cancel()
		return nil, &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "handshake"}
	})
	_, err := rebooter.RebootExternal(ctx, activationRequest("SystemLevelReset"))
	if !errors.Is(err, context.Canceled) || opens != 2 || actions != 1 {
		t.Fatalf("error=%v opens=%d actions=%d", err, opens, actions)
	}
}
