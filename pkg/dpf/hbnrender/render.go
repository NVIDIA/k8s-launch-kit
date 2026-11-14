// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package hbnrender renders the DPF workload resources for the flat HBN BGP
// profile. It does not inspect or modify a cluster or a DPU.
package hbnrender

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"gopkg.in/yaml.v3"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	apiVersion = "svc.dpu.nvidia.com/v1alpha1"
	provAPI    = "provisioning.dpu.nvidia.com/v1alpha1"
)

// Device is an identity-bound DPU selected by discovery. SelectorLabels must
// be labels already present and verified on that DPUDevice; DPF selectors do
// not match spec.serialNumber.
type Device struct {
	Serial         string            `json:"serial"`
	PSID           string            `json:"psid"`
	SelectorLabels map[string]string `json:"selectorLabels"`
	ASN            uint32            `json:"asn"`
}

// Inputs contains values that cannot safely be inferred from the selected
// DPF release/profile. FlavorSpec and HBNResources are caller-reviewed values;
// this renderer intentionally does not invent BF4 provisioning settings or
// resource requests.
type Inputs struct {
	Namespace  string
	Generation hbn.Generation
	Profile    hbn.Profile
	Software   config.Software
	Devices    []Device
	// DPUClusterSelector must uniquely identify the intended tenant DPUCluster.
	// The lifecycle caller verifies the selected object identity before apply.
	DPUClusterSelector map[string]string
	FlavorSpec         map[string]any
	HBNResources       map[string]any
}

// Render produces deterministic Kubernetes YAML documents in dependency
// order. The returned files are independent of Inputs. The caller must validate
// selectors against the live DPUDevice objects immediately before deployment.
func Render(input Inputs) ([]bundle.File, error) {
	if err := validate(input); err != nil {
		return nil, err
	}
	devices := cloneDevices(input.Devices)
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	psids := make([]string, 0, len(devices))
	serials := make([]string, 0, len(devices))
	for _, device := range devices {
		psids = append(psids, device.PSID)
		serials = append(serials, device.Serial)
	}
	software, issues, err := config.ValidateProvisioningSoftware(&config.Provisioning{Software: input.Software}, psids)
	if err != nil {
		return nil, fmt.Errorf("invalid provisioning software: %w", err)
	}
	if len(issues) != 0 {
		return nil, fmt.Errorf("provisioning software is incomplete: %s", issues[0].Field)
	}
	asns, err := input.Profile.AllocateASNs(serials)
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ASN != asns[device.Serial] {
			return nil, fmt.Errorf("device %q ASN %d does not match profile assignment %d", device.Serial, device.ASN, asns[device.Serial])
		}
	}
	hostReservations, err := hbn.HostReservations(input.Profile, input.Generation)
	if err != nil {
		return nil, fmt.Errorf("build workload host reservations: %w", err)
	}

	pldm := make(map[string]string, len(software.PLDM))
	for psid, artifact := range software.PLDM {
		pldm[psid] = artifact.URL
	}
	blueFieldSpec := map[string]any{"osIso": software.OSISO.URL, "pldmFwBundle": pldm}
	pldmDigests := make(map[string]string, len(software.PLDM))
	for psid, artifact := range software.PLDM {
		pldmDigests[psid] = artifact.SHA256
	}
	blueFieldIdentity := map[string]any{"spec": blueFieldSpec, "osIsoSHA256": software.OSISO.SHA256, "pldmSHA256": pldmDigests}
	blueFieldName, err := immutableName("hbn-software", blueFieldIdentity)
	if err != nil {
		return nil, fmt.Errorf("encode BlueFieldSoftware spec: %w", err)
	}
	flavorSpec := cloneMap(input.FlavorSpec)
	flavorName, err := immutableName("hbn-flavor", flavorSpec)
	if err != nil {
		return nil, fmt.Errorf("encode DPUFlavor spec: %w", err)
	}
	resources := []map[string]any{
		resource(provAPI, "BlueFieldSoftware", blueFieldName, input.Namespace, blueFieldSpec),
		resource(provAPI, "DPUFlavor", flavorName, input.Namespace, flavorSpec),
		ipam("hbn-loopback", input.Namespace, input.Generation.LoopbackCIDR, 32, input.DPUClusterSelector, nil, nil),
		ipam("hbn-workload", input.Namespace, input.Profile.WorkloadCIDR, input.Profile.WorkloadPrefix, input.DPUClusterSelector, &input.Generation.WorkloadGatewayIndex, hostReservations),
		iface("p0", input.Namespace, "physical", "p0", nil),
		iface("p1", input.Namespace, "physical", "p1", nil),
		iface("pf0hpf", input.Namespace, "pf", "", map[string]any{"pfID": int64(0)}),
		serviceTemplate(input.Namespace, input.Generation, cloneMap(input.HBNResources)),
		serviceConfiguration(input.Namespace, input.Profile, input.Generation, devices),
		deployment(input.Namespace, devices, input.DPUClusterSelector, blueFieldName, flavorName, input.Generation.ChartName, input.Generation.NodeEffect, input.Generation.DPUSetStrategy),
	}
	files := make([]bundle.File, 0, len(resources))
	for i, object := range resources {
		data, err := yaml.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode resource %d: %w", i+1, err)
		}
		files = append(files, bundle.File{Path: fmt.Sprintf("%02d-%s.yaml", i+1, object["kind"]), Data: data})
	}
	return files, nil
}

