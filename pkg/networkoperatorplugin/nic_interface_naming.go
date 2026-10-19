// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/internal/pfutil"
	"sigs.k8s.io/yaml"
)

// standardNamingPFs keeps the naming rail indices aligned with policy names.
// FilterEastWestPFs returns a copy, so sorting never changes the inventory.
func standardNamingPFs(pfs []config.PFConfig) []config.PFConfig {
	ew := pfutil.FilterEastWestPFs(pfs)
	if allHaveRail(ew) {
		slices.SortStableFunc(ew, func(a, b config.PFConfig) int { return cmp.Compare(*a.Rail, *b.Rail) })
	}
	return ew
}

// validateInterfaceNamingLayout checks the emitted mapping, rather than the
// inventory's Rail fields: standard templates place each PF in its own rail.
// NCO assigns one rail index to a physical NIC and applies it to all its PFs.
func validateInterfaceNamingLayout(group config.ClusterConfig, rendered map[string]string) error {
	filenames := make([]string, 0, len(rendered))
	for filename := range rendered {
		filenames = append(filenames, filename)
	}
	slices.Sort(filenames)
	for _, filename := range filenames {
		var naming nicop.NicInterfaceNameTemplate
		if err := yaml.Unmarshal([]byte(rendered[filename]), &naming); err != nil {
			return fmt.Errorf("read interface naming layout in %s: %w", filename, err)
		}
		nicRails := map[string]int{}
		for rail, addresses := range naming.Spec.RailPciAddresses {
			for _, address := range addresses {
				nic, ok := pfutil.PciBusDevicePrefix(strings.ToLower(strings.TrimSpace(address)))
				if !ok {
					return fmt.Errorf("interface naming for %s has invalid PCI address %q", templateGroupLabel(group), address)
				}
				if previous, exists := nicRails[nic]; exists && previous != rail {
					return apperrors.NewValidationError(
						fmt.Sprintf("cannot generate NicInterfaceNameTemplate for %s: physical NIC %q appears in multiple rails (%d and %d)", templateGroupLabel(group), nic, previous, rail),
						nil,
						"NCO assigns one rail index per physical NIC. Use a supported one-rail-per-NIC layout, or explicitly disable interface naming when PCI selectors are sufficient.",
					)
				}
				nicRails[nic] = rail
				for _, pf := range group.PFs {
					if pf.Traffic == "east-west" && strings.EqualFold(strings.TrimSpace(pf.PciAddress), strings.TrimSpace(address)) && pf.Rail != nil && *pf.Rail != rail {
						return apperrors.NewValidationError(
							fmt.Sprintf("cannot generate NicInterfaceNameTemplate for %s: PCI address %q has inventory rail %d but naming rail %d", templateGroupLabel(group), address, *pf.Rail, rail),
							nil, "Standard naming requires one east-west PF per rail, with rail numbers contiguous from zero.",
						)
					}
				}
			}
		}
	}
	return nil
}
