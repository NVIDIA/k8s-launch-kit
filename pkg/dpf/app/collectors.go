// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifactcheck"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/switchprobe"
)

// StandaloneCollectors composes discovery transports without opening access
// references or contacting endpoints. Discover validates access and diagnostic
// assets before collection and owns prior-inventory fallback and publication.
func StandaloneCollectors(request config.Request, runDir string) discovery.Dependencies {
	provisioningRelease := ""
	if request.HBN != nil && request.Software != nil {
		provisioningRelease = request.Software.Version
	}
	return collectors(runDir, request.HBN != nil, provisioningRelease, NewHBNPermissionObserver().Observe)
}

func retainedCollectors(runDir string, retained *bundle.Bundle, plan *generation.Plan, resume bool, phase deployment.PrerequisitePhase) discovery.Dependencies {
	deps := collectors(runDir, true, plan.Intent().Release, NewHBNPermissionObserver().ObserveRetained(retained, plan))
	deps.RetainedProvisioning = plan
	deps.DeferHBNReadiness = resume || phase == deployment.AfterActivation
	if phase == deployment.AfterActivation {
		deps.ArtifactSourceMode = discovery.ArtifactInputsOnly
	}
	return deps
}

func collectors(runDir string, includeMACs bool, provisioningRelease string, permissions discovery.HBNPermissionObserver) discovery.Dependencies {
	return discovery.Dependencies{
		Switches: switchprobe.Collector{}, SSH: sshprobe.Collector{}, HostLLDP: sshprobe.Collector{},
		BMC:            redfish.Collector{IncludeNetworkFunctionMACs: includeMACs, ProvisioningRelease: provisioningRelease},
		Probes:         probe.Runner{Assets: catalog.ProbeAssets(), Journal: probe.FileJournal(filepath.Join(runDir, "resources.json"))},
		HBNPermissions: permissions, HBNEnrollment: hbnEnrollmentAccessCheck,
		Artifacts: artifactcheck.Verifier{}, HostNetworking: sshprobe.ObserveHostWorkloadNetwork,
	}
}
