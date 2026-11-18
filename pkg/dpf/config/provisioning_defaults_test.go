// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
)

func TestResolveProvisioningDefaultsRequiresReleaseAndUseCase(t *testing.T) {
	if _, _, err := ResolveProvisioningDefaults(Site{}, nil); err == nil || !strings.Contains(err.Error(), "--use-case") {
		t.Fatalf("ResolveProvisioningDefaults() error = %v, want use-case error", err)
	}
}

func TestResolveProvisioningDefaultsRejectsLegacyUserOwnedInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Provisioning)
		want string
	}{
		{name: "flavor", edit: func(p *Provisioning) { p.Flavor = map[string]any{"configFiles": []any{"site value"}} }, want: "/provisioning/flavor"},
		{name: "software", edit: func(p *Provisioning) { p.Software.OSISO.URL = "https://artifacts.example/os.iso" }, want: "/provisioning/software"},
	} {
		t.Run(test.name, func(t *testing.T) {
			site := hbnDefaultsSite()
			site.Provisioning = &Provisioning{}
			test.edit(site.Provisioning)
			if _, _, err := ResolveProvisioningDefaults(site, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResolveProvisioningDefaults() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveProvisioningDefaultsRejectsUnlistedBF4ModelAndPreservesSiteSettings(t *testing.T) {
	site := hbnDefaultsSite()
	reset := "Never"
	resource := map[string]string{"memory": "8Gi"}
	hbnOverride := "https://charts.example.com/hbn"
	site.Provisioning = &Provisioning{
		Resources: resource,
		Bootstrap: &BootstrapOptions{OperatorConfigName: &reset},
		HBN:       &hbn.GenerationOptions{ChartRepository: &hbnOverride},
	}
	resolved, issues, err := ResolveProvisioningDefaults(site, []ProvisioningDevice{{Model: "B4241", PSID: "MT_0000001774"}})
	if err != nil {
		t.Fatalf("ResolveProvisioningDefaults() error = %v", err)
	}
	if resolved == nil || len(issues) == 0 {
		t.Fatalf("resolved=%#v issues=%#v, want provisioning plus catalog blocker", resolved, issues)
	}
	if issues[0].ReasonCode != "DPF_MODEL_NOT_CATALOGED" || !strings.Contains(issues[0].Message, `model "B4241"`) {
		t.Fatalf("first issue = %#v, want exact-model blocker", issues[0])
	}
	if resolved.Flavor != nil || softwareSupplied(resolved.Software) {
		t.Fatalf("resolver synthesized unmaintained flavor/software: %#v", resolved)
	}
	if resolved.Resources["memory"] != "8Gi" || resolved.HBN == nil || *resolved.HBN.ChartRepository != hbnOverride || resolved.Bootstrap == nil || *resolved.Bootstrap.OperatorConfigName != reset {
		t.Fatalf("site-owned settings not preserved: %#v", resolved)
	}
	resolved.Resources["memory"] = "9Gi"
	*resolved.HBN.ChartRepository = "changed"
	*resolved.Bootstrap.OperatorConfigName = "changed"
	if resource["memory"] != "8Gi" || hbnOverride != "https://charts.example.com/hbn" || reset != "Never" {
		t.Fatalf("resolver retained caller-owned mutable values: resources=%#v hbn=%q bootstrap=%q", resource, hbnOverride, reset)
	}
}

func TestResolveProvisioningDefaultsRequiresObservedModelAndPSID(t *testing.T) {
	site := hbnDefaultsSite()
	_, issues, err := ResolveProvisioningDefaults(site, nil)
	if err != nil {
		t.Fatalf("ResolveProvisioningDefaults() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ReasonCode != "DPF_DEVICE_IDENTITY_REQUIRED" || issues[0].Message == "" {
		t.Fatalf("issues = %#v, want actionable identity blocker", issues)
	}

}

func TestResolveProvisioningDefaultsUsesMaintainedB4240Recipe(t *testing.T) {
	mtu := 9000
	site := hbnDefaultsSite()
	site.DPF.HBN = &hbn.Options{UplinkMTU: &mtu}
	resolved, issues, err := ResolveProvisioningDefaults(site, []ProvisioningDevice{{Model: "B4240", PSID: "MT_0000001774"}})
	if err != nil || len(issues) != 0 {
		t.Fatalf("resolveProvisioningDefaults() = resolved %#v, issues %#v, error %v", resolved, issues, err)
	}
	if resolved.Software.OSISO.SHA256 != "bc69d99904300e09483f9fb9cb5187562fa770308acb4f6f139717b2105b3272" || resolved.Software.PLDM["MT_0000001774"].SHA256 != "ac3be7fc6e84ad678325834e42fdc34392e7ab7dcccea2346fd58fe851c9122a" {
		t.Fatalf("resolved software = %#v", resolved.Software)
	}
	ovs, ok := resolved.Flavor["ovs"].(map[string]any)
	if !ok {
		t.Fatalf("resolved flavor has no OVS spec: %#v", resolved.Flavor)
	}
	script, ok := ovs["rawConfigScript"].(string)
	if !ok || !strings.Contains(script, "mtu_request=9000") || strings.Contains(script, "__L8K_UPLINK_MTU__") {
		t.Fatalf("resolved OVS script did not render profile MTU: %#v", ovs["rawConfigScript"])
	}
}

func TestResolveProvisioningDefaultsRejectsUnlistedBF4PSID(t *testing.T) {
	_, issues, err := ResolveProvisioningDefaults(hbnDefaultsSite(), []ProvisioningDevice{{Model: "B4240", PSID: "MT_0000001775"}})
	if err != nil {
		t.Fatalf("ResolveProvisioningDefaults() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ReasonCode != "DPF_PSID_ARTIFACT_NOT_CATALOGED" {
		t.Fatalf("issues = %#v, want exact PSID blocker", issues)
	}
}

func TestProvisioningDefaultsRejectsCaseInsensitiveDuplicateCatalogPSIDs(t *testing.T) {
	release := releases.Release{Version: "test-release", Hardware: []releases.HardwareBOM{{
		Family: "BlueField-4", Models: []string{"B4-Test"},
		BlueFieldSoftware: &releases.BlueFieldSoftware{
			OSISO: releases.Artifact{URL: "https://artifacts.example/os.iso", Digest: strings.Repeat("a", 64)},
			PLDMFwBundle: map[string]releases.Artifact{
				"MT_TEST1774": {URL: "https://artifacts.example/fw-a.pkg", Digest: strings.Repeat("b", 64)},
				"mt_test1774": {URL: "https://artifacts.example/fw-b.pkg", Digest: strings.Repeat("c", 64)},
			},
		},
	}}}
	_, issue := softwareForDevice(release, "B4-Test", "MT_TEST1774", *release.Hardware[0].BlueFieldSoftware)
	if issue == nil || issue.ReasonCode != "DPF_PSID_ARTIFACT_AMBIGUOUS" {
		t.Fatalf("softwareForDevice() issue = %#v, want duplicate PSID blocker", issue)
	}
}

func hbnDefaultsSite() Site {
	useCase, release := releases.UseCaseHBNBGP, "26.8.0"
	var site Site
	site.DPF.UseCase = &useCase
	site.DPF.Release = &release
	return site
}
