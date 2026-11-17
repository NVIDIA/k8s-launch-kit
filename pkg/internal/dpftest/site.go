// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package dpftest supplies the resolved internal cohort used by DPF
// application and native adapter tests. It does not represent a public recipe.
package dpftest

import (
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"gopkg.in/yaml.v3"
)

// GenerationSite returns independent inputs with deliberately unavailable access references.
func GenerationSite() (config.Input, error) {
	useCase, vip, iface := "hbn-bgp", "192.0.2.20", "eth0"
	release, err := releases.Resolve("26.8.0")
	if err != nil {
		return config.Input{}, err
	}
	input := config.Input{Path: "/site/input.yaml", Source: []byte("# original source\n")}
	input.Site.DPF.UseCase = &useCase
	input.Site.Discovery.Hosts = []config.Endpoint{{SSH: &config.SSH{Address: "192.0.2.40"}, CredentialRef: "host"}}
	input.Site.Discovery.DPUs = []config.Endpoint{{BMCEndpoint: "https://192.0.2.30", CredentialRef: "bmc"}}
	missing := "/must-not-open/discovery-password"
	input.Site.Credentials = map[string]config.Credential{"bmc": {Username: "admin", PasswordRef: config.PasswordRef{File: &missing}}}
	input.Site.Management.KubeconfigRef.File = "/must-not-open/kubeconfig"
	input.Site.Provisioning = &config.Provisioning{
		Software:  config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: strings.Repeat("b", 64)}}},
		Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: &vip, Interface: &iface}},
		Flavor:    map[string]any{"dpuMode": "dpu"},
	}
	input.Site.Inventory = &inventory.Inventory{
		FormatVersion: inventory.FormatVersion, Software: &release, HBN: &inventory.HBNInventory{Namespace: "dpf-operator-system"},
		Management:   &inventory.Management{APIEndpoint: "https://192.0.2.10:6443", SystemNamespaceUID: "cluster-one", TrustDigest: "system-roots", KubernetesVersion: "v1.34.0"},
		DPUs:         []inventory.Device{{ID: "dpu-a", InputRef: "/discovery/dpus/0", BMCEndpoint: "https://192.0.2.30", SerialNumber: "SERIAL-A", ObservationState: "observed", PSID: "MT_0000001774", Model: "B4240", ReaderAdapter: "bluefield-card-v1", Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"}}}},
		Hosts:        []inventory.Device{{ID: "host-a", InputRef: "/discovery/hosts/0", SSHEndpoint: "ssh://192.0.2.40:22", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "adapter-a", IdentityState: "known", SerialNumber: "SERIAL-A", PCIAddress: "0000:01:00.0"}}}},
		Associations: []inventory.Association{{DPUID: "dpu-a", HostID: "host-a", HostAdapterKey: "adapter-a", SerialNumber: "SERIAL-A"}},
	}
	return input, nil
}

// PublicInventoryBytes projects resolved test inputs into the public inventory schema.
func PublicInventoryBytes(input config.Input) ([]byte, error) {
	settings := config.Configuration(input)
	var tenant *config.TenantInventory
	if p := input.Site.Provisioning; p != nil && p.Bootstrap != nil {
		b := p.Bootstrap
		tenant = &config.TenantInventory{VIP: b.Tenant.VIP, Interface: b.Tenant.Interface, Allocation: b.Tenant.Allocation, ManagementAPIIP: b.ManagementAPIIP, ManagementAPIPort: b.ManagementAPIPort}
	}
	d := input.Site.Discovery
	data, err := yaml.Marshal(config.ExternalInventory{
		Management: input.Site.Management, Credentials: input.Site.Credentials,
		Discovery: config.InventoryDiscoveryInput{DPUs: d.DPUs, Hosts: d.Hosts, Switches: d.Switches, TLS: d.TLS, Limits: d.Limits},
		Tenant:    tenant, Configuration: &settings, Inventory: input.Site.Inventory,
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}
