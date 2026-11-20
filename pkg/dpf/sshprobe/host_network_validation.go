// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

// HostNetworkValidation reports current configuration, not an earlier
// transaction or a proof that this host has rebooted with that configuration.
type HostNetworkValidation struct {
	RuntimeVerified    bool   `json:"runtimeVerified"`
	PersistentVerified bool   `json:"persistentVerified"`
	BootID             string `json:"bootID,omitempty"`
	MTU                int    `json:"mtu,omitempty"`
	current            hostNetworkQualification
}

// hostNetworkQualification binds independently verified current configuration.
// It is private observation state, never a persisted transaction or evidence hash.
type hostNetworkQualification struct {
	present                                               bool
	identity                                              HostNetworkIdentity
	boot, source, runtime, fence, networkFile, management string
	mtu                                                   int
}

// Qualified reports whether this value contains a complete current read, rather
// than booleans assembled by a caller or an unsuccessful partial observation.
func (v HostNetworkValidation) Qualified() bool {
	return v.RuntimeVerified && v.PersistentVerified && v.current.present
}

// SameCurrent compares selected hardware, boot, runtime and persistent sources
// across fresh read-only qualifications. New SSH source ports are not identity.
func (v HostNetworkValidation) SameCurrent(other HostNetworkValidation) bool {
	return v.Qualified() && other.Qualified() && v.current == other.current
}

// VerifyHostWorkloadNetwork verifies the desired address, route and MTU against
// current runtime state and merged persistent Netplan configuration. LeaseBinding
// is not consulted: the caller binds these values to a fresh DPF allocation.
// It uses only the snapshot action, which neither creates a transaction
// directory nor acquires its lock, generates Netplan files or changes links.
func VerifyHostWorkloadNetwork(ctx context.Context, target config.Target, request HostNetworkRequest, timeout time.Duration) (result HostNetworkValidation, err error) {
	if ctx == nil || timeout < time.Second || timeout > 5*time.Minute {
		return result, networkError("HOST_NETWORK_INPUT_INVALID", false)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := validateHostNetworkAccess(ctx, target, request.Identity, timeout); err != nil {
		return result, err
	}
	if err := validateHostNetworkDesired(request); err != nil {
		return result, err
	}
	session, closeSession, err := openHostNetworkSession(ctx, target, timeout)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := closeSession(); closeErr != nil && err == nil {
			result = HostNetworkValidation{}
			err = networkError("HOST_NETWORK_SSH_CLEANUP_UNCONFIRMED", false)
		}
	}()
	return session.verifyConfiguration(ctx, request)
}

func (s *hostNetworkSession) verifyConfiguration(ctx context.Context, request HostNetworkRequest) (HostNetworkValidation, error) {
	if err := validateHostNetworkDesired(request); err != nil {
		return HostNetworkValidation{}, err
	}
	before, err := s.observe(ctx, request.Identity)
	if err != nil {
		return HostNetworkValidation{}, err
	}
	defer clear(before.merged)
	result, err := currentHostNetworkConfiguration(request, before)
	if err != nil {
		return result, err
	}
	after, err := s.observe(ctx, request.Identity)
	if err != nil {
		return HostNetworkValidation{}, err
	}
	defer clear(after.merged)
	if before.raw.BootID != after.raw.BootID || before.SourceDigest != after.SourceDigest ||
		before.raw.Fence != after.raw.Fence || before.raw.RuntimeDigest != after.raw.RuntimeDigest || before.connection != after.connection {
		return HostNetworkValidation{}, networkError("HOST_NETWORK_OBSERVATION_CHANGED", false)
	}
	return currentHostNetworkConfiguration(request, after)
}

func currentHostNetworkConfiguration(request HostNetworkRequest, observation HostNetworkObservation) (HostNetworkValidation, error) {
	plan, address, route, err := inspectHostNetworkConfiguration(request, observation)
	if err != nil {
		return HostNetworkValidation{}, err
	}
	result := HostNetworkValidation{
		RuntimeVerified:    observation.MTU == request.MTU && address && route,
		PersistentVerified: plan.MTUMatchesDesired && plan.AddressAlreadyDeclared && plan.RouteAlreadyDeclared,
		BootID:             observation.raw.BootID, MTU: observation.MTU,
	}
	if !result.RuntimeVerified {
		return result, networkError("HOST_NETWORK_RUNTIME_MISMATCH", false)
	}
	if !result.PersistentVerified {
		return result, networkError("HOST_NETWORK_PERSISTENT_MISMATCH", false)
	}
	result.current = hostNetworkQualification{present: true, identity: observation.Identity, boot: observation.raw.BootID, source: observation.SourceDigest, runtime: observation.raw.RuntimeDigest, fence: observation.raw.Fence, networkFile: observation.raw.NetworkFile, management: observation.ManagementInterface, mtu: observation.MTU}
	return result, nil
}
