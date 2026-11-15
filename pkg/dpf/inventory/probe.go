// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package inventory

type Interface struct {
	AdminUp   *bool    `json:"adminUp,omitempty" yaml:"adminUp,omitempty"`
	OperState string   `json:"operState,omitempty" yaml:"operState,omitempty"`
	Name      string   `json:"name" yaml:"name"`
	Index     int      `json:"index" yaml:"index"`
	MTU       int      `json:"mtu" yaml:"mtu"`
	Addresses []string `json:"addresses" yaml:"addresses"`
}

// RouteLookup is the resolved IPv4 route to one requested destination.
type RouteLookup struct {
	Destination    string `json:"destination" yaml:"destination"`
	Source         string `json:"source,omitempty" yaml:"source,omitempty"`
	Gateway        string `json:"gateway,omitempty" yaml:"gateway,omitempty"`
	ReasonCode     string `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message        string `json:"message,omitempty" yaml:"message,omitempty"`
	InterfaceIndex int    `json:"interfaceIndex,omitempty" yaml:"interfaceIndex,omitempty"`
}
type Route struct {
	Destination    string `json:"destination" yaml:"destination"`
	Gateway        string `json:"gateway,omitempty" yaml:"gateway,omitempty"`
	InterfaceIndex int    `json:"interfaceIndex" yaml:"interfaceIndex"`
	Table          int    `json:"table" yaml:"table"`
}
type ProbeAddress struct {
	Address    string `json:"address" yaml:"address"`
	TCP        string `json:"tcp" yaml:"tcp"`
	TLS        string `json:"tls" yaml:"tls"`
	HTTP       string `json:"http" yaml:"http"`
	HTTPStatus int    `json:"httpStatus,omitempty" yaml:"httpStatus,omitempty"`
}
type ProbeResult struct {
	NodeUID     string         `json:"nodeUID" yaml:"nodeUID"`
	NetworkMode string         `json:"networkMode" yaml:"networkMode"`
	Destination string         `json:"destination" yaml:"destination"`
	DNS         string         `json:"dns" yaml:"dns"`
	Addresses   []ProbeAddress `json:"addresses" yaml:"addresses"`
	EvidenceRef string         `json:"evidenceRef" yaml:"evidenceRef"`
}
