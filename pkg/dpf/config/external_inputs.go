// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/addresslookup"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	"gopkg.in/yaml.v3"
)

// ManagementInput selects an explicit management cluster without reading it.
type ManagementInput struct {
	KubeconfigRef FileRef `yaml:"kubeconfigRef"`
	Context       *string `yaml:"context,omitempty"`
}

// DiscoveryInput selects authorized access scope and discovery controls.
type DiscoveryInput struct {
	AddressLookup *addresslookup.Config `yaml:"addressLookup,omitempty"`
	DPUs          []Endpoint            `yaml:"dpus"`
	Hosts         []Endpoint            `yaml:"hosts"`
	Switches      []Switch              `yaml:"switches,omitempty"`
	TLS           *TLS                  `yaml:"tls,omitempty"`
	Probes        Probes                `yaml:"probes,omitempty"`
	Limits        Limits                `yaml:"limits,omitempty"`
}

// InventoryDiscoveryInput explicitly exposes supported discovery inputs. Its
// probe controls omit the broader internal software-composition fields.
type InventoryDiscoveryInput struct {
	AddressLookup *addresslookup.Config `yaml:"addressLookup,omitempty"`
	DPUs          []Endpoint            `yaml:"dpus"`
	Hosts         []Endpoint            `yaml:"hosts"`
	Switches      []Switch              `yaml:"switches,omitempty"`
	TLS           *TLS                  `yaml:"tls,omitempty"`
	Probes        InventoryProbes       `yaml:"probes,omitempty"`
	Limits        Limits                `yaml:"limits,omitempty"`
}

type InventoryProbes struct {
	Mode      *string `yaml:"mode,omitempty"`
	Namespace *string `yaml:"namespace,omitempty"`
}

// TenantInventory contains environment allocations, never use-case defaults.
type TenantInventory struct {
	VIP               *string                   `yaml:"vip,omitempty"`
	Interface         *string                   `yaml:"interface,omitempty"`
	Allocation        *bootstrap.SiteAllocation `yaml:"allocation,omitempty"`
	ManagementAPIIP   *string                   `yaml:"managementAPIIP,omitempty"`
	ManagementAPIPort *int                      `yaml:"managementAPIPort,omitempty"`
}

// Overrides is the complete allowlist of public desired-policy inputs. A supplied
// overrides file replaces retained configuration; omission reuses it. This lets
// removing a value from the file restore the selected profile default.
type Overrides struct {
	UseCase   *string           `yaml:"useCase,omitempty"`
	Release   *string           `yaml:"release,omitempty"`
	Namespace *string           `yaml:"namespace,omitempty"`
	HBN       *HBNOverrides     `yaml:"hbn,omitempty"`
	Cluster   *ClusterOverrides `yaml:"cluster,omitempty"`
}

type HBNOverrides struct {
	Networking   *hbn.Options `yaml:"networking,omitempty"`
	LoopbackCIDR *string      `yaml:"loopbackCIDR,omitempty"`
	Memory       *string      `yaml:"memory,omitempty"`
}

type ClusterOverrides struct {
	OperatorConfigName *string            `yaml:"operatorConfigName,omitempty"`
	TenantName         *string            `yaml:"tenantName,omitempty"`
	TenantNamespace    *string            `yaml:"tenantNamespace,omitempty"`
	PodCIDR            *string            `yaml:"podCIDR,omitempty"`
	RouterID           *int               `yaml:"routerID,omitempty"`
	MaxNodes           *int               `yaml:"maxNodes,omitempty"`
	NodeSelector       *map[string]string `yaml:"nodeSelector,omitempty"`
}

// ExternalInventory separates authored environment information from retained
// allowed policy and tool-owned observations. Software composition and raw
// implementation cannot enter through either external input type.
type ExternalInventory struct {
	Management    ManagementInput         `yaml:"management,omitempty"`
	Credentials   map[string]Credential   `yaml:"credentials"`
	Discovery     InventoryDiscoveryInput `yaml:"discovery"`
	Tenant        *TenantInventory        `yaml:"tenant,omitempty"`
	Inventory     *inventory.Inventory    `yaml:"inventory,omitempty"`
	Configuration *Overrides              `yaml:"configuration,omitempty"`
}

