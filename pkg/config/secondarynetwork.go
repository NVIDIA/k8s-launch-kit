// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import "fmt"

// ComponentManagement is the desired owner of one secondary-network installer.
// Empty, like an omitted value, means auto for older/programmatic inputs.
type ComponentManagement string

const (
	ManagementAuto     ComponentManagement = "auto"
	ManagementManaged  ComponentManagement = "managed"
	ManagementExternal ComponentManagement = "external"
)

// SecondaryNetworkPolicy controls Multus separately from the reference CNI
// plugins: an existing Multus does not prove that plugin binaries are installed.
type SecondaryNetworkPolicy struct {
	Multus     ComponentManagement `yaml:"multus,omitempty"`
	CNIPlugins ComponentManagement `yaml:"cniPlugins,omitempty"`
}

// SecondaryNetworkObservation is API evidence, never desired ownership policy.
// An absent observation is different from an unsuccessful inspection.
type SecondaryNetworkObservation struct {
	Multus     ComponentObservation `yaml:"multus"`
	CNIPlugins ComponentObservation `yaml:"cniPlugins"`
}

type ComponentObservation struct {
	Status     string               `yaml:"status"`
	Reason     string               `yaml:"reason,omitempty"`
	Installers []InstallerReference `yaml:"installers,omitempty"`
}

type InstallerReference struct {
	Namespace  string `yaml:"namespace"`
	Name       string `yaml:"name"`
	UID        string `yaml:"uid"`
	PolicyName string `yaml:"policyName,omitempty"`
	PolicyUID  string `yaml:"policyUID,omitempty"`
}

const (
	InstallerNotDetected     = "notDetected"
	InstallerOperatorManaged = "operatorManaged"
	InstallerExternal        = "external"
	InstallerAmbiguous       = "ambiguous"
	InstallerUnavailable     = "unavailable"
)

// ValidateSecondaryNetworkConfig accepts uncertainty as valid discovery output.
// Whether auto ownership can be rendered is checked only during generation.
func ValidateSecondaryNetworkConfig(cfg *LaunchKitConfig) error {
	if cfg == nil {
		return nil
	}
	policy := SecondaryNetworkPolicy{}
	if cfg.NetworkOperator != nil && cfg.NetworkOperator.SecondaryNetwork != nil {
		policy = *cfg.NetworkOperator.SecondaryNetwork
	}
	for _, component := range []struct {
		name string
		mode ComponentManagement
	}{{"multus", policy.Multus}, {"cniPlugins", policy.CNIPlugins}} {
		switch component.mode {
		case "", ManagementAuto, ManagementExternal:
		case ManagementManaged:
			if cfg.Flavor == FlavorOCP {
				return fmt.Errorf("networkOperator.secondaryNetwork.%s cannot be managed on OpenShift", component.name)
			}
		default:
			return fmt.Errorf("networkOperator.secondaryNetwork.%s must be auto, managed or external, got %q", component.name, component.mode)
		}
	}
	if cfg.DiscoveredSecondaryNetwork != nil {
		for _, obs := range []ComponentObservation{cfg.DiscoveredSecondaryNetwork.Multus, cfg.DiscoveredSecondaryNetwork.CNIPlugins} {
			switch obs.Status {
			case InstallerNotDetected, InstallerOperatorManaged, InstallerExternal, InstallerAmbiguous, InstallerUnavailable:
			default:
				return fmt.Errorf("invalid discoveredSecondaryNetwork status %q", obs.Status)
			}
		}
	}
	return nil
}

// SecondaryNetworkDecision is transient rendering output; policy stays unchanged.
type SecondaryNetworkDecision struct {
	Multus, CNIPlugins bool
	Notes              []string
}

// ResolveSecondaryNetworkRendering combines desired ownership and discovery
// evidence. It must not be used in shared resolution: uncertain observations
// still need to be saved by discovery.
func ResolveSecondaryNetworkRendering(cfg *LaunchKitConfig) (SecondaryNetworkDecision, error) {
	d := SecondaryNetworkDecision{}
	if err := ValidateSecondaryNetworkConfig(cfg); err != nil {
		return d, err
	}
	if cfg == nil {
		return d, fmt.Errorf("configuration must not be nil")
	}
	if cfg.Flavor == FlavorOCP {
		return d, nil
	}
	policy := SecondaryNetworkPolicy{}
	if cfg.NetworkOperator != nil && cfg.NetworkOperator.SecondaryNetwork != nil {
		policy = *cfg.NetworkOperator.SecondaryNetwork
	}
	var observations [2]*ComponentObservation
	if cfg.DiscoveredSecondaryNetwork != nil {
		observations = [2]*ComponentObservation{&cfg.DiscoveredSecondaryNetwork.Multus, &cfg.DiscoveredSecondaryNetwork.CNIPlugins}
	}
	for i, component := range []struct {
		name string
		mode ComponentManagement
	}{{"multus", policy.Multus}, {"cniPlugins", policy.CNIPlugins}} {
		managed := true
		obs := observations[i]
		switch component.mode {
		case ManagementExternal:
			managed = false
		case ManagementManaged:
			if obs != nil && (obs.Status == InstallerExternal || obs.Status == InstallerAmbiguous) {
				d.Notes = append(d.Notes, component.name+": requested managed ownership conflicts with discovery; deployment will inspect live ownership")
			}
		default:
			if obs == nil || obs.Status == InstallerNotDetected {
				d.Notes = append(d.Notes, component.name+": ownership unverified; deployment will inspect recognized API installers")
			} else {
				switch obs.Status {
				case InstallerExternal:
					managed = false
				case InstallerOperatorManaged:
				case InstallerUnavailable, InstallerAmbiguous:
					return d, fmt.Errorf("cannot resolve automatic %s ownership: %s (%s); inspect the platform and select explicit managed/external ownership", component.name, obs.Status, obs.Reason)
				}
			}
		}
		if i == 0 {
			d.Multus = managed
		} else {
			d.CNIPlugins = managed
		}
	}
	return d, nil
}
