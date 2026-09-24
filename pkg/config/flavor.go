// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"gopkg.in/yaml.v2"
)

const (
	FlavorK8s = "k8s"
	FlavorOCP = "ocp"

	DefaultSriovOperatorNamespace       = "openshift-sriov-network-operator"
	DefaultNFDOperatorNamespace         = "openshift-nfd"
	DefaultNFDConfigurationName         = "nfd-instance"
	DefaultMaintenanceOperatorNamespace = "nvidia-maintenance-operator"
)

type NFDConfig struct {
	OperatorNamespace string `yaml:"operatorNamespace,omitempty"`
	ConfigurationName string `yaml:"configurationName,omitempty"`
}

type flavorNamespacePresence struct {
	sriov, nfd, nfdName, maintenance bool
}

// ApplyFlavorDefaultsWithPresence uses the raw input's explicit YAML paths to
// preserve operator namespace choices through the central resolver's clone.
func ApplyFlavorDefaultsWithPresence(cfg *LaunchKitConfig, present PathSet) error {
	if cfg == nil {
		return fmt.Errorf("launch kit config must not be nil")
	}
	cfg.namespacePresence = flavorNamespacePresence{
		sriov:       present.Has("sriov.operatorNamespace"),
		nfd:         present.Has("nfd.operatorNamespace"),
		nfdName:     present.Has("nfd.configurationName"),
		maintenance: present.Has("maintenance.operatorNamespace"),
	}
	return ApplyFlavorDefaults(cfg)
}

func (cfg *LaunchKitConfig) recordFlavorNamespacePresence(source []byte) error {
	var fields struct {
		Sriov *struct {
			OperatorNamespace *string `yaml:"operatorNamespace"`
		} `yaml:"sriov"`
		NFD *struct {
			OperatorNamespace *string `yaml:"operatorNamespace"`
			ConfigurationName *string `yaml:"configurationName"`
		} `yaml:"nfd"`
		Maintenance *struct {
			OperatorNamespace *string `yaml:"operatorNamespace"`
		} `yaml:"maintenance"`
	}
	if err := yaml.Unmarshal(source, &fields); err != nil {
		return err
	}
	cfg.namespacePresence.sriov = fields.Sriov != nil && fields.Sriov.OperatorNamespace != nil
	cfg.namespacePresence.nfd = fields.NFD != nil && fields.NFD.OperatorNamespace != nil
	cfg.namespacePresence.nfdName = fields.NFD != nil && fields.NFD.ConfigurationName != nil
	cfg.namespacePresence.maintenance = fields.Maintenance != nil && fields.Maintenance.OperatorNamespace != nil
	return nil
}

// ApplyFlavorDefaults resolves the platform without changing explicit namespace
// choices. Call it after the final CLI override and before rendering.
func ApplyFlavorDefaults(cfg *LaunchKitConfig) error {
	if cfg == nil {
		return fmt.Errorf("launch kit config must not be nil")
	}
	if cfg.Flavor == "" {
		cfg.Flavor = FlavorK8s
	}
	if cfg.Flavor != FlavorK8s && cfg.Flavor != FlavorOCP {
		return fmt.Errorf("flavor must be %q or %q, got %q", FlavorK8s, FlavorOCP, cfg.Flavor)
	}
	if cfg.Flavor != FlavorOCP {
		if cfg.Sriov != nil && !cfg.namespacePresence.sriov && cfg.Sriov.OperatorNamespace == DefaultSriovOperatorNamespace {
			cfg.Sriov.OperatorNamespace = ""
		}
		if cfg.NFD != nil {
			if !cfg.namespacePresence.nfd && cfg.NFD.OperatorNamespace == DefaultNFDOperatorNamespace {
				cfg.NFD.OperatorNamespace = ""
			}
			if !cfg.namespacePresence.nfdName && cfg.NFD.ConfigurationName == DefaultNFDConfigurationName {
				cfg.NFD.ConfigurationName = ""
			}
			if cfg.NFD.OperatorNamespace == "" && cfg.NFD.ConfigurationName == "" {
				cfg.NFD = nil
			}
		}
		if cfg.Maintenance != nil && !cfg.namespacePresence.maintenance && cfg.Maintenance.OperatorNamespace == DefaultMaintenanceOperatorNamespace {
			cfg.Maintenance.OperatorNamespace = ""
		}
		return nil
	}
	if cfg.NetworkOperator == nil {
		cfg.NetworkOperator = &NetworkOperatorConfig{}
	}
	if cfg.Sriov == nil {
		cfg.Sriov = &SriovConfig{}
	}
	if cfg.Sriov.OperatorNamespace == "" {
		cfg.Sriov.OperatorNamespace = DefaultSriovOperatorNamespace
	}
	if cfg.NFD == nil {
		cfg.NFD = &NFDConfig{}
	}
	if cfg.NFD.OperatorNamespace == "" {
		cfg.NFD.OperatorNamespace = DefaultNFDOperatorNamespace
	}
	if cfg.NFD.ConfigurationName == "" {
		cfg.NFD.ConfigurationName = DefaultNFDConfigurationName
	}
	if cfg.Maintenance == nil {
		cfg.Maintenance = DefaultMaintenanceConfig()
	}
	if cfg.Maintenance.OperatorNamespace == "" {
		cfg.Maintenance.OperatorNamespace = DefaultMaintenanceOperatorNamespace
	}
	return nil
}