// HasConfiguration distinguishes a fresh seed from a handoff that deliberately
// retained configuration, including an explicit empty configuration mapping.
func HasConfiguration(in Input) bool {
	if in.InventoryDocument == nil || len(in.InventoryDocument.Content) != 1 {
		return false
	}
	root := in.InventoryDocument.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "configuration" {
			return root.Content[i+1].Tag != "!!null"
		}
	}
	return false
}

// LoadInputs reads two bounded local YAML files. It does not open access
// references or read environment credentials, so generation remains offline.
func LoadInputs(inventoryPath, userConfigPath string) (Input, error) {
	abs, err := inputPath(inventoryPath, "--inventory")
	if err != nil {
		return Input{}, err
	}
	source, err := regularFile(abs, 16<<20)
	if err != nil {
		return Input{}, fmt.Errorf("%s: cannot read inventory", abs)
	}
	doc, err := externalYAML(source)
	if err != nil {
		return Input{}, fmt.Errorf("%s: %w", abs, err)
	}
	input, err := decodeExternalInventory(source, abs, doc)
	if err != nil {
		return Input{}, fmt.Errorf("%s: %w", abs, err)
	}
	if userConfigPath == "" {
		return input, nil
	}
	path, err := inputPath(userConfigPath, "--user-config")
	if err != nil {
		return Input{}, err
	}
	settings, err := regularFile(path, 16<<20)
	if err != nil {
		return Input{}, fmt.Errorf("%s: cannot read overrides", path)
	}
	settingsDoc, err := externalYAML(settings)
	if err != nil {
		return Input{}, fmt.Errorf("%s: %w", path, err)
	}
	var overrides Overrides
	if err := decodeExternal(settingsDoc, &overrides); err != nil {
		return Input{}, fmt.Errorf("%s: %w", path, err)
	}
	// The selected workflow survives a settings-only override file. Omitted
	// policy knobs still reset to their owning defaults.
	if overrides.UseCase == nil {
		overrides.UseCase = input.Site.DPF.UseCase
	}
	input, err = WithConfiguration(input, overrides)
	if err != nil {
		return Input{}, err
	}
	input.OverridesPath, input.OverridesSource = path, bytes.Clone(settings)
	return input, nil
}

func inputPath(value, flag string) (string, error) {
	if value == "" || value == "-" {
		return "", fmt.Errorf("%s must name one local YAML file", flag)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("invalid %s path", flag)
	}
	return abs, nil
}

func externalYAML(data []byte) (*yaml.Node, error) {
	if len(data) > 16<<20 {
		return nil, fmt.Errorf("YAML input exceeds 16 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc, extra yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return nil, yamlSyntaxDiagnostic(err)
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping document")
	}
	if err := validateKeys(&doc, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}, 0); err != nil {
		return nil, err
	}
	return yamlinput.Expand(&doc)
}

func externalDocument(doc *yaml.Node) bool {
	if doc == nil || len(doc.Content) != 1 {
		return false
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "tenant" || root.Content[i].Value == "configuration" {
			return true
		}
	}
	return false
}

