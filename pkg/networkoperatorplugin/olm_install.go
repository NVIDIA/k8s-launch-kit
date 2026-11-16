// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// Offline fallbacks, used when discover cannot read a value from a PackageManifest.
const (
	olmCatalogCertified    = "certified-operators"
	olmCatalogRedHat       = "redhat-operators"
	olmCatalogSourceNS     = "openshift-marketplace"
	olmDefaultChannel      = "stable"
	olmNetworkOperatorPkg  = "nvidia-network-operator"
	olmMaintenanceOperator = "nvidia-maintenance-operator"
)

// fallbackOLMSource maps a package to its catalog: NVIDIA operators are
// certified, everything else is Red Hat.
func fallbackOLMSource(packageName string) string {
	switch packageName {
	case olmNetworkOperatorPkg, olmMaintenanceOperator:
		return olmCatalogCertified
	default:
		return olmCatalogRedHat
	}
}

// DefaultOCPOperatorSubscriptions returns offline install descriptors for the
// operators l8k configures (same selection as requiredOCPOperators). discover
// overrides channel/source/sourceNamespace from each PackageManifest.
func DefaultOCPOperatorSubscriptions(cfg *config.LaunchKitConfig) []config.OCPOperatorSubscription {
	operators := requiredOCPOperators(cfg, true)
	subs := make([]config.OCPOperatorSubscription, 0, len(operators))
	for _, op := range operators {
		subs = append(subs, config.OCPOperatorSubscription{
			Package:             op.packageName,
			Namespace:           op.namespace,
			Channel:             olmDefaultChannel,
			Source:              fallbackOLMSource(op.packageName),
			SourceNamespace:     olmCatalogSourceNS,
			InstallPlanApproval: config.DefaultOCPInstallPlanApproval,
		})
	}
	return subs
}

// EnsureOCPOperatorInstallDefaults backfills the ocpOperatorInstall block with
// offline defaults when management is on (e.g. generate without a prior
// discover). No-op otherwise.
func EnsureOCPOperatorInstallDefaults(cfg *config.LaunchKitConfig) {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return
	}
	if cfg.OCPOperatorInstall == nil || !cfg.OCPOperatorInstall.Manage {
		return
	}
	if len(cfg.OCPOperatorInstall.Operators) == 0 {
		cfg.OCPOperatorInstall.Operators = DefaultOCPOperatorSubscriptions(cfg)
		return
	}
	for i := range cfg.OCPOperatorInstall.Operators {
		op := &cfg.OCPOperatorInstall.Operators[i]
		if op.Channel == "" {
			op.Channel = olmDefaultChannel
		}
		if op.Source == "" {
			op.Source = fallbackOLMSource(op.Package)
		}
		if op.SourceNamespace == "" {
			op.SourceNamespace = olmCatalogSourceNS
		}
		if op.InstallPlanApproval == "" {
			op.InstallPlanApproval = config.DefaultOCPInstallPlanApproval
		}
	}
}