func validate(input Inputs) error {
	if len(kvalidation.IsDNS1123Label(input.Namespace)) != 0 {
		return fmt.Errorf("namespace must be a DNS label")
	}
	if err := hbn.ValidateGeneration(input.Profile, input.Generation); err != nil {
		return fmt.Errorf("invalid HBN generation profile: %w", err)
	}
	if len(input.Generation.ChartName) > 28 {
		return fmt.Errorf("chartName must be at most 28 characters for the DPF deploymentServiceName API")
	}
	if input.Generation.VXLANStartupProbeEnabled {
		return fmt.Errorf("flat HBN profile requires vxlanStartupProbeEnabled=false")
	}
	if len(input.Devices) == 0 || len(input.Devices) > 256 {
		return fmt.Errorf("one to 256 selected devices are required")
	}
	if len(input.DPUClusterSelector) == 0 {
		return fmt.Errorf("verified tenant DPUCluster selector labels are required")
	}
	for key, value := range input.DPUClusterSelector {
		if len(kvalidation.IsQualifiedName(key)) != 0 || len(kvalidation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("invalid DPUCluster selector label")
		}
	}
	if len(input.FlavorSpec) == 0 {
		return fmt.Errorf("a reviewed concrete DPUFlavor spec is required")
	}
	if len(input.HBNResources) == 0 {
		return fmt.Errorf("explicit HBN resource requests are required")
	}
	loopback, _ := netip.ParsePrefix(input.Generation.LoopbackCIDR)
	if uint64(1)<<uint(32-loopback.Bits()) < uint64(len(input.Devices)) {
		return fmt.Errorf("loopbackCIDR does not contain enough /32 allocations for selected devices")
	}
	workload, _ := netip.ParsePrefix(input.Profile.WorkloadCIDR)
	if uint64(1)<<uint(input.Profile.WorkloadPrefix-workload.Bits()) < uint64(len(input.Devices)) {
		return fmt.Errorf("workloadCIDR does not contain enough /%d allocations for selected devices", input.Profile.WorkloadPrefix)
	}
	seenSerial, seenHostname := map[string]bool{}, map[string]bool{}
	selectors := make([]map[string]string, 0, len(input.Devices))
	for _, d := range input.Devices {
		if strings.TrimSpace(d.Serial) == "" || d.Serial != strings.TrimSpace(d.Serial) || seenSerial[d.Serial] {
			return fmt.Errorf("device serials must be nonempty, trimmed, and unique")
		}
		hostname := hbnHostname(d.Serial)
		if len(kvalidation.IsDNS1123Subdomain(hostname)) != 0 || seenHostname[hostname] {
			return fmt.Errorf("device %q produces an invalid or duplicate DPF HBN hostname %q", d.Serial, hostname)
		}
		seenHostname[hostname] = true
		if strings.TrimSpace(d.PSID) == "" || d.PSID != strings.TrimSpace(d.PSID) || len(d.SelectorLabels) == 0 {
			return fmt.Errorf("device %q requires a PSID and verified selector labels", d.Serial)
		}
		for _, previous := range selectors {
			if selectorsOverlap(previous, d.SelectorLabels) {
				return fmt.Errorf("device %q selector overlaps another selected DPU selector", d.Serial)
			}
		}
		selectors = append(selectors, d.SelectorLabels)
		for k, v := range d.SelectorLabels {
			if len(kvalidation.IsQualifiedName(k)) != 0 || len(kvalidation.IsValidLabelValue(v)) != 0 {
				return fmt.Errorf("device %q has an invalid selector label", d.Serial)
			}
		}
	}
	return nil
}

