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
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
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
	return generationInputsForPlan(input, nil)
}

// generationInputsForPlan uses the retained fixed recipe during deployment,
// while fresh generation resolves the maintained catalog. Current supported
// knobs and allocations always remain inputs to the exact-plan comparison.
func generationInputsForPlan(input config.Input, retained *generation.Plan) (generationPlan, error) {
	if input.External && retained != nil {
		var err error
		input, err = withRetainedProvisioning(input, retained)
		if err != nil {
			return generationPlan{}, err
		}
	}
	intent, err := config.ResolveIntent(input.Site)
	if err != nil {
		return generationPlan{}, err
	}
	if intent.UseCase != "hbn-bgp" || intent.Networking == nil || intent.Generation == nil {
		return generationPlan{}, fmt.Errorf("DPF generation requires --use-case hbn-bgp or a saved HBN-BGP selection")
	}
	if input.External && retained != nil {
		previous := retained.Intent()
		if previous.Release != intent.Release.Version {
			return generationPlan{}, fmt.Errorf("current release differs from the retained plan; regenerate and review the bundle")
		}
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
	if input.External && retained == nil {
		selected := make([]config.ProvisioningDevice, 0, len(inv.DPUs))
		for _, device := range inv.DPUs {
			psid, err := discovery.ObservedDevicePSID(inv, device)
			if err != nil {
				return generationPlan{}, fmt.Errorf("DPU %q: %w", device.SerialNumber, err)
			}
			selected = append(selected, config.ProvisioningDevice{Model: device.Model, PSID: psid})
		}
		provisioning, issues, err := config.ResolveProvisioningDefaults(input.Site, selected)
		if err != nil {
			return generationPlan{}, err
		}
		if len(issues) > 0 {
			return generationPlan{}, fmt.Errorf("release catalog cannot provision the selected hardware: %s (%s): %s", issues[0].Field, issues[0].ReasonCode, issues[0].Message)
		}
		input.Site.Provisioning = provisioning
	}
	if input.Site.Provisioning == nil {
		return generationPlan{}, fmt.Errorf("provisioning inputs are required")
	}
	provisioning := input.Site.Provisioning
	if len(provisioning.Flavor) == 0 {
		return generationPlan{}, fmt.Errorf("maintained provisioning recipe lacks the required DPU flavor for the selected hardware")
	}
	resources, err := generationResources(provisioning.Resources)
	if err != nil {
		return generationPlan{}, err
	}
	resolved, err := generationDevices(input.Site, inv, *intent.Networking)
	if err != nil {
		return generationPlan{}, err
	}
	devices := make([]hbnrender.Device, 0, len(resolved))
	selected := make([]bootstrap.Device, 0, len(resolved))
	psids := make([]string, 0, len(resolved))
	for _, device := range resolved {
		devices = append(devices, hbnrender.Device{Serial: device.Serial, PSID: device.PSID, ASN: device.ASN, SelectorLabels: maps.Clone(device.SelectorLabels)})
		selected = append(selected, bootstrap.Device{Serial: device.Serial, BMCIP: device.BMCIP, BMCPort: device.BMCPort})
		psids = append(psids, device.PSID)
	}
	software, issues, err := config.ValidateProvisioningSoftware(provisioning, psids)
	if err != nil {
		return generationPlan{}, err
	}
	if len(issues) > 0 {
		return generationPlan{}, fmt.Errorf("required provisioning input missing: %s", issues[0].Field)
	}
	bootstrapInputs, issues, err := config.ResolveBootstrap(provisioning.Bootstrap, intent.Namespace, inv.Management.APIEndpoint, selected, intent.Release.TenantServiceCIDR)
	if err != nil {
		return generationPlan{}, err
	}
	if len(issues) > 0 {
		return generationPlan{}, fmt.Errorf("required bootstrap input missing: %s", issues[0].Field)
	}
	source := slices.Clone(input.Source)
	if input.External {
		source, err = config.Saved(input, inv, input.Path)
		if err != nil {
			return generationPlan{}, fmt.Errorf("retain effective discovery inputs: %w", err)
		}
	}
	return generationPlan{Inputs: generation.Inputs{
		Operator: bootstrap.OperatorRenderInputs{ReleaseVersion: intent.Release.Version, ReleaseName: intent.Release.Operator.Chart,
			KubernetesVersion: inv.Management.KubernetesVersion, Bootstrap: bootstrapInputs},
		Workload: hbnrender.Inputs{Namespace: intent.Namespace, Generation: *intent.Generation, Profile: *intent.Networking,
			Software: software, Devices: devices, DPUClusterSelector: maps.Clone(bootstrapInputs.Tenant.Labels), FlavorSpec: provisioning.Flavor, HBNResources: resources},
		Management:   generation.ManagementIdentity{APIEndpoint: inv.Management.APIEndpoint, SystemNamespaceUID: inv.Management.SystemNamespaceUID, TrustDigest: inv.Management.TrustDigest},
		SourceConfig: source,
	}, OperatorChartURL: strings.TrimSuffix(intent.Release.Operator.ChartRepository, "/") + "/charts/" + intent.Release.Operator.Chart + "-" + intent.Release.Operator.Version + ".tgz"}, nil
}

// generationDevices checks the complete selected source set, then reuses the
// discovery association/PSID contract and the resolved profile's ASN allocation.
// Labels are desired enrollment bindings;
// deployment must verify identity before attaching them to any DPUDevice.
func generationDevices(site config.Site, inv *inventory.Inventory, profile hbn.Profile) ([]generation.ResolvedDevice, error) {
	if inv == nil || len(inv.DPUs) == 0 {
		return nil, fmt.Errorf("current selected DPU inventory is required")
	}
	if err := validateGenerationSources("hosts", site.Discovery.Hosts, inv.Hosts); err != nil {
		return nil, err
	}
	if err := validateGenerationSources("dpus", site.Discovery.DPUs, inv.DPUs); err != nil {
		return nil, err
	}
	devices := make([]generation.ResolvedDevice, 0, len(inv.DPUs))
	for _, dpu := range inv.DPUs {
		admin := dpu.Observations["bmc-administrator"]
		if admin.State != "observed" || admin.ReasonCode != "BMC_ADMIN_VERIFIED" || dpu.BMCEndpoint == "" || dpu.SSHEndpoint != "" {
			return nil, fmt.Errorf("DPU %q does not have verified BMC Administrator access", dpu.SerialNumber)
		}
		psid, err := discovery.ObservedDevicePSID(inv, dpu)
		if err != nil {
			return nil, fmt.Errorf("DPU %q: %w", dpu.SerialNumber, err)
		}
		endpoint, err := config.Origin(dpu.BMCEndpoint)
		if err != nil {
			return nil, fmt.Errorf("DPU %q requires a canonical BMC endpoint", dpu.SerialNumber)
		}
		bmc, _ := url.Parse(endpoint)
		port := 443
		if bmc.Port() != "" {
			port, err = strconv.Atoi(bmc.Port())
			if err != nil {
				return nil, fmt.Errorf("DPU BMC port is invalid")
			}
		}
		label := sha256.Sum256([]byte(intentDeviceLabelDomain + dpu.SerialNumber))
		devices = append(devices, generation.ResolvedDevice{Serial: dpu.SerialNumber, PSID: psid, BMCIP: bmc.Hostname(), BMCPort: port, SelectorLabels: map[string]string{"launch-kit.nvidia.com/dpu": fmt.Sprintf("%x", label[:16])}})
	}
	serials := make([]string, len(devices))
	for i := range devices {
		serials[i] = devices[i].Serial
	}
	asns, err := profile.AllocateASNs(serials)
	if err != nil {
		return nil, err
	}
	for i := range devices {
		devices[i].ASN = asns[devices[i].Serial]
	}
	return devices, nil
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
