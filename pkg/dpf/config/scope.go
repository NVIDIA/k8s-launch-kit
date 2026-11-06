// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

// Scope records resolved access policy without credentials or trust material.
// Trust snapshots are hashed so changed trust invalidates an earlier scope.
func Scope(t Target) inventory.ScopeEndpoint {
	s := inventory.ScopeEndpoint{Role: t.Role, Endpoint: t.Endpoint, SystemURI: t.SystemURI, TLSInsecureSkipVerify: t.Access.InsecureSkipVerify()}
	if t.SSH != nil {
		s.SSHTrustDigest = inventory.Digest("dpf-ssh-trust-v1", t.SSH.KnownHosts())
		s.SSHSudo = t.SSH.Sudo
	} else {
		s.TLSTrustDigest = t.Access.TrustDigest()
	}
	return s
}
