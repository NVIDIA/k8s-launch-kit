// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package hbn resolves desired HBN networking defaults without inspecting or
// changing live devices, switches, or Kubernetes resources.
package hbn

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	dpfassets "github.com/nvidia/k8s-launch-kit/profiles"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	maxDevices     = 256
	minUplinkMTU   = 1280
	minWorkloadMTU = 576
	maxEthernetMTU = 9216
)

var vrfNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`)
var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var chartVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var startupVersionPattern = regexp.MustCompile(`^HBN [0-9]+\.[0-9]+\.[0-9]+$`)
var imageRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
var chartNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

type DPUSetStrategy string

const (
	DPUSetStrategyOnDelete      DPUSetStrategy = "OnDelete"
	DPUSetStrategyRollingUpdate DPUSetStrategy = "RollingUpdate"
)

type NodeEffect string

const (
	NodeEffectHold     NodeEffect = "hold"
	NodeEffectNoEffect NodeEffect = "noEffect"
)

// Options contains user-supplied HBN profile values. Nil means omitted and
// selects the profile default. Explicit zero and empty values are validated,
// not treated as omitted.
type Options struct {
	FabricVRF      *string `yaml:"fabricVRF,omitempty" json:"fabricVRF,omitempty"`
	FabricASN      *uint32 `yaml:"fabricASN,omitempty" json:"fabricASN,omitempty"`
	DPUASNBase     *uint32 `yaml:"dpuASNBase,omitempty" json:"dpuASNBase,omitempty"`
	DPUASNStride   *uint32 `yaml:"dpuASNStride,omitempty" json:"dpuASNStride,omitempty"`
	UplinkMTU      *int    `yaml:"uplinkMTU,omitempty" json:"uplinkMTU,omitempty"`
	WorkloadMTU    *int    `yaml:"workloadMTU,omitempty" json:"workloadMTU,omitempty"`
	WorkloadCIDR   *string `yaml:"workloadCIDR,omitempty" json:"workloadCIDR,omitempty"`
	WorkloadPrefix *int    `yaml:"workloadPrefix,omitempty" json:"workloadPrefix,omitempty"`
}

// Profile is resolved desired HBN configuration. It is not discovery evidence
// and does not establish that a value is free or configured on the fabric.
type Profile struct {
	FabricVRF      string `yaml:"fabricVRF" json:"fabricVRF"`
	FabricASN      uint32 `yaml:"fabricASN" json:"fabricASN"`
	DPUASNBase     uint32 `yaml:"dpuASNBase" json:"dpuASNBase"`
	DPUASNStride   uint32 `yaml:"dpuASNStride" json:"dpuASNStride"`
	UplinkMTU      int    `yaml:"uplinkMTU" json:"uplinkMTU"`
	WorkloadMTU    int    `yaml:"workloadMTU" json:"workloadMTU"`
	WorkloadCIDR   string `yaml:"workloadCIDR" json:"workloadCIDR"`
	WorkloadPrefix int    `yaml:"workloadPrefix" json:"workloadPrefix"`
}

// Resources are desired per-container HBN resource quantities. The HBN chart
// applies this same map to both requests and limits. These are profile defaults,
// not measurements of available DPU capacity.
type Resources map[string]string

// GenerationOptions contains optional user overrides for HBN manifest
// generation. Nil means omitted; explicit zero and empty values are validated.
type GenerationOptions struct {
	ChartRepository          *string         `yaml:"chartRepository,omitempty" json:"chartRepository,omitempty"`
	ChartName                *string         `yaml:"chartName,omitempty" json:"chartName,omitempty"`
	ChartVersion             *string         `yaml:"chartVersion,omitempty" json:"chartVersion,omitempty"`
	ChartDigest              *string         `yaml:"chartDigest,omitempty" json:"chartDigest,omitempty"`
	ImageRepository          *string         `yaml:"imageRepository,omitempty" json:"imageRepository,omitempty"`
	ImageTag                 *string         `yaml:"imageTag,omitempty" json:"imageTag,omitempty"`
	ImageDigest              *string         `yaml:"imageDigest,omitempty" json:"imageDigest,omitempty"`
	StartupVersion           *string         `yaml:"startupVersion,omitempty" json:"startupVersion,omitempty"`
	LoopbackCIDR             *string         `yaml:"loopbackCIDR,omitempty" json:"loopbackCIDR,omitempty"`
	WorkloadGatewayIndex     *int            `yaml:"workloadGatewayIndex,omitempty" json:"workloadGatewayIndex,omitempty"`
	HostAddressIndex         *int            `yaml:"hostAddressIndex,omitempty" json:"hostAddressIndex,omitempty"`
	DPUSetStrategy           *DPUSetStrategy `yaml:"dpuSetStrategy,omitempty" json:"dpuSetStrategy,omitempty"`
	NodeEffect               *NodeEffect     `yaml:"nodeEffect,omitempty" json:"nodeEffect,omitempty"`
	VXLANStartupProbeEnabled *bool           `yaml:"vxlanStartupProbeEnabled,omitempty" json:"vxlanStartupProbeEnabled,omitempty"`
}

// Generation is the resolved, desired manifest-generation contract. These
// values are profile inputs, not observations of installed artifacts or state.
type Generation struct {
	ChartRepository          string         `yaml:"chartRepository" json:"chartRepository"`
	ChartName                string         `yaml:"chartName" json:"chartName"`
	ChartVersion             string         `yaml:"chartVersion" json:"chartVersion"`
	ChartDigest              string         `yaml:"chartDigest" json:"chartDigest"`
	ImageRepository          string         `yaml:"imageRepository" json:"imageRepository"`
	ImageTag                 string         `yaml:"imageTag" json:"imageTag"`
	ImageDigest              string         `yaml:"imageDigest" json:"imageDigest"`
	StartupVersion           string         `yaml:"startupVersion" json:"startupVersion"`
	LoopbackCIDR             string         `yaml:"loopbackCIDR" json:"loopbackCIDR"`
	WorkloadGatewayIndex     int            `yaml:"workloadGatewayIndex" json:"workloadGatewayIndex"`
	HostAddressIndex         int            `yaml:"hostAddressIndex" json:"hostAddressIndex"`
	DPUSetStrategy           DPUSetStrategy `yaml:"dpuSetStrategy" json:"dpuSetStrategy"`
	NodeEffect               NodeEffect     `yaml:"nodeEffect" json:"nodeEffect"`
	VXLANStartupProbeEnabled bool           `yaml:"vxlanStartupProbeEnabled" json:"vxlanStartupProbeEnabled"`
}

// BootstrapDefaults contains profile-owned names and safe defaults for a
// separately resolved DPF management-plane bootstrap request. Address/interface
// allocations and management API coordinates are deliberately absent.
type BootstrapDefaults struct {
	OperatorConfigName    string            `yaml:"operatorConfigName" json:"operatorConfigName"`
	BMCFactoryResetPolicy string            `yaml:"bmcFactoryResetPolicy" json:"bmcFactoryResetPolicy"`
	TenantName            string            `yaml:"tenantName" json:"tenantName"`
	TenantNamespace       string            `yaml:"tenantNamespace" json:"tenantNamespace"`
	TenantMaxNodes        int               `yaml:"tenantMaxNodes" json:"tenantMaxNodes"`
	TenantRouterID        int               `yaml:"tenantRouterID" json:"tenantRouterID"`
	TenantNodeSelector    map[string]string `yaml:"tenantNodeSelector" json:"tenantNodeSelector"`
	PodCIDR               string            `yaml:"podCIDR" json:"podCIDR"`
	TenantServiceCIDR     string            `yaml:"tenantServiceCIDR" json:"tenantServiceCIDR"`
}

// Definition describes the embedded DPF HBN profile and its standalone
// defaults. It is loaded from the version-controlled profile asset.
type Definition struct {
	Plugin      string             `yaml:"plugin"`
	Name        string             `yaml:"name"`
	Target      string             `yaml:"target"`
	Description string             `yaml:"description"`
	Defaults    DefinitionDefaults `yaml:"defaults"`
}

type DefinitionDefaults struct {
	Release    string            `yaml:"release"`
	Namespace  string            `yaml:"namespace"`
	Networking Profile           `yaml:"networking"`
	Generation Generation        `yaml:"generation"`
	Resources  Resources         `yaml:"resources"`
	Bootstrap  BootstrapDefaults `yaml:"bootstrap"`
}

// LoadDefinition strictly decodes the build-embedded HBN profile. It performs
// no filesystem or network access and returns an independent value.
func LoadDefinition() (Definition, error) {
	return decodeDefinition(dpfassets.DPFHBNBGPProfile())
}

func decodeDefinition(data []byte) (Definition, error) {
	var definition Definition
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, fmt.Errorf("decode embedded DPF HBN profile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Definition{}, fmt.Errorf("embedded DPF HBN profile must contain one YAML document")
	}
	if err := validateDefinition(definition); err != nil {
		return Definition{}, fmt.Errorf("invalid embedded DPF HBN profile: %w", err)
	}
	return definition, nil
}

func validateDefinition(definition Definition) error {
	if definition.Plugin != "dpf" || definition.Name != "hbn-bgp" || definition.Target != "dpf" {
		return fmt.Errorf("profile metadata must identify plugin dpf, name hbn-bgp, and target dpf")
	}
	if strings.TrimSpace(definition.Description) == "" {
		return fmt.Errorf("profile description is required")
	}
	if strings.TrimSpace(definition.Defaults.Release) == "" || strings.TrimSpace(definition.Defaults.Namespace) == "" {
		return fmt.Errorf("profile defaults require a release and namespace")
	}
	if err := validateProfile(definition.Defaults.Networking); err != nil {
		return fmt.Errorf("invalid profile networking defaults: %w", err)
	}
	if err := ValidateGeneration(definition.Defaults.Networking, definition.Defaults.Generation); err != nil {
		return fmt.Errorf("invalid profile generation defaults: %w", err)
	}
	if err := ValidateResources(definition.Defaults.Resources); err != nil {
		return fmt.Errorf("invalid profile HBN resource defaults: %w", err)
	}
	if err := validateBootstrapDefaults(definition.Defaults.Bootstrap); err != nil {
		return fmt.Errorf("invalid profile bootstrap defaults: %w", err)
	}
	return nil
}

// ValidateResources checks resource names and positive Kubernetes quantities.
// The HBN chart consumes this one map as both requests and limits.
func ValidateResources(resources Resources) error {
	if len(resources) == 0 {
		return fmt.Errorf("resources must contain at least one quantity")
	}
	return validateResourceQuantities("resources", resources)
}

func validateResourceQuantities(field string, values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !validContainerResourceName(name) {
			return fmt.Errorf("%s contains invalid Kubernetes container resource name %q", field, name)
		}
		value := values[name]
		quantity, err := resource.ParseQuantity(value)
		if err != nil || quantity.Sign() <= 0 {
			return fmt.Errorf("%s quantity for resource %q must be a positive Kubernetes quantity", field, name)
		}
		if strings.Contains(name, "/") {
			_, exponent := quantity.AsCanonicalBytes(nil)
			if exponent < 0 {
				return fmt.Errorf("%s quantity for extended resource %q must be an integer", field, name)
			}
		}
	}
	return nil
}

func validContainerResourceName(name string) bool {
	if len(validation.IsQualifiedName(name)) != 0 {
		return false
	}
	if strings.Contains(name, "/") {
		domain, _, _ := strings.Cut(name, "/")
		return domain != "kubernetes.io" && domain != "k8s.io"
	}
	switch name {
	case "cpu", "memory", "ephemeral-storage":
		return true
	}
	if !strings.HasPrefix(name, "hugepages-") {
		return false
	}
	pageSize, err := resource.ParseQuantity(strings.TrimPrefix(name, "hugepages-"))
	return err == nil && pageSize.Sign() > 0
}

func validateBootstrapDefaults(defaults BootstrapDefaults) error {
	if defaults.BMCFactoryResetPolicy != "Never" && defaults.BMCFactoryResetPolicy != "OnInitialization" {
		return fmt.Errorf("bmcFactoryResetPolicy must be Never or OnInitialization")
	}
	if !chartNamePattern.MatchString(defaults.OperatorConfigName) || !chartNamePattern.MatchString(defaults.TenantName) || !chartNamePattern.MatchString(defaults.TenantNamespace) {
		return fmt.Errorf("operatorConfigName, tenantName, and tenantNamespace must be lowercase DNS-style names")
	}
	if defaults.TenantMaxNodes < 1 {
		return fmt.Errorf("tenantMaxNodes must be greater than zero")
	}
	if defaults.TenantRouterID < 1 || defaults.TenantRouterID > 255 {
		return fmt.Errorf("tenantRouterID must be between 1 and 255")
	}
	if len(defaults.TenantNodeSelector) == 0 {
		return fmt.Errorf("tenantNodeSelector must select at least one node label")
	}
	for key := range defaults.TenantNodeSelector {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("tenantNodeSelector keys must be nonempty")
		}
	}
	podCIDR, err := netip.ParsePrefix(defaults.PodCIDR)
	if err != nil || !podCIDR.Addr().Is4() || podCIDR != podCIDR.Masked() || !podCIDR.Addr().IsGlobalUnicast() {
		return fmt.Errorf("podCIDR must be a canonical IPv4 unicast CIDR")
	}
	serviceCIDR, err := netip.ParsePrefix(defaults.TenantServiceCIDR)
	if err != nil || !serviceCIDR.Addr().Is4() || serviceCIDR != serviceCIDR.Masked() || !serviceCIDR.Addr().IsGlobalUnicast() || serviceCIDR.Overlaps(podCIDR) {
		return fmt.Errorf("tenantServiceCIDR must be a canonical IPv4 unicast CIDR disjoint from the tenant podCIDR")
	}
	return nil
}

// ResolveGeneration fills omitted generation values from the embedded profile
// and validates the resulting coordinates and generation settings.
func ResolveGeneration(options GenerationOptions) (Generation, error) {
	definition, err := LoadDefinition()
	if err != nil {
		return Generation{}, err
	}
	generation := definition.Defaults.Generation
	if options.ChartRepository != nil {
		generation.ChartRepository = *options.ChartRepository
	}
	if options.ChartName != nil {
		generation.ChartName = *options.ChartName
	}
	if options.ChartVersion != nil {
		generation.ChartVersion = *options.ChartVersion
	}
	if options.ChartDigest != nil {
		generation.ChartDigest = *options.ChartDigest
	}
	if options.ImageRepository != nil {
		generation.ImageRepository = *options.ImageRepository
	}
	if options.ImageTag != nil {
		generation.ImageTag = *options.ImageTag
	}
	if options.ImageDigest != nil {
		generation.ImageDigest = *options.ImageDigest
	}
	if options.StartupVersion != nil {
		generation.StartupVersion = *options.StartupVersion
	}
	if options.LoopbackCIDR != nil {
		generation.LoopbackCIDR = *options.LoopbackCIDR
	}
	if options.WorkloadGatewayIndex != nil {
		generation.WorkloadGatewayIndex = *options.WorkloadGatewayIndex
	}
	if options.HostAddressIndex != nil {
		generation.HostAddressIndex = *options.HostAddressIndex
	}
	if options.DPUSetStrategy != nil {
		generation.DPUSetStrategy = *options.DPUSetStrategy
	}
	if options.NodeEffect != nil {
		generation.NodeEffect = *options.NodeEffect
	}
	if options.VXLANStartupProbeEnabled != nil {
		generation.VXLANStartupProbeEnabled = *options.VXLANStartupProbeEnabled
	}
	if err := validateGeneration(generation); err != nil {
		return Generation{}, err
	}
	return generation, nil
}

// ValidateGeneration checks generation settings together with their networking
// profile so address pools cannot overlap and the gateway fits each allocation.
func ValidateGeneration(profile Profile, generation Generation) error {
	if err := validateProfile(profile); err != nil {
		return fmt.Errorf("invalid HBN networking profile: %w", err)
	}
	if err := validateGeneration(generation); err != nil {
		return err
	}
	loopback, _ := netip.ParsePrefix(generation.LoopbackCIDR)
	workload, _ := netip.ParsePrefix(profile.WorkloadCIDR)
	if loopback.Overlaps(workload) {
		return fmt.Errorf("loopbackCIDR %q overlaps workloadCIDR %q", generation.LoopbackCIDR, profile.WorkloadCIDR)
	}
	allocationSize := uint64(1) << uint(32-profile.WorkloadPrefix)
	if uint64(generation.WorkloadGatewayIndex) >= allocationSize-1 {
		return fmt.Errorf("workloadGatewayIndex must fit within a workload allocation of prefix /%d", profile.WorkloadPrefix)
	}
	if _, _, _, err := workloadAddressPlan(profile, generation); err != nil {
		return err
	}
	return nil
}

func validateGeneration(generation Generation) error {
	chartRepository, err := url.Parse(generation.ChartRepository)
	if err != nil || chartRepository.Scheme != "https" || chartRepository.Host == "" || chartRepository.User != nil || chartRepository.RawQuery != "" || chartRepository.Fragment != "" || strings.TrimSpace(generation.ChartRepository) != generation.ChartRepository {
		return fmt.Errorf("chartRepository must be an HTTPS URL without credentials, query, or fragment")
	}
	if !chartNamePattern.MatchString(generation.ChartName) {
		return fmt.Errorf("chartName must be a lowercase DNS-style name")
	}
	if !chartVersionPattern.MatchString(generation.ChartVersion) {
		return fmt.Errorf("chartVersion must be an exact numeric major.minor.patch version")
	}
	if !sha256DigestPattern.MatchString(generation.ChartDigest) {
		return fmt.Errorf("chartDigest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if !imageRepositoryPattern.MatchString(generation.ImageRepository) {
		return fmt.Errorf("imageRepository must be a lowercase OCI repository path without tag or digest")
	}
	if !imageTagPattern.MatchString(generation.ImageTag) {
		return fmt.Errorf("imageTag must be a valid OCI tag without a digest")
	}
	if !sha256DigestPattern.MatchString(generation.ImageDigest) {
		return fmt.Errorf("imageDigest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if !startupVersionPattern.MatchString(generation.StartupVersion) {
		return fmt.Errorf("startupVersion must use the form HBN major.minor.patch")
	}
	loopback, err := netip.ParsePrefix(generation.LoopbackCIDR)
	if err != nil || !loopback.Addr().Is4() || loopback != loopback.Masked() || !loopback.Addr().IsGlobalUnicast() {
		return fmt.Errorf("loopbackCIDR %q must be a canonical IPv4 unicast CIDR", generation.LoopbackCIDR)
	}
	if generation.WorkloadGatewayIndex < 1 {
		return fmt.Errorf("workloadGatewayIndex must be greater than zero")
	}
	if generation.HostAddressIndex < 1 {
		return fmt.Errorf("hostAddressIndex must be greater than zero")
	}
	if generation.DPUSetStrategy != DPUSetStrategyOnDelete && generation.DPUSetStrategy != DPUSetStrategyRollingUpdate {
		return fmt.Errorf("dpuSetStrategy must be OnDelete or RollingUpdate")
	}
	if generation.NodeEffect != NodeEffectHold && generation.NodeEffect != NodeEffectNoEffect {
		return fmt.Errorf("nodeEffect must be hold or noEffect")
	}
	return nil
}

// Resolve fills omitted values from the embedded profile and validates all
// supplied and defaulted values.
func Resolve(options Options) (Profile, error) {
	definition, err := LoadDefinition()
	if err != nil {
		return Profile{}, err
	}
	profile := definition.Defaults.Networking

	if options.FabricVRF != nil {
		profile.FabricVRF = *options.FabricVRF
	}
	if options.FabricASN != nil {
		profile.FabricASN = *options.FabricASN
	}
	if options.DPUASNBase != nil {
		profile.DPUASNBase = *options.DPUASNBase
	}
	if options.DPUASNStride != nil {
		profile.DPUASNStride = *options.DPUASNStride
	}
	if options.UplinkMTU != nil {
		profile.UplinkMTU = *options.UplinkMTU
	}
	if options.WorkloadMTU != nil {
		profile.WorkloadMTU = *options.WorkloadMTU
	}
	if options.WorkloadCIDR != nil {
		profile.WorkloadCIDR = *options.WorkloadCIDR
	}
	if options.WorkloadPrefix != nil {
		profile.WorkloadPrefix = *options.WorkloadPrefix
	}

	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}

	return profile, nil
}

// AllocateASNs assigns private ASNs in serial sort order. The result is stable
// regardless of input order. This computes proposed values only; it cannot
// detect collisions with an external fabric not supplied to the caller.
func (profile Profile) AllocateASNs(serials []string) (map[string]uint32, error) {
	if len(serials) > maxDevices {
		return nil, fmt.Errorf("cannot allocate ASNs for %d devices; maximum is %d", len(serials), maxDevices)
	}
	if err := validateProfile(profile); err != nil {
		return nil, fmt.Errorf("invalid HBN profile: %w", err)
	}

	sorted := append([]string(nil), serials...)
	sort.Strings(sorted)
	for i, serial := range sorted {
		if strings.TrimSpace(serial) == "" {
			return nil, fmt.Errorf("serial at sorted index %d is empty", i)
		}
		if strings.TrimSpace(serial) != serial {
			return nil, fmt.Errorf("DPU serial %q has leading or trailing whitespace", serial)
		}
		if i > 0 && serial == sorted[i-1] {
			return nil, fmt.Errorf("duplicate DPU serial %q", serial)
		}
	}

	assignments := make(map[string]uint32, len(sorted))
	for i, serial := range sorted {
		asn := uint64(profile.DPUASNBase) + uint64(i)*uint64(profile.DPUASNStride)
		if asn > math.MaxUint32 {
			return nil, fmt.Errorf("DPU ASN overflow allocating serial %q", serial)
		}
		if !privateASN(uint32(asn)) {
			return nil, fmt.Errorf("DPU ASN %d for serial %q is outside the private ASN ranges", asn, serial)
		}
		if uint32(asn) == profile.FabricASN {
			return nil, fmt.Errorf("DPU ASN %d for serial %q collides with fabricASN", asn, serial)
		}
		assignments[serial] = uint32(asn)
	}
	return assignments, nil
}

func privateASN(asn uint32) bool {
	return (asn >= 64512 && asn <= 65534) || (asn >= 4200000000 && asn <= 4294967294)
}

func validateProfile(profile Profile) error {
	if !vrfNamePattern.MatchString(profile.FabricVRF) {
		return fmt.Errorf("fabricVRF must be 1-15 ASCII letters, digits, '_' or '-'")
	}
	if !privateASN(profile.FabricASN) {
		return fmt.Errorf("fabricASN %d is outside the private ASN ranges", profile.FabricASN)
	}
	if !privateASN(profile.DPUASNBase) {
		return fmt.Errorf("dpuASNBase %d is outside the private ASN ranges", profile.DPUASNBase)
	}
	if profile.DPUASNStride == 0 {
		return fmt.Errorf("dpuASNStride must be greater than zero")
	}
	if profile.DPUASNBase == profile.FabricASN {
		return fmt.Errorf("dpuASNBase must differ from fabricASN")
	}
	if profile.UplinkMTU < minUplinkMTU || profile.UplinkMTU > maxEthernetMTU {
		return fmt.Errorf("uplinkMTU must be between %d and %d", minUplinkMTU, maxEthernetMTU)
	}
	if profile.WorkloadMTU < minWorkloadMTU || profile.WorkloadMTU > maxEthernetMTU {
		return fmt.Errorf("workloadMTU must be between %d and %d", minWorkloadMTU, maxEthernetMTU)
	}
	if profile.WorkloadMTU > profile.UplinkMTU {
		return fmt.Errorf("workloadMTU %d must not exceed uplinkMTU %d", profile.WorkloadMTU, profile.UplinkMTU)
	}
	workloadPrefix, err := netip.ParsePrefix(profile.WorkloadCIDR)
	if err != nil || !workloadPrefix.Addr().Is4() || workloadPrefix != workloadPrefix.Masked() || !workloadPrefix.Addr().IsGlobalUnicast() {
		return fmt.Errorf("workloadCIDR %q must be a canonical IPv4 unicast CIDR", profile.WorkloadCIDR)
	}
	if profile.WorkloadPrefix < workloadPrefix.Bits() || profile.WorkloadPrefix > 32 {
		return fmt.Errorf("workloadPrefix %d must be between workloadCIDR prefix %d and 32", profile.WorkloadPrefix, workloadPrefix.Bits())
	}
	return nil
}