func decodeExternalInventory(source []byte, path string, doc *yaml.Node) (Input, error) {
	var external ExternalInventory
	if err := decodeExternal(doc, &external); err != nil {
		return Input{}, err
	}
	if err := inventory.NormalizeInventory(external.Inventory); err != nil {
		return Input{}, err
	}
	d := external.Discovery
	site := Site{
		Management: external.Management, Credentials: external.Credentials, Inventory: external.Inventory,
		Discovery: DiscoveryInput{
			AddressLookup: d.AddressLookup, DPUs: d.DPUs, Hosts: d.Hosts,
			Switches: d.Switches, TLS: d.TLS, Limits: d.Limits,
			Probes: Probes{Mode: d.Probes.Mode, Namespace: d.Probes.Namespace},
		},
	}
	if external.Tenant != nil {
		t := external.Tenant
		site.Provisioning = &Provisioning{Bootstrap: &BootstrapOptions{ManagementAPIIP: t.ManagementAPIIP, ManagementAPIPort: t.ManagementAPIPort, Tenant: TenantOptions{VIP: t.VIP, Interface: t.Interface, Allocation: t.Allocation.Clone()}}}
	}
	in := Input{Site: site, Source: bytes.Clone(source), Path: path, External: true, InventoryDocument: doc}
	settings := Overrides{}
	if external.Configuration != nil {
		settings = *external.Configuration
	}
	return WithConfiguration(in, settings)
}

func decodeExternal(doc *yaml.Node, target any) error {
	if err := externalScalarTypes(doc.Content[0], ""); err != nil {
		return err
	}
	encoded, err := yamlinput.Encode(diagnosticBlock(doc))
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		message, field := "invalid external input type", ""
		if match := yamlUnknownField.FindStringSubmatch(err.Error()); len(match) == 2 {
			message, field = "unsupported external input field; use only inventory fields or supported overrides", match[1]
		}
		var normalized yaml.Node
		if yaml.Unmarshal(encoded, &normalized) != nil {
			return yamlDiagnostic(message, "", doc)
		}
		path, node := diagnosticLocation(&normalized, doc, diagnosticLine(err), field, "")
		return yamlDiagnostic(message, path, node)
	}
	return nil
}

func externalScalarTypes(node *yaml.Node, path string) error {
	// Observations use their existing versioned strict public decoder.
	if path == "/inventory" {
		return nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := externalScalarTypes(node.Content[i+1], path+"/"+node.Content[i].Value); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := externalScalarTypes(child, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil
		}
		p := strings.TrimPrefix(path, "/configuration")
		expected := "!!str"
		switch p {
		case "/hbn/networking/fabricASN", "/hbn/networking/dpuASNBase", "/hbn/networking/dpuASNStride", "/hbn/networking/uplinkMTU", "/hbn/networking/workloadMTU", "/hbn/networking/workloadPrefix", "/cluster/routerID", "/cluster/maxNodes", "/tenant/managementAPIPort", "/discovery/limits/maxConcurrentBMCs":
			expected = "!!int"
		}
		if strings.HasSuffix(p, "/tls/insecureSkipVerify") || strings.HasSuffix(p, "/ssh/sudo") || strings.HasPrefix(p, "/tenant/allocation/") {
			expected = "!!bool"
		}
		if node.Tag != expected {
			return yamlDiagnostic("invalid scalar type", path, node)
		}
	}
	return nil
}

