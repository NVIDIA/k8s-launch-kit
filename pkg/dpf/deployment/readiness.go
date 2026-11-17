// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

// Operational outcomes share the bounded status result without interpreting controller fields.
func result(code, message string) controllercontract.Readiness {
	return controllercontract.Readiness{ReasonCode: code, Message: message}
}
