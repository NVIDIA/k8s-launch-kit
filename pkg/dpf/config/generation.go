// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/hex"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"net/netip"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Artifact identifies exact release bytes. Generation never downloads it.
type Artifact struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// GenerationReleases lists the exact inspected API contracts, not production
// qualification claims or the discovery software-minimum catalog.
func GenerationReleases() []string { return []string{"26.8.0-rc.1", "26.8.0"} }

func generationRelease(value string) bool {
	for _, release := range GenerationReleases() {
		if release == value {
			return true
		}
	}
	return false
}

// Software separates firmware selected for a release from observed installed software.
type Software struct {
	OSISO       Artifact            `yaml:"osISO" json:"osISO"`
	PLDM        map[string]Artifact `yaml:"pldm" json:"pldm"`
	NICFirmware *Artifact           `yaml:"nicFirmware,omitempty" json:"nicFirmware,omitempty"`
}

// DeviceBinding names existing DPF enrollment objects; it never authorizes adoption
// of a DPU owned by another DPUSet or DPUDeployment.
type DeviceBinding struct {
	ID        string `yaml:"id" json:"id"`
	DPUDevice string `yaml:"dpuDevice" json:"dpuDevice"`
	DPUNode   string `yaml:"dpuNode" json:"dpuNode"`
}

type ClusterBinding struct {
	KubernetesVersion string            `yaml:"kubernetesVersion" json:"kubernetesVersion"`
	Name              string            `yaml:"name" json:"name"`
	Namespace         string            `yaml:"namespace" json:"namespace"`
	ClusterUID        string            `yaml:"clusterUID" json:"clusterUID"`
	ResourceUID       string            `yaml:"resourceUID" json:"resourceUID"`
	Selector          map[string]string `yaml:"selector" json:"selector"`
}

type Provisioning struct {
	Devices    []DeviceBinding `yaml:"devices" json:"devices"`
	DPUCluster ClusterBinding  `yaml:"dpuCluster" json:"dpuCluster"`
	Software   Software        `yaml:"software" json:"software"`
}

type Sentry struct {
	LLMInspection        *bool   `yaml:"llmInspection,omitempty" json:"llmInspection,omitempty"`
	Namespace            *string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	ChartDirectory       string  `yaml:"chartDirectory" json:"chartDirectory"`
	IngressCIDR          string  `yaml:"ingressCIDR" json:"ingressCIDR"`
	IngressIP            string  `yaml:"ingressIP" json:"ingressIP"`
	ImagePullSecret      string  `yaml:"imagePullSecret" json:"imagePullSecret"`
	MITMSecret           string  `yaml:"mitmSecret" json:"mitmSecret"`
	InferenceSecret      string  `yaml:"inferenceSecret" json:"inferenceSecret"`
	PolicyServerAddress  string  `yaml:"policyServerAddress" json:"policyServerAddress"`
	PolicyServerNodePort *int    `yaml:"policyServerNodePort,omitempty" json:"policyServerNodePort,omitempty"`
}

// Generation is a pure, fully resolved input. It contains references, never
// credential contents or a Kubernetes client. Existing DPF management and tenant
// clusters and enrolled devices are prerequisites for this first deployment slice.
type Generation struct {
	ManagementKubernetesVersion string             `yaml:"managementKubernetesVersion" json:"managementKubernetesVersion"`
	Name                        string             `yaml:"name" json:"name"`
	Profile                     string             `yaml:"profile" json:"profile"`
	Release                     string             `yaml:"release" json:"release"`
	Namespace                   string             `yaml:"namespace" json:"namespace"`
	ManagementUID               string             `yaml:"managementUID" json:"managementUID"`
	KubeconfigRef               FileRef            `yaml:"kubeconfigRef" json:"kubeconfigRef"`
	Context                     string             `yaml:"context" json:"context"`
	Provisioning                Provisioning       `yaml:"provisioning" json:"provisioning"`
	Sentry                      Sentry             `yaml:"sentry" json:"sentry"`
	Devices                     []inventory.Device `yaml:"devices" json:"devices"`
	DevicePSIDs                 map[string]string  `yaml:"devicePSIDs" json:"devicePSIDs"`
}