// hbnHostname mirrors the DPF 26.8 zero-trust naming path: discovery creates
// DPUNode `dpu-node-${lowercase serial}`, DPUSet derives the DPU name by
// appending the lowercase DPUDevice name, and the HBN chart sets HOSTNAME from
// the scheduled pod's spec.nodeName. Per-DPU values use this exact hostname as
// hostnamePattern, with no glob that could overlap another ASN assignment.
func hbnHostname(serial string) string {
	dpuDeviceName := strings.ToLower(serial)
	dpuNodeName := "dpu-node-" + dpuDeviceName
	return dpuNodeName + "-" + dpuDeviceName
}

func cloneDevices(in []Device) []Device {
	out := make([]Device, len(in))
	for i, d := range in {
		out[i] = d
		out[i].SelectorLabels = make(map[string]string, len(d.SelectorLabels))
		for k, v := range d.SelectorLabels {
			out[i].SelectorLabels[k] = v
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func resource(api, kind, name, namespace string, spec map[string]any) map[string]any {
	return map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec}
}

func ipam(name, namespace, network string, prefix int, clusterSelector map[string]string, gatewayIndex *int, excludeRanges []hbn.AddressRange) map[string]any {
	config := map[string]any{"network": network, "prefixSize": int64(prefix)}
	if gatewayIndex != nil {
		config["gatewayIndex"] = int64(*gatewayIndex)
	}
	if excludeRanges != nil {
		ranges := make([]map[string]any, len(excludeRanges))
		for i, r := range excludeRanges {
			ranges[i] = map[string]any{"startIP": r.StartIP, "endIP": r.EndIP}
		}
		config["excludeRanges"] = ranges
	}
	labels := cloneStringMap(clusterSelector)
	selector := map[string]any{"matchLabels": labels}
	return resource(apiVersion, "DPUServiceIPAM", name, namespace, map[string]any{"ipv4Network": config, "dpuClusterSelector": selector})
}

func iface(name, namespace, kind, physical string, extra map[string]any) map[string]any {
	spec := map[string]any{"interfaceType": kind}
	if kind == "physical" {
		spec["physical"] = map[string]any{"interfaceName": physical}
	}
	if kind == "pf" {
		spec["pf"] = extra
	}
	return resource(apiVersion, "DPUServiceInterface", name, namespace, map[string]any{"template": map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"interface": name}}, "spec": spec}}}})
}

func serviceTemplate(namespace string, coordinates hbn.Generation, resources map[string]any) map[string]any {
	return resource(apiVersion, "DPUServiceTemplate", coordinates.ChartName, namespace, map[string]any{
		"deploymentServiceName": coordinates.ChartName,
		"helmChart": map[string]any{
			"source": map[string]any{"repoURL": coordinates.ChartRepository, "version": coordinates.ChartVersion, "chart": coordinates.ChartName},
			"values": map[string]any{"image": map[string]any{"repository": coordinates.ImageRepository, "tag": coordinates.ImageTag + "@" + coordinates.ImageDigest}, "resources": resources, "hbnSidecar": map[string]any{"vxlanStartupProbe": map[string]any{"enabled": false}}},
		},
	})
}