// WithConfiguration explicitly composes allowed policy with current environment
// inputs. It preserves source bytes and both publication guards. Callers can use
// Configuration(retained) here when deployment omits a fresh overrides file.
func WithConfiguration(in Input, settings Overrides) (Input, error) {
	// Encoding and decoding the small typed override value provides independent
	// pointer/map ownership without sharing mutable defaults or caller inputs.
	data, err := yaml.Marshal(settings)
	if err != nil {
		return Input{}, err
	}
	var owned Overrides
	if err := yaml.Unmarshal(data, &owned); err != nil {
		return Input{}, err
	}
	site := in.Site
	site.DPF.UseCase, site.DPF.Release, site.DPF.Namespace = owned.UseCase, owned.Release, owned.Namespace
	site.DPF.HBN = nil
	var provisioning Provisioning
	if site.Provisioning != nil {
		provisioning = *site.Provisioning
	}
	var bootstrapOptions BootstrapOptions
	// Keep only the environment-owned bootstrap portion. No hidden previous
	// profile policy survives a supplied replacement overrides document.
	if previous := provisioning.Bootstrap; previous != nil {
		bootstrapOptions = BootstrapOptions{
			ManagementAPIIP: previous.ManagementAPIIP, ManagementAPIPort: previous.ManagementAPIPort,
			Tenant: TenantOptions{VIP: previous.Tenant.VIP, Interface: previous.Tenant.Interface, Allocation: previous.Tenant.Allocation.Clone()},
		}
	}
	provisioning.HBN, provisioning.Resources = nil, nil
	if owned.HBN != nil {
		site.DPF.HBN = owned.HBN.Networking
		if owned.HBN.LoopbackCIDR != nil {
			provisioning.HBN = &hbn.GenerationOptions{LoopbackCIDR: owned.HBN.LoopbackCIDR}
		}
		if owned.HBN.Memory != nil {
			provisioning.Resources = map[string]string{"memory": *owned.HBN.Memory}
		}
	}
	if owned.Cluster != nil {
		c := owned.Cluster
		bootstrapOptions.OperatorConfigName, bootstrapOptions.PodCIDR = c.OperatorConfigName, c.PodCIDR
		bootstrapOptions.Tenant.Name, bootstrapOptions.Tenant.Namespace = c.TenantName, c.TenantNamespace
		bootstrapOptions.Tenant.RouterID, bootstrapOptions.Tenant.MaxNodes, bootstrapOptions.Tenant.NodeSelector = c.RouterID, c.MaxNodes, c.NodeSelector
	}
	// Avoid manufacturing HBN provisioning merely for basic inventory collection.
	if site.Provisioning != nil || owned.HBN != nil || owned.Cluster != nil {
		provisioning.Bootstrap = &bootstrapOptions
		site.Provisioning = &provisioning
	}
	in.Site = site
	return internalDocument(in)
}

