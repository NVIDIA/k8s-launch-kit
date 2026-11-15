// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"encoding/json"
	"net"
	"strings"
)

// LLDPLocalIdentity is the switch's own advertised identity, read through the
// authenticated switch session. It binds a host's LLDP observation to that
// selected switch; the configuration's display name is not an identity.
type LLDPLocalIdentity struct {
	ChassisID  string `json:"chassisId" yaml:"chassisId"`
	SystemName string `json:"systemName" yaml:"systemName"`
}

func parseLocalLLDP(raw []byte, out *Snapshot) {
	const input = "localLLDP"
	var response struct {
		Local []struct {
			Chassis []struct {
				ID []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"id"`
				Name []struct {
					Value string `json:"value"`
				} `json:"name"`
			} `json:"chassis"`
		} `json:"local-chassis"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Local) != 1 || len(response.Local[0].Chassis) != 1 {
		addIssue(&out.Issues, input, "$", "local_identity_unavailable", "Expected exactly one local LLDP chassis from the selected switch.")
		return
	}
	chassis := response.Local[0].Chassis[0]
	if len(chassis.ID) != 1 || chassis.ID[0].Type != "mac" || len(chassis.Name) != 1 {
		addIssue(&out.Issues, input, "$.local-chassis", "local_identity_unsupported", "Local LLDP identity requires one MAC chassis identifier and one system name.")
		return
	}
	mac, err := net.ParseMAC(chassis.ID[0].Value)
	name := chassis.Name[0].Value
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" || strings.TrimSpace(name) != name || name == "" || len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") {
		addIssue(&out.Issues, input, "$.local-chassis", "local_identity_invalid", "Local LLDP chassis MAC or system name is invalid.")
		return
	}
	out.Operational.LocalLLDP = &LLDPLocalIdentity{ChassisID: mac.String(), SystemName: name}
}