func serviceConfiguration(namespace string, p hbn.Profile, coordinates hbn.Generation, devices []Device) map[string]any {
	perDPU := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		perDPU = append(perDPU, map[string]any{"hostnamePattern": hbnHostname(d.Serial), "values": map[string]any{"bgp_autonomous_system": d.ASN}})
	}
	perDPUYAML, _ := yaml.Marshal(perDPU)
	startup := fmt.Sprintf(`- header:
    model: bluefield
    nvue-api-version: nvue_v1
    rev-id: 1.0
    version: %s
- set:
    interface:
      lo:
        ip:
          address:
            {{ ipaddresses.ip_lo.ip }}/32: {}
        type: loopback
      p0_if,p1_if:
        type: swp
        link:
          mtu: %d
      pf0hpf_if:
        ip:
          address:
            {{ ipaddresses.ip_pf0hpf.cidr }}: {}
        type: swp
        link:
          mtu: %d
    router:
      bgp:
        autonomous-system: {{ config.bgp_autonomous_system }}
        enable: on
        graceful-restart:
          mode: full
        router-id: {{ ipaddresses.ip_lo.ip }}
    vrf:
      default:
        router:
          bgp:
            address-family:
              ipv4-unicast:
                enable: on
                redistribute:
                  connected:
                    enable: on
            enable: on
            neighbor:
              p0_if:
                peer-group: fabric
                type: unnumbered
              p1_if:
                peer-group: fabric
                type: unnumbered
            peer-group:
              fabric:
                address-family:
                  ipv4-unicast:
                    enable: on
                remote-as: external
`, coordinates.StartupVersion, p.UplinkMTU, p.WorkloadMTU)
	return resource(apiVersion, "DPUServiceConfiguration", coordinates.ChartName, namespace, map[string]any{
		"deploymentServiceName": coordinates.ChartName,
		"upgradePolicy":         map[string]any{"applyNodeEffect": false},
		"interfaces":            []map[string]any{{"name": "p0_if", "network": "mybrhbn"}, {"name": "p1_if", "network": "mybrhbn"}, {"name": "pf0hpf_if", "network": "mybrhbn"}},
		"serviceConfiguration": map[string]any{
			"serviceDaemonSet": map[string]any{"annotations": map[string]any{"k8s.v1.cni.cncf.io/networks": `[{"name":"iprequest","interface":"ip_lo","cni-args":{"poolNames":["hbn-loopback"],"poolType":"cidrpool"}},{"name":"iprequest","interface":"ip_pf0hpf","cni-args":{"poolNames":["hbn-workload"],"poolType":"cidrpool","allocateDefaultGateway":true}}]`}},
			"helmChart":        map[string]any{"values": map[string]any{"configuration": map[string]any{"perDPUValuesYAML": string(perDPUYAML), "startupYAMLJ2": startup}}},
		},
	})
}

func deployment(namespace string, devices []Device, clusterSelector map[string]string, blueFieldName, flavorName, serviceName string, nodeEffect hbn.NodeEffect, strategy hbn.DPUSetStrategy) map[string]any {
	dpuSets := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		dpuSets = append(dpuSets, map[string]any{"nameSuffix": nameSuffix(d.Serial), "dpuDeviceSelector": map[string]any{"matchLabels": cloneStringMap(d.SelectorLabels)}, "dpuClusterSelector": cloneStringMap(clusterSelector)})
	}
	switches := make([]map[string]any, 0, 3)
	for _, pair := range [][2]string{{"p0", "p0_if"}, {"p1", "p1_if"}, {"pf0hpf", "pf0hpf_if"}} {
		switches = append(switches, map[string]any{"ports": []map[string]any{{"serviceInterface": map[string]any{"matchLabels": map[string]any{"interface": pair[0]}}}, {"service": map[string]any{"name": serviceName, "interface": pair[1]}}}})
	}
	return resource(apiVersion, "DPUDeployment", "hbn-bgp", namespace, map[string]any{
		"dpus":          map[string]any{"blueFieldSoftware": blueFieldName, "flavor": flavorName, "nodeEffect": actionObject(nodeEffect), "dpuSetStrategy": map[string]any{"type": string(strategy)}, "dpuSets": dpuSets},
		"services":      map[string]any{serviceName: map[string]any{"serviceTemplate": serviceName, "serviceConfiguration": serviceName}},
		"serviceChains": map[string]any{"upgradePolicy": map[string]any{"applyNodeEffect": false}, "switches": switches},
	})
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func immutableName(prefix string, spec map[string]any) (string, error) {
	data, err := yaml.Marshal(spec)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(hash[:8]), nil
}

func nameSuffix(serial string) string {
	hash := sha256.Sum256([]byte(serial))
	return "dpu-" + hex.EncodeToString(hash[:8])
}

func selectorsOverlap(a, b map[string]string) bool {
	for key, value := range a {
		if other, exists := b[key]; exists && other != value {
			return false
		}
	}
	return true
}

func actionObject(action hbn.NodeEffect) map[string]any {
	return map[string]any{string(action): true}
}