func requiredDefault(p *string, fallback, field string) (string, error) {
	if p == nil {
		return fallback, nil
	}
	if *p == "" {
		return "", fmt.Errorf("%s must not be empty", field)
	}
	return *p, nil
}

// ResolveGeneration deliberately does not call Resolve: discovery resolves
// passwords and trust files, while local generation must never read them.
func ResolveGeneration(in Input) (Generation, error) {
	var g Generation
	var err error
	if g.Name, err = requiredDefault(in.Site.DPF.Name, "sentry", "/dpf/name"); err != nil {
		return g, err
	}
	if g.Profile, err = requiredDefault(in.Site.DPF.Profile, "sentry-bf4", "/dpf/profile"); err != nil {
		return g, err
	}
	if g.Namespace, err = requiredDefault(in.Site.DPF.Namespace, "dpf-operator-system", "/dpf/namespace"); err != nil {
		return g, err
	}
	if in.Site.DPF.Release == nil {
		return g, fmt.Errorf("/dpf/release is required for generation; select the installed DPF release explicitly")
	}
	g.Release = *in.Site.DPF.Release
	if !generationRelease(g.Release) {
		return g, fmt.Errorf("DPF release %q has no Sentry BF4 generation contract", g.Release)
	}
	if g.Profile != "sentry-bf4" {
		return g, fmt.Errorf("unknown DPF profile %q", g.Profile)
	}
	for field, value := range map[string]string{"/dpf/name": g.Name, "/dpf/namespace": g.Namespace} {
		if len(validation.IsDNS1123Label(value)) != 0 || len(value) > 40 {
			return g, fmt.Errorf("%s must be a DNS label of at most 40 characters", field)
		}
	}
	if in.Site.Provisioning == nil || in.Site.Sentry == nil {
		return g, fmt.Errorf("/provisioning and /sentry are required for generation")
	}
	// Round-trip the typed input to isolate maps, slices and pointer values.
	b, err := yaml.Marshal(in.Site.Provisioning)
	if err != nil {
		return g, err
	}
	if err = yaml.Unmarshal(b, &g.Provisioning); err != nil {
		return g, err
	}
	b, err = yaml.Marshal(in.Site.Sentry)
	if err != nil {
		return g, err
	}
	if err = yaml.Unmarshal(b, &g.Sentry); err != nil {
		return g, err
	}
	g.KubeconfigRef.File = resolvePath(filepath.Dir(in.Path), in.Site.Management.KubeconfigRef.File)
	if in.Site.Management.KubeconfigRef.File == "" {
		return g, fmt.Errorf("/management/kubeconfigRef/file is required (reference only; not read during generation)")
	}
	if in.Site.Management.Context == nil || *in.Site.Management.Context == "" {
		return g, fmt.Errorf("/management/context is required; ambient current-context is not a deployment binding")
	}
	g.Context = *in.Site.Management.Context
	if in.Site.Management.ClusterUID != nil {
		g.ManagementUID = *in.Site.Management.ClusterUID
	}
	inv := in.Site.Inventory
	if inv == nil || inv.FormatVersion != inventory.FormatVersion {
		return g, fmt.Errorf("generation requires a supported discovery inventory")
	}
	if g.ManagementUID == "" && inv.Management != nil {
		g.ManagementUID = inv.Management.SystemNamespaceUID
	}
	if g.ManagementUID == "" {
		return g, fmt.Errorf("management cluster UID is unobserved; supply /management/clusterUID from kube-system before generation")
	}
	if inv.Management != nil && inv.Management.SystemNamespaceUID != "" && inv.Management.SystemNamespaceUID != g.ManagementUID {
		return g, fmt.Errorf("management cluster UID conflicts with discovery inventory")
	}
	if in.Site.Management.KubernetesVersion != nil {
		g.ManagementKubernetesVersion = *in.Site.Management.KubernetesVersion
	} else if inv.Management != nil {
		g.ManagementKubernetesVersion = inv.Management.KubernetesVersion
	}
	if err = validateKubernetesVersion(g.ManagementKubernetesVersion); err != nil {
		return g, fmt.Errorf("management Kubernetes version: %w", err)
	}
	if err = validateKubernetesVersion(g.Provisioning.DPUCluster.KubernetesVersion); err != nil {
		return g, fmt.Errorf("DPUCluster Kubernetes version: %w", err)
	}
	if err = resolveProvisioning(&g, inv); err != nil {
		return g, err
	}
	if err = resolveSentry(&g.Sentry); err != nil {
		return g, err
	}
	if g.Sentry.Namespace == nil {
		value := g.Name + "-services"
		g.Sentry.Namespace = &value
	}
	if !dnsLabel(*g.Sentry.Namespace) || *g.Sentry.Namespace == g.Namespace {
		return g, fmt.Errorf("/sentry/namespace must be a dedicated DNS namespace distinct from the DPF enrollment namespace")
	}
	g.Sentry.ChartDirectory = resolvePath(filepath.Dir(in.Path), g.Sentry.ChartDirectory)
	return g, ValidateGeneration(g)
}

