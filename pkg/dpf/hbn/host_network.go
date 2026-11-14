// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbn

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

const maxHostReservationPrefixes = 4096

// AddressRange is an inclusive address range used by DPUServiceIPAM. Host
// reservations use singleton ranges so NVIPAM excludes one CNI address without
// excluding the DPU's whole per-node prefix.
type AddressRange struct {
	StartIP string `json:"startIP" yaml:"startIP"`
	EndIP   string `json:"endIP" yaml:"endIP"`
}

// HostReservations returns one reserved host address for every possible
// workload allocation prefix. The address index is counted from each prefix's
// network address, matching NVIPAM's gatewayIndex convention.
func HostReservations(profile Profile, generation Generation) ([]AddressRange, error) {
	if err := ValidateGeneration(profile, generation); err != nil {
		return nil, err
	}
	workload, count, blockSize, err := workloadAddressPlan(profile, generation)
	if err != nil {
		return nil, err
	}

	base := binary.BigEndian.Uint32(workload.Addr().AsSlice())
	reservations := make([]AddressRange, 0, count)
	for i := uint64(0); i < count; i++ {
		blockBase := base + uint32(i*blockSize)
		address := blockBase + uint32(generation.HostAddressIndex)
		reserved := netip.AddrFrom4([4]byte{
			byte(address >> 24), byte(address >> 16), byte(address >> 8), byte(address),
		})
		if !workload.Contains(reserved) {
			return nil, fmt.Errorf("host reservation %q is outside workloadCIDR %q", reserved, profile.WorkloadCIDR)
		}
		value := reserved.String()
		reservations = append(reservations, AddressRange{StartIP: value, EndIP: value})
	}
	return reservations, nil
}

func workloadAddressPlan(profile Profile, generation Generation) (netip.Prefix, uint64, uint64, error) {
	workload, _ := netip.ParsePrefix(profile.WorkloadCIDR)
	if profile.WorkloadPrefix > 30 {
		return netip.Prefix{}, 0, 0, fmt.Errorf("workloadPrefix /%d must be /30 or broader to leave usable host addresses", profile.WorkloadPrefix)
	}
	if profile.WorkloadPrefix < workload.Bits() {
		return netip.Prefix{}, 0, 0, fmt.Errorf("workloadPrefix /%d cannot be broader than workloadCIDR %q", profile.WorkloadPrefix, profile.WorkloadCIDR)
	}
	allocationSize := uint64(1) << uint(32-profile.WorkloadPrefix)
	if uint64(generation.HostAddressIndex) >= allocationSize-1 {
		return netip.Prefix{}, 0, 0, fmt.Errorf("hostAddressIndex must fit within a workload allocation of prefix /%d", profile.WorkloadPrefix)
	}
	if generation.HostAddressIndex == generation.WorkloadGatewayIndex {
		return netip.Prefix{}, 0, 0, fmt.Errorf("hostAddressIndex must differ from workloadGatewayIndex")
	}
	count := uint64(1) << uint(profile.WorkloadPrefix-workload.Bits())
	if count > maxHostReservationPrefixes {
		return netip.Prefix{}, 0, 0, fmt.Errorf("workloadCIDR %q with workloadPrefix /%d requires %d host exclusions; maximum is %d", profile.WorkloadCIDR, profile.WorkloadPrefix, count, maxHostReservationPrefixes)
	}
	return workload, count, uint64(1) << uint(32-profile.WorkloadPrefix), nil
}
