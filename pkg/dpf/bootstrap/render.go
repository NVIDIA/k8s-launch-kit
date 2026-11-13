// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap renders management-plane DPF resources. Rendering performs
// no installation, credential access, address reservation, or enrollment.
package bootstrap

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"gopkg.in/yaml.v3"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Device is a selected physical identity. A singleton scan avoids enrolling
// other BMCs that happen to lie between two selected addresses. The lifecycle
// must verify the returned DPUDevice serial before binding workload selectors.
type Device struct {
	Serial  string `yaml:"serial" json:"serial"`
	BMCIP   string `yaml:"bmcIP" json:"bmcIP"`
	BMCPort int    `yaml:"bmcPort" json:"bmcPort"`
}

// Tenant describes the explicitly allocated Kamaji control-plane endpoint.
// VIP and RouterID must be reserved on the selected management interfaces;
// syntactic validation here cannot prove that either allocation is free.
type Tenant struct {
	Name         string            `yaml:"name" json:"name"`
	Namespace    string            `yaml:"namespace" json:"namespace"`
	Labels       map[string]string `yaml:"labels" json:"labels"`
	VIP          string            `yaml:"vip" json:"vip"`
	Interface    string            `yaml:"interface" json:"interface"`
	RouterID     int               `yaml:"routerID" json:"routerID"`
	NodeSelector map[string]string `yaml:"nodeSelector" json:"nodeSelector"`
	MaxNodes     int               `yaml:"maxNodes" json:"maxNodes"`
	// ServiceCIDR is fixed by the selected DPF Kamaji controller. It is retained
	// for collision checks and live comparison, not emitted as an unsupported CR field.
	ServiceCIDR string `yaml:"serviceCIDR" json:"serviceCIDR"`
}

// Inputs is resolved desired state. Defaults belong to the selected profile,
// not to this renderer. ArtifactPVC optionally selects an existing claim in OperatorNamespace.
// An omitted claim uses DPF host storage. Either storage path needs live checks.
type Inputs struct {
	OperatorNamespace     string                `yaml:"operatorNamespace" json:"operatorNamespace"`
	OperatorConfigName    string                `yaml:"operatorConfigName" json:"operatorConfigName"`
	BMCFactoryResetPolicy BMCFactoryResetPolicy `yaml:"bmcFactoryResetPolicy" json:"bmcFactoryResetPolicy"`
	ManagementAPIIP       string                `yaml:"managementAPIIP" json:"managementAPIIP"`
	ManagementAPIPort     int                   `yaml:"managementAPIPort" json:"managementAPIPort"`
	ArtifactPVC           string                `yaml:"artifactPVC,omitempty" json:"artifactPVC,omitempty"`
	PodCIDR               string                `yaml:"podCIDR" json:"podCIDR"`
	Tenant                Tenant                `yaml:"tenant" json:"tenant"`
	Devices               []Device              `yaml:"devices" json:"devices"`
}

// BMCFactoryResetPolicy controls the reset policy stamped onto DPUDevices
// created by DPUDiscovery. The HBN profile resolves a value explicitly so
// omitted input cannot inherit DPF's hardware-reset default accidentally.
type BMCFactoryResetPolicy string

const (
	BMCFactoryResetPolicyNever            BMCFactoryResetPolicy = "Never"
	BMCFactoryResetPolicyOnInitialization BMCFactoryResetPolicy = "OnInitialization"
)