func validateKubernetesVersion(value string) error {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(value, "v"))
	if err != nil || v.Major() != 1 {
		return fmt.Errorf("an explicit observed Kubernetes v1.x.y API server version is required")
	}
	return nil
}

func resolveProvisioning(g *Generation, inv *inventory.Inventory) error {
	p := &g.Provisioning
	if len(p.Devices) == 0 {
		return fmt.Errorf("/provisioning/devices must explicitly select at least one inventory device")
	}
	for field, value := range map[string]string{"dpuCluster/name": p.DPUCluster.Name, "dpuCluster/namespace": p.DPUCluster.Namespace} {
		if !dnsLabel(value) {
			return fmt.Errorf("/provisioning/%s must be a DNS label", field)
		}
	}
	if p.DPUCluster.ClusterUID == "" || p.DPUCluster.ResourceUID == "" {
		return fmt.Errorf("/provisioning/dpuCluster/clusterUID (tenant kube-system UID) and resourceUID (management DPUCluster UID) are required")
	}
	if len(p.DPUCluster.Selector) == 0 {
		return fmt.Errorf("/provisioning/dpuCluster/selector requires labels selecting exactly the intended existing DPUCluster")
	}
	for key, value := range p.DPUCluster.Selector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("invalid DPUCluster selector")
		}
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	g.DevicePSIDs = map[string]string{}
	for _, binding := range p.Devices {
		if binding.ID == "" || seen[binding.ID] {
			return fmt.Errorf("/provisioning/devices contains an empty or duplicate inventory id")
		}
		seen[binding.ID] = true
		if !dnsLabel(binding.DPUDevice) || !dnsLabel(binding.DPUNode) {
			return fmt.Errorf("device %s requires explicit DPUDevice and DPUNode DNS names", binding.ID)
		}
		if names[binding.DPUDevice] {
			return fmt.Errorf("duplicate DPUDevice binding %s", binding.DPUDevice)
		}
		names[binding.DPUDevice] = true
		matches := []inventory.Device{}
		for _, d := range inv.DPUs {
			if d.ID == binding.ID {
				matches = append(matches, d)
			}
		}
		if len(matches) != 1 || matches[0].SerialNumber == "" {
			return fmt.Errorf("device %s does not resolve to exactly one observed serial", binding.ID)
		}
		d := matches[0]
		if err := validateProvisioningDevice(d); err != nil {
			return err
		}
		associations := []inventory.Association{}
		for _, a := range inv.Associations {
			if a.DPUID == d.ID {
				associations = append(associations, a)
			}
		}
		if len(associations) != 1 || associations[0].SerialNumber != d.SerialNumber {
			return fmt.Errorf("device %s requires one unambiguous host association", binding.ID)
		}
		psids, err := measuredDevicePSIDs(d)
		if err != nil {
			return err
		}
		hosts := 0
		for _, h := range inv.Hosts {
			if h.ID == associations[0].HostID {
				hosts++
				collectRuntimePSIDs(psids, d.SerialNumber, h)
			}
		}
		if hosts != 1 {
			return fmt.Errorf("device %s host association does not resolve to one inventory host", binding.ID)
		}
		if len(psids) != 1 {
			return fmt.Errorf("device %s requires one observed, nonconflicting PSID", binding.ID)
		}
		for psid := range psids {
			if _, ok := p.Software.PLDM[psid]; !ok {
				return fmt.Errorf("device %s PSID %s has no selected PLDM artifact", binding.ID, psid)
			}
			g.DevicePSIDs[d.ID] = psid
		}
		b, err := yaml.Marshal(d)
		if err != nil {
			return err
		}
		var copy inventory.Device
		if err = yaml.Unmarshal(b, &copy); err != nil {
			return err
		}
		g.Devices = append(g.Devices, copy)
	}
	sort.Slice(p.Devices, func(i, j int) bool { return p.Devices[i].ID < p.Devices[j].ID })
	sort.Slice(g.Devices, func(i, j int) bool { return g.Devices[i].ID < g.Devices[j].ID })
	if err := validateArtifact(p.Software.OSISO); err != nil {
		return fmt.Errorf("/provisioning/software/osISO: %w", err)
	}
	if len(p.Software.PLDM) == 0 {
		return fmt.Errorf("/provisioning/software/pldm requires PSID-specific artifacts")
	}
	for psid, a := range p.Software.PLDM {
		if !strings.HasPrefix(psid, "MT_") || len(psid) > 32 {
			return fmt.Errorf("invalid PLDM PSID %q", psid)
		}
		if err := validateArtifact(a); err != nil {
			return fmt.Errorf("PLDM %s: %w", psid, err)
		}
	}
	if p.Software.NICFirmware != nil {
		return validateArtifact(*p.Software.NICFirmware)
	}
	return nil
}

