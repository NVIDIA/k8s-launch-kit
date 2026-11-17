// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package inventory

// AddressCandidate is an unverified locator, never physical identity or a connection grant.
type AddressCandidate struct {
	Name        string `json:"name" yaml:"name"`
	Role        string `json:"role" yaml:"role"`
	MAC         string `json:"mac" yaml:"mac"`
	Address     string `json:"address" yaml:"address"`
	Source      string `json:"source" yaml:"source"`
	EvidenceRef string `json:"evidenceRef" yaml:"evidenceRef"`
}
type AddressLookup struct {
	Candidates   []AddressCandidate     `json:"candidates" yaml:"candidates"`
	Observations map[string]Observation `json:"observations" yaml:"observations"`
}
