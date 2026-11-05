// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

// Scope records access policy without credentials. The SSH trust snapshot is
// hashed so changing a trust file invalidates an earlier discovery scope.
func Scope(t Target) inventory.ScopeEndpoint {
	s := inventory.ScopeEndpoint{Role: t.Role, Endpoint: t.Endpoint, SystemURI: t.SystemURI, TLSInsecureSkipVerify: t.Access.InsecureSkipVerify()}
	if t.SSH != nil {
		s.SSHTrustDigest = inventory.Digest("dpf-ssh-trust-v1", t.SSH.KnownHosts())
		s.SSHSudo = t.SSH.Sudo
	}
	return s
}