// Validate source inventory and retained bundle devices by the same contract.
// Discovery evidence is a prerequisite; deploy still verifies live DPF access.
func validateProvisioningDevice(d inventory.Device) error {
	if d.ObservationState != "observed" || (d.Model != "BlueField-4" && (d.Model != "B4240" || d.ReaderAdapter != "bluefield-card-v1")) {
		return fmt.Errorf("device %s requires observed BlueField-4 identity; B4240 requires the bluefield-card-v1 reader", d.ID)
	}
	admin := d.Observations["bmc-administrator"]
	if admin.State != "observed" || admin.ReasonCode != "BMC_ADMIN_VERIFIED" || d.BMCEndpoint == "" || d.SSHEndpoint != "" {
		return fmt.Errorf("device %s requires verified BMC administrator discovery evidence; rerun discovery with an explicit DPU BMC endpoint and administrator credentials, then regenerate the bundle", d.ID)
	}
	if _, err := Origin(d.BMCEndpoint); err != nil {
		return fmt.Errorf("device %s has invalid BMC endpoint", d.ID)
	}
	return nil
}

// measuredDevicePSIDs shares source and retained-device consistency checks.
// Host runtime evidence is added by resolution using the verified association.
func measuredDevicePSIDs(d inventory.Device) (map[string]bool, error) {
	psids := map[string]bool{}
	psid, err := observedRedfishPSID(d)
	if err != nil {
		return nil, err
	}
	if psid != "" {
		psids[psid] = true
	}
	collectRuntimePSIDs(psids, d.SerialNumber, d)
	return psids, nil
}

