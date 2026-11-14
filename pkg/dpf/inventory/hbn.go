// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

// SwitchPort is an intended physical connection. It does not establish cabling.
type SwitchPort struct {
	Name         string `json:"name" yaml:"name"`
	DPUSerial    string `json:"dpuSerial" yaml:"dpuSerial"`
	DPUInterface string `json:"dpuInterface" yaml:"dpuInterface"`
}

type FabricSwitch struct {
	Name     string          `json:"name" yaml:"name"`
	Endpoint string          `json:"endpoint" yaml:"endpoint"`
	Platform string          `json:"platform" yaml:"platform"`
	Ports    []SwitchPort    `json:"ports" yaml:"ports"`
	Snapshot fabric.Snapshot `json:"snapshot" yaml:"snapshot"`
}

type ASNAssignment struct {
	DPUSerial string `json:"dpuSerial" yaml:"dpuSerial"`
	ASN       uint32 `json:"asn" yaml:"asn"`
}

// HBNInventory separates resolved intent from independently observed switches.
type HBNInventory struct {
	Namespace  string          `json:"namespace" yaml:"namespace"`
	Management *HBNManagement  `json:"management,omitempty" yaml:"management,omitempty"`
	Profile    hbn.Profile     `json:"profile" yaml:"profile"`
	ASNs       []ASNAssignment `json:"asns,omitempty" yaml:"asns,omitempty"`
	Switches   []FabricSwitch  `json:"switches,omitempty" yaml:"switches,omitempty"`
}

// Readiness is use-case eligibility, independent of physical inventory success.
type Readiness struct {
	UseCase string  `json:"useCase"`
	State   string  `json:"state"`
	Checks  []Check `json:"checks"`
}