// ValidateBMCFactoryResetPolicy checks the DPF-supported per-device reset values.
func ValidateBMCFactoryResetPolicy(policy BMCFactoryResetPolicy) error {
	if policy != BMCFactoryResetPolicyNever && policy != BMCFactoryResetPolicyOnInitialization {
		return fmt.Errorf("bmcFactoryResetPolicy must be Never or OnInitialization")
	}
	return nil
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// Render emits separate ordered files for namespaces, operator configuration,
// tenant creation and selected BMC scans. The executor must insert readiness
// barriers between these phases; applying the files does not prove readiness.
// Secret contents are deliberately absent. The executor supplies the supported
// DPF BMC credential Secrets immediately before starting enrollment.
func Render(in Inputs) ([]bundle.File, error) {
	if err := Validate(in); err != nil {
		return nil, err
	}
	objects := []map[string]any{namespace(in.OperatorNamespace)}
	if in.Tenant.Namespace != in.OperatorNamespace {
		objects = append(objects, namespace(in.Tenant.Namespace))
	}
	provisioning := map[string]any{"installInterface": map[string]any{"installViaRedfish": map[string]any{
		"skipDPUNodeDiscovery":                     false,
		"discoveredDPUDeviceBMCFactoryResetPolicy": string(in.BMCFactoryResetPolicy),
	}}}
	if in.ArtifactPVC != "" {
		provisioning["bfbPVCName"] = in.ArtifactPVC
	}
	objects = append(objects, object("operator.dpu.nvidia.com/v1alpha1", "DPFOperatorConfig", in.OperatorConfigName, in.OperatorNamespace, map[string]any{
		"deploymentMode":         "zero-trust",
		"overrides":              map[string]any{"kubernetesAPIServerVIP": in.ManagementAPIIP, "kubernetesAPIServerPort": in.ManagementAPIPort},
		"dpuDetector":            map[string]any{"disable": true},
		"provisioningController": provisioning,
		"flannel":                map[string]any{"podCIDR": in.PodCIDR},
		"kamajiClusterManager":   map[string]any{"disable": false},
	}))
	cluster := object("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", in.Tenant.Name, in.Tenant.Namespace, map[string]any{
		"type": "kamaji", "maxNodes": in.Tenant.MaxNodes,
		"clusterEndpoint": map[string]any{"keepalived": map[string]any{"vip": in.Tenant.VIP, "interface": in.Tenant.Interface, "virtualRouterID": in.Tenant.RouterID, "nodeSelector": in.Tenant.NodeSelector}},
	})
	cluster["metadata"].(map[string]any)["labels"] = in.Tenant.Labels
	objects = append(objects, cluster)
	devices := slices.Clone(in.Devices)
	slices.SortFunc(devices, func(a, b Device) int { return strings.Compare(a.Serial, b.Serial) })
	for _, device := range devices {
		key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", device.Serial, device.BMCIP, device.BMCPort)))
		objects = append(objects, object("provisioning.dpu.nvidia.com/v1alpha1", "DPUDiscovery", fmt.Sprintf("l8k-%x", key[:16]), in.OperatorNamespace, map[string]any{
			"ipRangeSpec": map[string]any{"ipRange": map[string]any{"startIP": device.BMCIP, "endIP": device.BMCIP, "port": device.BMCPort}},
		}))
	}
	files := make([]bundle.File, 0, len(objects))
	for index, item := range objects {
		data, err := yaml.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode bootstrap resource %d: %w", index, err)
		}
		files = append(files, bundle.File{Path: fmt.Sprintf("bootstrap/%02d-%s.yaml", index+1, item["kind"]), Data: data})
	}
	return files, nil
}