func internalDocument(in Input) (Input, error) {
	data, err := yaml.Marshal(in.Site)
	if err != nil {
		return Input{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Input{}, err
	}
	in.Document = &doc
	return in, nil
}

// Configuration projects only allowed policy, so hydrated software/flavor and
// fixed lifecycle values never become user input or saved configuration.
func Configuration(in Input) Overrides {
	s := in.Site
	result := Overrides{UseCase: s.DPF.UseCase, Release: s.DPF.Release, Namespace: s.DPF.Namespace}
	var h HBNOverrides
	h.Networking = s.DPF.HBN
	if p := s.Provisioning; p != nil {
		if p.HBN != nil {
			h.LoopbackCIDR = p.HBN.LoopbackCIDR
		}
		if memory, ok := p.Resources["memory"]; ok {
			h.Memory = &memory
		}
		if b := p.Bootstrap; b != nil {
			c := ClusterOverrides{OperatorConfigName: b.OperatorConfigName, TenantName: b.Tenant.Name, TenantNamespace: b.Tenant.Namespace, PodCIDR: b.PodCIDR, RouterID: b.Tenant.RouterID, MaxNodes: b.Tenant.MaxNodes, NodeSelector: b.Tenant.NodeSelector}
			if c != (ClusterOverrides{}) {
				result.Cluster = &c
			}
		}
	}
	if h != (HBNOverrides{}) {
		result.HBN = &h
	}
	return result
}

func savedExternal(in Input, inv *inventory.Inventory, destination string) ([]byte, error) {
	doc, err := yamlinput.Expand(in.InventoryDocument)
	if err != nil {
		return nil, err
	}
	var observations, settings yaml.Node
	if err := observations.Encode(inv); err != nil {
		return nil, err
	}
	effective, err := effectiveConfiguration(in)
	if err != nil {
		return nil, err
	}
	if err := settings.Encode(effective); err != nil {
		return nil, err
	}
	yamlinput.Set(doc.Content[0], "inventory", &observations)
	// Always include the typed configuration marker, even when empty, so the
	// saved public input remains distinguishable from legacy retained Site YAML.
	yamlinput.Set(doc.Content[0], "configuration", &settings)
	if filepath.Dir(destination) != filepath.Dir(in.Path) {
		if err := rebase(doc.Content[0], filepath.Dir(in.Path), filepath.Dir(destination), ""); err != nil {
			return nil, err
		}
	}
	return yamlinput.Encode(doc)
}

// effectiveConfiguration freezes supported desired defaults for phase handoff.
// It uses only embedded release/profile data and authored values, never access
// references. Environment allocation declarations remain in inventory.
func effectiveConfiguration(in Input) (Overrides, error) {
	settings := Configuration(in)
	if settings.UseCase == nil {
		return settings, nil
	}
	intent, err := ResolveIntent(in.Site)
	if err != nil {
		return Overrides{}, err
	}
	settings.Release, settings.Namespace = &intent.Release.Version, &intent.Namespace
	if intent.Networking == nil || intent.Generation == nil {
		return settings, nil
	}
	p := intent.Networking
	settings.HBN = &HBNOverrides{Networking: &hbn.Options{FabricVRF: &p.FabricVRF, FabricASN: &p.FabricASN, DPUASNBase: &p.DPUASNBase, DPUASNStride: &p.DPUASNStride, UplinkMTU: &p.UplinkMTU, WorkloadMTU: &p.WorkloadMTU, WorkloadCIDR: &p.WorkloadCIDR, WorkloadPrefix: &p.WorkloadPrefix}, LoopbackCIDR: &intent.Generation.LoopbackCIDR}
	var suppliedResources map[string]string
	if in.Site.Provisioning != nil {
		suppliedResources = in.Site.Provisioning.Resources
	}
	resources, err := ResolveHBNResources(suppliedResources)
	if err != nil {
		return Overrides{}, err
	}
	if memory, ok := resources["memory"]; ok {
		settings.HBN.Memory = &memory
	}
	definition, err := hbn.LoadDefinition()
	if err != nil {
		return Overrides{}, err
	}
	d := definition.Defaults.Bootstrap
	c := ClusterOverrides{}
	if settings.Cluster != nil {
		c = *settings.Cluster
	}
	defaultPointer(&c.OperatorConfigName, d.OperatorConfigName)
	defaultPointer(&c.TenantName, d.TenantName)
	defaultPointer(&c.TenantNamespace, d.TenantNamespace)
	defaultPointer(&c.PodCIDR, d.PodCIDR)
	defaultPointer(&c.RouterID, d.TenantRouterID)
	defaultPointer(&c.MaxNodes, d.TenantMaxNodes)
	defaultPointer(&c.NodeSelector, d.TenantNodeSelector)
	settings.Cluster = &c
	return settings, nil
}

func defaultPointer[T any](destination **T, value T) {
	if *destination == nil {
		*destination = &value
	}
}

// VerifyRecordedConfiguration prevents generation from reusing observations
// collected for different desired settings. Semantic normalization permits
// equivalent explicit/default values while requiring discovery after changes.
// Call this at the saved-inventory generation boundary, not during discovery or
// the deployment path that has just collected fresh prerequisite observations.
func VerifyRecordedConfiguration(in Input) error {
	if !in.External {
		return nil
	}
	if !HasConfiguration(in) {
		return fmt.Errorf("inventory lacks recorded configuration; run discovery with the intended use case and overrides before generation")
	}
	var recorded ExternalInventory
	if err := decodeExternal(in.InventoryDocument, &recorded); err != nil {
		return fmt.Errorf("cannot read recorded discovery configuration: %w", err)
	}
	settings := Overrides{}
	if recorded.Configuration != nil {
		settings = *recorded.Configuration
	}
	before, err := WithConfiguration(in, settings)
	if err != nil {
		return err
	}
	expected, err := effectiveConfiguration(before)
	if err != nil {
		return fmt.Errorf("cannot resolve recorded discovery configuration: %w", err)
	}
	current, err := effectiveConfiguration(in)
	if err != nil {
		return err
	}
	expectedBytes, err := yaml.Marshal(expected)
	if err != nil {
		return err
	}
	currentBytes, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedBytes, currentBytes) {
		return fmt.Errorf("desired configuration differs from saved discovery; run discovery with the intended use case and overrides before generation")
	}
	return nil
}
