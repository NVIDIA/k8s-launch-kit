// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"encoding/json"
	"fmt"
	"strings"
)

type devlinkFirmware struct {
	running         string
	stored          string
	psid            string
	runningConflict bool
	storedConflict  bool
	runningErr      error
	storedErr       error
	psidErr         error
	err             error
}

// parseDevlinkInfo parses devlink's structured running, stored and fixed
// firmware facts. It keeps PCI addresses as the only join key; callers must
// match these against the host's already-correlated adapter inventory.
func parseDevlinkInfo(text string) (map[string]devlinkFirmware, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil || root == nil {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}
	}
	infoRaw, ok := root["info"]
	if !ok {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(infoRaw, &info); err != nil || info == nil {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}
	}

	result := map[string]devlinkFirmware{}
	for name, raw := range info {
		if !strings.HasPrefix(name, "pci/") {
			continue
		}
		address := strings.TrimPrefix(name, "pci/")
		if !bdfToken.MatchString(address) {
			continue
		}
		address = strings.ToLower(address)
		if _, duplicate := result[address]; duplicate {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			result[address] = devlinkFirmware{err: &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}}
			continue
		}
		var versions map[string]json.RawMessage
		if rawVersions, exists := entry["versions"]; exists {
			if err := json.Unmarshal(rawVersions, &versions); err != nil || versions == nil {
				result[address] = devlinkFirmware{err: &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}}
				continue
			}
		}
		running, runningConflict, runningErr := devlinkVersion(versions, "running")
		stored, storedConflict, storedErr := devlinkVersion(versions, "stored")
		psid, psidErr := devlinkPSID(versions)
		result[address] = devlinkFirmware{
			running: running, stored: stored, psid: psid, runningConflict: runningConflict, storedConflict: storedConflict,
			runningErr: asDevlinkError(runningErr), storedErr: asDevlinkError(storedErr), psidErr: asDevlinkError(psidErr),
		}
	}
	return result, nil
}

func asDevlinkError(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: "SSH_RUNTIME_INVALID", Stage: "devlink-firmware", Tool: "devlink"}
}

func devlinkVersion(versions map[string]json.RawMessage, category string) (string, bool, error) {
	if versions == nil {
		return "", false, nil
	}
	raw, exists := versions[category]
	if !exists {
		return "", false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", false, fmt.Errorf("invalid devlink version group")
	}
	values := []string{}
	for _, key := range []string{"fw.version", "fw"} {
		if value, exists := fields[key]; exists {
			var text string
			if err := json.Unmarshal(value, &text); err != nil || !versionToken.MatchString(strings.TrimSpace(text)) {
				return "", false, fmt.Errorf("invalid devlink firmware version")
			}
			values = append(values, strings.TrimSpace(text))
		}
	}
	if len(values) == 2 && values[0] != values[1] {
		return "", true, nil
	}
	if len(values) == 0 {
		return "", false, nil
	}
	return values[0], false, nil
}

func devlinkPSID(versions map[string]json.RawMessage) (string, error) {
	if versions == nil {
		return "", nil
	}
	raw, exists := versions["fixed"]
	if !exists {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", fmt.Errorf("invalid devlink fixed version group")
	}
	rawPSID, exists := fields["fw.psid"]
	if !exists {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(rawPSID, &value); err != nil || !versionToken.MatchString(strings.TrimSpace(value)) {
		return "", fmt.Errorf("invalid devlink firmware PSID")
	}
	return strings.TrimSpace(value), nil
}