// Validate checks semantic inputs without claiming live allocation or access.
func Validate(in Inputs) error {
	if err := ValidateBMCFactoryResetPolicy(in.BMCFactoryResetPolicy); err != nil {
		return err
	}
	for _, name := range []struct{ field, value string }{
		{"operatorNamespace", in.OperatorNamespace}, {"operatorConfigName", in.OperatorConfigName},
		{"tenant.namespace", in.Tenant.Namespace},
	} {
		if len(kvalidation.IsDNS1123Label(name.value)) != 0 {
			return fmt.Errorf("%s must be a nonempty DNS label", name.field)
		}
	}
	if in.ArtifactPVC != "" && len(kvalidation.IsDNS1123Subdomain(in.ArtifactPVC)) != 0 {
		return fmt.Errorf("artifactPVC must be a valid PVC name")
	}
	if len(kvalidation.IsDNS1035Label(in.Tenant.Name)) != 0 {
		return fmt.Errorf("tenant.name must be a DNS label starting with a letter")
	}
	management, err := netip.ParseAddr(in.ManagementAPIIP)
	if err != nil || !management.IsGlobalUnicast() || management.Is4In6() {
		return fmt.Errorf("managementAPIIP must be a unicast IP address")
	}
	if in.ManagementAPIPort < 1 || in.ManagementAPIPort > 65535 {
		return fmt.Errorf("managementAPIPort must be between 1 and 65535")
	}
	vip, err := netip.ParseAddr(in.Tenant.VIP)
	if err != nil || !vip.Is4() || !vip.IsGlobalUnicast() {
		return fmt.Errorf("tenant.vip must be a unicast IPv4 address")
	}
	podCIDR, err := netip.ParsePrefix(in.PodCIDR)
	if err != nil || !podCIDR.Addr().Is4() || !podCIDR.Addr().IsGlobalUnicast() || podCIDR != podCIDR.Masked() {
		return fmt.Errorf("podCIDR must be a canonical IPv4 unicast CIDR")
	}
	if podCIDR.Contains(vip) || podCIDR.Contains(management) {
		return fmt.Errorf("podCIDR overlaps a control-plane endpoint")
	}
	serviceCIDR, err := netip.ParsePrefix(in.Tenant.ServiceCIDR)
	if err != nil || !serviceCIDR.Addr().Is4() || !serviceCIDR.Addr().IsGlobalUnicast() || serviceCIDR != serviceCIDR.Masked() {
		return fmt.Errorf("the selected controller's tenant serviceCIDR must be a canonical IPv4 unicast CIDR")
	}
	// The supported 26.8 Kamaji controller hardcodes this range. This is an
	// upstream API limitation, not an override this renderer can implement.
	if in.Tenant.ServiceCIDR != "10.96.0.0/16" {
		return fmt.Errorf("DPF 26.8 Kamaji serviceCIDR is fixed at 10.96.0.0/16")
	}
	if podCIDR.Overlaps(serviceCIDR) {
		return fmt.Errorf("tenant podCIDR overlaps the selected controller's serviceCIDR")
	}
	if serviceCIDR.Contains(vip) || serviceCIDR.Contains(management) {
		return fmt.Errorf("tenant serviceCIDR overlaps a control-plane endpoint")
	}
	if vip == management {
		return fmt.Errorf("tenant.vip must differ from the management API address")
	}
	if !interfaceName.MatchString(in.Tenant.Interface) || in.Tenant.Interface == "." || in.Tenant.Interface == ".." {
		return fmt.Errorf("tenant.interface must be a concrete Linux interface name")
	}
	if in.Tenant.RouterID < 1 || in.Tenant.RouterID > 255 {
		return fmt.Errorf("tenant.routerID must be between 1 and 255")
	}
	if in.Tenant.MaxNodes < len(in.Devices) || in.Tenant.MaxNodes < 1 || in.Tenant.MaxNodes > 3000 {
		return fmt.Errorf("tenant.maxNodes must cover selected devices and be between 1 and 3000")
	}
	if len(in.Tenant.NodeSelector) == 0 || len(in.Tenant.Labels) == 0 {
		return fmt.Errorf("tenant node selector and identity labels are required")
	}
	for _, labels := range []map[string]string{in.Tenant.NodeSelector, in.Tenant.Labels} {
		for key, value := range labels {
			if len(kvalidation.IsQualifiedName(key)) != 0 || len(kvalidation.IsValidLabelValue(value)) != 0 {
				return fmt.Errorf("invalid tenant label or node selector")
			}
		}
	}
	if len(in.Devices) == 0 || len(in.Devices) > 256 {
		return fmt.Errorf("one to 256 selected DPUs are required")
	}
	serials, endpoints := map[string]bool{}, map[string]bool{}
	for _, device := range in.Devices {
		if device.Serial == "" || strings.TrimSpace(device.Serial) != device.Serial || serials[device.Serial] {
			return fmt.Errorf("selected DPU serials must be nonempty, trimmed and unique")
		}
		ip, err := netip.ParseAddr(device.BMCIP)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
			return fmt.Errorf("DPF enrollment requires explicit unicast IPv4 BMC addresses")
		}
		if podCIDR.Contains(ip) || serviceCIDR.Contains(ip) {
			return fmt.Errorf("tenant podCIDR or serviceCIDR overlaps a BMC address")
		}
		if ip == vip || ip == management {
			return fmt.Errorf("BMC address conflicts with a control-plane endpoint")
		}
		if device.BMCPort < 1 || device.BMCPort > 65535 {
			return fmt.Errorf("BMC port must be between 1 and 65535")
		}
		endpoint := fmt.Sprintf("%s:%d", device.BMCIP, device.BMCPort)
		if endpoints[endpoint] {
			return fmt.Errorf("duplicate selected BMC endpoint")
		}
		serials[device.Serial], endpoints[endpoint] = true, true
	}
	return nil
}

func object(api, kind, name, ns string, spec map[string]any) map[string]any {
	return map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}, "spec": spec}
}

func namespace(name string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name}}
}
