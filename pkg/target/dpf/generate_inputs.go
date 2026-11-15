// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type generationPlan struct {
	generation.Inputs
	OperatorChartURL string
}

// generationInputs resolves desired artifacts from saved observations, without
// opening any access references. Deployment must refresh identity and readiness.
func generationInputs(input config.Input) (generationPlan, error) {
	intent, err := config.ResolveIntent(input.Site)
	if err != nil {
		return generationPlan{}, err
	}
	if intent.UseCase != "hbn-bgp" || intent.Networking == nil || intent.Generation == nil {
		return generationPlan{}, fmt.Errorf("DPF generation requires dpf.useCase: hbn-bgp")
	}
	inv := input.Site.Inventory
	if inv == nil || inv.FormatVersion != inventory.FormatVersion || inv.Management == nil || inv.HBN == nil || len(inv.DPUs) == 0 {
		return generationPlan{}, fmt.Errorf("DPF generation requires a current-format HBN discovery inventory with management and selected DPU identities")
	}
	if inv.Software == nil || inv.Software.Version != intent.Release.Version || inv.HBN.Namespace != intent.Namespace {
		return generationPlan{}, fmt.Errorf("selected DPF release or namespace differs from discovery; run discovery with the intended configuration")
	}
	if inv.Management.SystemNamespaceUID == "" || inv.Management.TrustDigest == "" {
		return generationPlan{}, fmt.Errorf("management identity or trust are missing; rerun discovery")
	}
	if input.Site.Provisioning == nil {
		return generationPlan{}, fmt.Errorf("provisioning inputs are required")
	}
	provisioning := input.Site.Provisioning
	if len(provisioning.Flavor) == 0 {
		return generationPlan{}, fmt.Errorf("provisioning.flavor requires the DPUFlavor spec for the selected hardware")
	}
	resources, err := generationResources(provisioning.Resources)
	if err != nil {
		return generationPlan{}, err
	}
	devices, selected, psids, err := generationDevices(input.Site, inv)
	if err != nil {
		return generationPlan{}, err
	}
	software, issues, err := config.ValidateProvisioningSoftware(provisioning, psids)
	if err != nil {
		return generationPlan{}, err
	}
	if len(issues) > 0 {
		return generationPlan{}, fmt.Errorf("required provisioning input missing: %s", issues[0].Field)
	}
	bootstrapInputs, issues, err := config.ResolveBootstrap(provisioning.Bootstrap, intent.Namespace, inv.Management.APIEndpoint, selected)
	if err != nil {
		return generationPlan{}, err
	}
	if len(issues) > 0 {
		return generationPlan{}, fmt.Errorf("required bootstrap input missing: %s", issues[0].Field)
	}
	serials := make([]string, len(devices))
	for i := range devices {
		serials[i] = devices[i].Serial
	}
	asns, err := intent.Networking.AllocateASNs(serials)
	if err != nil {
		return generationPlan{}, err
	}
	for i := range devices {
		devices[i].ASN = asns[devices[i].Serial]
	}
	return generationPlan{Inputs: generation.Inputs{
		Operator: bootstrap.OperatorRenderInputs{ReleaseVersion: intent.Release.Version, ReleaseName: intent.Release.Operator.Chart,
			KubernetesVersion: inv.Management.KubernetesVersion, Bootstrap: bootstrapInputs},
		Workload: hbnrender.Inputs{Namespace: intent.Namespace, Generation: *intent.Generation, Profile: *intent.Networking,
			Software: software, Devices: devices, DPUClusterSelector: maps.Clone(bootstrapInputs.Tenant.Labels), FlavorSpec: provisioning.Flavor, HBNResources: resources},
		Management:   generation.ManagementIdentity{APIEndpoint: inv.Management.APIEndpoint, SystemNamespaceUID: inv.Management.SystemNamespaceUID, TrustDigest: inv.Management.TrustDigest},
		SourceConfig: slices.Clone(input.Source),
	}, OperatorChartURL: strings.TrimSuffix(intent.Release.Operator.ChartRepository, "/") + "/charts/" + intent.Release.Operator.Chart + "-" + intent.Release.Operator.Version + ".tgz"}, nil
}