func collectRuntimePSIDs(psids map[string]bool, serial string, d inventory.Device) {
	if d.Runtime == nil || d.Runtime.Hardware == nil {
		return
	}
	for _, card := range d.Runtime.Hardware.Cards {
		if card.SerialNumber == serial && card.PSID.State == "observed" {
			for _, value := range card.PSID.Values {
				if value != "" {
					psids[value] = true
				}
			}
		}
	}
}

// Redfish and SSH measurements may complement each other, but an explicitly
// contradictory or changed Redfish identity cannot be hidden by another source.
func observedRedfishPSID(d inventory.Device) (string, error) {
	observation := d.Observations["psid"]
	switch observation.ReasonCode {
	case "PSID_CONFLICT", "PSID_INVALID", "PSID_CHANGED", "PSID_RECHECK_FAILED", "PSID_NOT_VERIFIED":
		return "", fmt.Errorf("device %s has rejected Redfish PSID evidence (%s); resolve the reported cause and rediscover", d.ID, observation.ReasonCode)
	}
	if observation.State != "observed" && observation.ReasonCode != "PSID_OBSERVED" {
		return "", nil
	}
	if observation.State != "observed" || observation.ReasonCode != "PSID_OBSERVED" || observation.Tool != "redfish" || d.Model != "B4240" || d.ReaderAdapter != "bluefield-card-v1" || len(observation.Values) != 1 {
		return "", fmt.Errorf("device %s has inconsistent Redfish PSID evidence; rediscover", d.ID)
	}
	psid := observation.Values[0]
	if !strings.HasPrefix(psid, "MT_") || len(psid) > 32 || strings.IndexFunc(psid, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-')
	}) >= 0 {
		return "", fmt.Errorf("device %s Redfish PSID is not supported for PLDM selection", d.ID)
	}
	return psid, nil
}

func validateArtifact(a Artifact) error {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("artifact requires HTTPS URL without credentials, query or fragment")
	}
	h, err := hex.DecodeString(a.SHA256)
	if err != nil || len(h) != 32 || a.SHA256 != strings.ToLower(a.SHA256) {
		return fmt.Errorf("artifact requires lowercase SHA-256 digest")
	}
	return nil
}

func resolveSentry(s *Sentry) error {
	if s.LLMInspection == nil {
		value := false
		s.LLMInspection = &value
	}
	if s.ChartDirectory == "" {
		return fmt.Errorf("/sentry/chartDirectory requires local pinned Helm chart archives")
	}
	prefix, err := netip.ParsePrefix(s.IngressCIDR)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() > 30 || prefix.Bits() < 8 {
		return fmt.Errorf("/sentry/ingressCIDR requires a canonical IPv4 subnet (/8 through /30)")
	}
	ip, err := netip.ParseAddr(s.IngressIP)
	if err != nil || !prefix.Contains(ip) || ip == prefix.Addr() || !prefix.Contains(ip.Next()) {
		return fmt.Errorf("/sentry/ingressIP must be a usable address inside ingressCIDR")
	}
	for field, name := range map[string]string{"imagePullSecret": s.ImagePullSecret, "mitmSecret": s.MITMSecret} {
		if !dnsLabel(name) {
			return fmt.Errorf("/sentry/%s requires an existing Secret name", field)
		}
	}
	if *s.LLMInspection && !dnsLabel(s.InferenceSecret) {
		return fmt.Errorf("/sentry/inferenceSecret requires an existing Secret when llmInspection is enabled")
	}
	ip, err = netip.ParseAddr(s.PolicyServerAddress)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("/sentry/policyServerAddress requires a management-node IPv4 address reachable from the DPU")
	}
	if s.PolicyServerNodePort == nil {
		v := 30080
		s.PolicyServerNodePort = &v
	}
	if *s.PolicyServerNodePort < 30000 || *s.PolicyServerNodePort > 32767 {
		return fmt.Errorf("/sentry/policyServerNodePort must be within 30000-32767")
	}
	return nil
}
