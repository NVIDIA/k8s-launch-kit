// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

// BootstrapOptions supplies site allocations separately from profile defaults
// and observed device identities. It contains no access credentials.
type BootstrapOptions struct {
	OperatorConfigName    *string                          `yaml:"operatorConfigName,omitempty" json:"operatorConfigName,omitempty"`
	BMCFactoryResetPolicy *bootstrap.BMCFactoryResetPolicy `yaml:"bmcFactoryResetPolicy,omitempty" json:"bmcFactoryResetPolicy,omitempty"`
	ManagementAPIIP       *string                          `yaml:"managementAPIIP,omitempty" json:"managementAPIIP,omitempty"`
	ManagementAPIPort     *int                             `yaml:"managementAPIPort,omitempty" json:"managementAPIPort,omitempty"`
	ArtifactPVC           *string                          `yaml:"artifactPVC,omitempty" json:"artifactPVC,omitempty"`
	PodCIDR               *string                          `yaml:"podCIDR,omitempty" json:"podCIDR,omitempty"`
	Tenant                TenantOptions                    `yaml:"tenant,omitempty" json:"tenant,omitempty"`
}

type TenantOptions struct {
	Name         *string                   `yaml:"name,omitempty" json:"name,omitempty"`
	Namespace    *string                   `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	VIP          *string                   `yaml:"vip,omitempty" json:"vip,omitempty"`
	Interface    *string                   `yaml:"interface,omitempty" json:"interface,omitempty"`
	RouterID     *int                      `yaml:"routerID,omitempty" json:"routerID,omitempty"`
	MaxNodes     *int                      `yaml:"maxNodes,omitempty" json:"maxNodes,omitempty"`
	NodeSelector *map[string]string        `yaml:"nodeSelector,omitempty" json:"nodeSelector,omitempty"`
	Allocation   *bootstrap.SiteAllocation `yaml:"allocation,omitempty" json:"allocation,omitempty"`
}

// ResolveBootstrap computes a desired management bootstrap plan without reading
// credentials or contacting the cluster. observedAPI must come from the selected
// management inventory. Only its literal IP and port are eligible for defaulting;
// a hostname is not resolved speculatively. Missing site allocations remain
// explicit issues, allowing discovery to continue collecting other facts.
func ResolveBootstrap(options *BootstrapOptions, namespace, observedAPI string, selected []bootstrap.Device) (bootstrap.Inputs, []ProvisioningIssue, error) {
	definition, err := hbn.LoadDefinition()
	if err != nil {
		return bootstrap.Inputs{}, nil, err
	}
	d := definition.Defaults.Bootstrap
	in := bootstrap.Inputs{
		OperatorNamespace: namespace, OperatorConfigName: d.OperatorConfigName,
		BMCFactoryResetPolicy: bootstrap.BMCFactoryResetPolicy(d.BMCFactoryResetPolicy), PodCIDR: d.PodCIDR,
		Tenant:  bootstrap.Tenant{Name: d.TenantName, Namespace: d.TenantNamespace, RouterID: d.TenantRouterID, MaxNodes: d.TenantMaxNodes, NodeSelector: maps.Clone(d.TenantNodeSelector)},
		Devices: slices.Clone(selected),
	}
	in.Tenant.ServiceCIDR = d.TenantServiceCIDR
	if endpoint, err := url.Parse(observedAPI); err == nil && endpoint.Scheme == "https" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" && (endpoint.Path == "" || endpoint.Path == "/") {
		if ip, err := netip.ParseAddr(endpoint.Hostname()); err == nil && ip.IsGlobalUnicast() && !ip.Is4In6() {
			in.ManagementAPIIP = ip.String()
			in.ManagementAPIPort = 443
			if endpoint.Port() != "" {
				in.ManagementAPIPort, _ = strconv.Atoi(endpoint.Port())
			}
		}
	}
	if options == nil {
		options = &BootstrapOptions{}
	}
	assign := func(dst *string, value *string) {
		if value != nil {
			*dst = *value
		}
	}
	assign(&in.OperatorConfigName, options.OperatorConfigName)
	if options.BMCFactoryResetPolicy != nil {
		in.BMCFactoryResetPolicy = *options.BMCFactoryResetPolicy
	}
	assign(&in.ManagementAPIIP, options.ManagementAPIIP)
	if options.ManagementAPIPort != nil {
		in.ManagementAPIPort = *options.ManagementAPIPort
	}
	assign(&in.ArtifactPVC, options.ArtifactPVC)
	assign(&in.PodCIDR, options.PodCIDR)
	assign(&in.Tenant.Name, options.Tenant.Name)
	assign(&in.Tenant.Namespace, options.Tenant.Namespace)
	assign(&in.Tenant.VIP, options.Tenant.VIP)
	assign(&in.Tenant.Interface, options.Tenant.Interface)
	if err := bootstrap.ValidateBMCFactoryResetPolicy(in.BMCFactoryResetPolicy); err != nil {
		return bootstrap.Inputs{}, nil, fmt.Errorf("/provisioning/bootstrap/bmcFactoryResetPolicy: %w", err)
	}
	if options.Tenant.RouterID != nil {
		in.Tenant.RouterID = *options.Tenant.RouterID
	}
	if options.Tenant.MaxNodes != nil {
		in.Tenant.MaxNodes = *options.Tenant.MaxNodes
	}
	if options.Tenant.NodeSelector != nil {
		in.Tenant.NodeSelector = maps.Clone(*options.Tenant.NodeSelector)
	}
	in.Tenant.Allocation = options.Tenant.Allocation.Clone()
	identity := sha256.Sum256([]byte(in.Tenant.Namespace + "/" + in.Tenant.Name))
	in.Tenant.Labels = map[string]string{"launch-kit.nvidia.com/dpf-tenant": fmt.Sprintf("%x", identity[:16])}
	issues := []ProvisioningIssue{}
	for _, required := range []struct {
		field   string
		missing bool
	}{
		{"managementAPIIP", options.ManagementAPIIP == nil && in.ManagementAPIIP == ""},
		{"managementAPIPort", options.ManagementAPIPort == nil && in.ManagementAPIPort == 0},
		{"tenant.vip", options.Tenant.VIP == nil}, {"tenant.interface", options.Tenant.Interface == nil},
	} {
		if required.missing {
			issues = append(issues, ProvisioningIssue{Field: "/provisioning/bootstrap/" + required.field, ReasonCode: "BOOTSTRAP_INPUT_MISSING"})
		}
	}
	if len(issues) > 0 {
		return in, issues, nil
	}
	if err := bootstrap.Validate(in); err != nil {
		return bootstrap.Inputs{}, nil, fmt.Errorf("/provisioning/bootstrap: %w", err)
	}
	return in, issues, nil
}

func bootstrapIntegerPath(path string) bool {
	switch path {
	case "/provisioning/bootstrap/managementAPIPort", "/provisioning/bootstrap/tenant/routerID", "/provisioning/bootstrap/tenant/maxNodes":
		return true
	}
	return false
}