// generationDevices checks the complete selected source set, then reuses the
// discovery association/PSID contract. Labels are desired enrollment bindings;
// deployment must verify identity before attaching them to any DPUDevice.
func generationDevices(site config.Site, inv *inventory.Inventory) ([]hbnrender.Device, []bootstrap.Device, []string, error) {
	if err := validateGenerationSources("hosts", site.Discovery.Hosts, inv.Hosts); err != nil {
		return nil, nil, nil, err
	}
	if err := validateGenerationSources("dpus", site.Discovery.DPUs, inv.DPUs); err != nil {
		return nil, nil, nil, err
	}
	devices, selected, psids := []hbnrender.Device{}, []bootstrap.Device{}, []string{}
	for _, dpu := range inv.DPUs {
		admin := dpu.Observations["bmc-administrator"]
		if admin.State != "observed" || admin.ReasonCode != "BMC_ADMIN_VERIFIED" || dpu.BMCEndpoint == "" || dpu.SSHEndpoint != "" {
			return nil, nil, nil, fmt.Errorf("DPU %q does not have verified BMC Administrator access", dpu.SerialNumber)
		}
		psid, err := discovery.ObservedDevicePSID(inv, dpu)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DPU %q: %w", dpu.SerialNumber, err)
		}
		endpoint, err := config.Origin(dpu.BMCEndpoint)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DPU %q requires a canonical BMC endpoint", dpu.SerialNumber)
		}
		bmc, _ := url.Parse(endpoint)
		port := 443
		if bmc.Port() != "" {
			port, err = strconv.Atoi(bmc.Port())
			if err != nil {
				return nil, nil, nil, fmt.Errorf("DPU BMC port is invalid")
			}
		}
		label := sha256.Sum256([]byte(intentDeviceLabelDomain + dpu.SerialNumber))
		devices = append(devices, hbnrender.Device{Serial: dpu.SerialNumber, PSID: psid, SelectorLabels: map[string]string{"launch-kit.nvidia.com/dpu": fmt.Sprintf("%x", label[:16])}})
		selected = append(selected, bootstrap.Device{Serial: dpu.SerialNumber, BMCIP: bmc.Hostname(), BMCPort: port})
		psids = append(psids, psid)
	}
	return devices, selected, psids, nil
}

// Source matching covers all recorded hosts as well as DPUs. A changed host
// endpoint or explicit Redfish system must not reuse another source's PSID.
func validateGenerationSources(role string, sources []config.Endpoint, observed []inventory.Device) error {
	if len(sources) != len(observed) {
		return fmt.Errorf("%s selection changed since discovery; rerun discovery", role)
	}
	byRef := map[string]inventory.Device{}
	for _, device := range observed {
		if _, exists := byRef[device.InputRef]; exists {
			return fmt.Errorf("%s source identity is ambiguous", role)
		}
		byRef[device.InputRef] = device
	}
	for i, source := range sources {
		device, ok := byRef[fmt.Sprintf("/discovery/%s/%d", role, i)]
		if !ok {
			return fmt.Errorf("%s source identity is missing; rerun discovery", role)
		}
		if source.SSH != nil {
			endpoint, err := config.SSHOrigin(source.SSH.Address)
			if err != nil || source.BMCEndpoint != "" || endpoint != device.SSHEndpoint || source.SystemURI != nil {
				return fmt.Errorf("%s SSH source changed or is invalid; rerun discovery", role)
			}
		} else {
			endpoint, err := config.Origin(source.BMCEndpoint)
			if err != nil || endpoint != device.BMCEndpoint {
				return fmt.Errorf("%s BMC source changed or is invalid; rerun discovery", role)
			}
			if source.SystemURI != nil {
				if config.ResourceURI(*source.SystemURI) != nil || *source.SystemURI != device.SystemURI {
					return fmt.Errorf("%s Redfish system selection changed; rerun discovery", role)
				}
			}
		}
	}
	return nil
}

const intentDeviceLabelDomain = "dpf-hbn-device-v1:"

func generationResources(resources map[string]string) (map[string]any, error) {
	resolved, err := config.ResolveHBNResources(resources)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any, len(resolved))
	for name, quantity := range resolved {
		result[name] = quantity
	}
	return result, nil
}
