// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Package hostplatform defines the explicit platform selected for a Host operation.
package hostplatform

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// Selection is an immutable operation-local choice. Its zero value is invalid.
// Operator ownership (including external Helm) is independent of this choice.
type Selection string

const (
	Kubernetes Selection = config.FlavorK8s
	OpenShift  Selection = config.FlavorOCP
)

// Select snapshots the flavor after configuration resolution. An omitted flavor
// retains the Host CLI's Kubernetes default, including resource-only operations.
func Select(flavor string) (Selection, error) {
	if flavor == "" {
		return Kubernetes, nil
	}
	selected := Selection(flavor)
	return selected, selected.Validate()
}

// Validate rejects an unknown or omitted operation selection. Serialized config
// flavor is not consulted: consumers use this value as their only authority.
func (s Selection) Validate() error {
	if s != Kubernetes && s != OpenShift {
		return fmt.Errorf("invalid Host platform selection %q: expected %q or %q", s, Kubernetes, OpenShift)
	}
	return nil
}
