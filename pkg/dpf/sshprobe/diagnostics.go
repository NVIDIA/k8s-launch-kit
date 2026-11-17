// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"golang.org/x/crypto/ssh"
)

// commandFailure classifies bounded output, but never returns it. Tool diagnostics
// can include paths and credentials; only static explanations reach artifacts.
func commandFailure(err error, stage, command, output string) *Error {
	code := "SSH_COMMAND_FAILED"
	var exitStatus *int
	var status *ssh.ExitError
	if errors.As(err, &status) {
		exit := status.ExitStatus()
		if exit >= 0 && exit <= 255 {
			exitStatus = &exit
		}
		switch exit {
		case 126:
			code = "TOOL_NOT_EXECUTABLE"
		case 127:
			code = "TOOL_NOT_AVAILABLE"
		case 66:
			if stage == "dnsmasq" {
				code = "PROPERTY_NOT_EXPOSED"
			}
		case 77:
			if stage == "dnsmasq" {
				code = "PERMISSION_DENIED"
			}
		}
	}
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "command not found"), strings.Contains(lower, "not found in path"):
		code = "TOOL_NOT_AVAILABLE"
	case strings.Contains(lower, "permission denied"), strings.Contains(lower, "access denied"), strings.Contains(lower, "operation not permitted"), strings.Contains(lower, "a password is required"), strings.Contains(lower, "not allowed to execute"):
		code = "PERMISSION_DENIED"
	case strings.Contains(lower, "failed to open device"), strings.Contains(lower, "cannot open device"):
		code = "DEVICE_ACCESS_FAILED"
	case strings.Contains(lower, "not supported"), strings.Contains(lower, "unsupported"), strings.Contains(lower, "unknown parameter"), strings.Contains(lower, "unrecognized option"):
		code = "OPERATION_UNSUPPORTED"
	}
	tool := commandTool(command)
	if strings.HasPrefix(command, "sudo -n ") && (strings.Contains(lower, "sudo: not found") || strings.Contains(lower, "sudo: command not found")) {
		tool = "sudo"
	}
	return &Error{Code: code, Stage: stage, Tool: tool, ExitStatus: exitStatus}
}

func commandTool(command string) string {
	f := strings.Fields(command)
	if len(f) >= 3 && f[0] == "sudo" && f[1] == "-n" {
		f = f[2:]
	}
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Reason returns a safe explanation without library or remote error text.
func Reason(err error) string {
	code := Code(err)
	if reason, ok := hostNetworkReadReason(code); ok {
		return reason
	}
	if strings.HasPrefix(code, "HOST_NETWORK_TOOL_MISSING_") {
		return "A required host networking tool is not installed; the reason code identifies the missing tool."
	}
	switch code {
	case "HOST_NETWORK_DEADLINE_REQUIRED":
		return "The host networking observer requires a caller context with a deadline; no SSH read was started."
	case "HOST_NETWORK_TOOL_MISSING":
		return "A required host networking tool is not available in the remote command environment."
	case "HOST_NETWORK_BACKEND_UNSUPPORTED":
		return "The observed host OS, network manager or tool versions are outside the supported host networking combination."
	case "HOST_NETWORK_NETPLAN_UNSUPPORTED":
		return "The selected interface's Netplan definition has unsupported or conflicting settings."
	case "HOST_NETWORK_MTU_CAPABILITY_UNAVAILABLE":
		return "The kernel did not report usable minimum and maximum MTUs for the selected interface."
	case "HOST_NETWORK_MTU_UNSUPPORTED":
		return fmt.Sprintf("The current or desired workload MTU is outside the supported kernel or host-backend limits; this backend requires at least %d bytes.", MinimumHostWorkloadMTU)
	case "HOST_NETWORK_BACKEND_OWNERSHIP_UNAVAILABLE":
		return "The active interface is not managed by its matching generated Netplan networkd file."
	case "HOST_NETWORK_MANAGEMENT_PATH_CONFLICT":
		return "An active workload interface and a separate management path without a selected-interface default route could not be established."
	case "HOST_NETWORK_IDENTITY_CHANGED", "HOST_NETWORK_IDENTITY_INVALID":
		return "The observed host, card, PCI function, physical port or permanent MAC does not match the selected identity."
	case "HOST_NETWORK_ROOT_REQUIRED":
		return "The host networking backend requires root access through the configured account or sudo."
	case "HOST_NETWORK_ROUTING_POLICY_UNSUPPORTED":
		return "The host has policy-routing rules outside the supported workload route configuration."
	case "HOST_NETWORK_OWNERSHIP_CONFLICT":
		return "The host-networking intent or fragment could not be verified against its expected presence, protected ownership and exact bytes; resolve the ownership conflict before retrying."
	case "HOST_NETWORK_SOURCE_CHANGED":
		return "The protected Netplan source or bound host-network context changed while the merged configuration was being read or used; retry after the source and bound context are stable."
	case "HOST_NETWORK_SOURCE_INVALID":
		return "The host's Netplan source is missing, unsafe, inconsistent or exceeds the supported size."
	case "HOST_NETWORK_ACCOUNT_UNAVAILABLE":
		return "The host's required root or systemd-network account group could not be established."
	case "HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED":
		return "The complete interface matching context could not be represented safely for isolated Netplan generation."
	case "HOST_NETWORK_GENERATOR_CONTEXT_CHANGED":
		return "The interface set, current MAC addresses or driver bindings changed during the host networking operation."
	case "HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED":
		return "The Netplan wait-online unit could not be read safely or has unsafe ancestry, unsupported bytes or command structure."
	case "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED":
		return "The selected wait-online contribution cannot be established from its current MAC and Netplan matching context."
	case "HOST_NETWORK_WAIT_ONLINE_CONTRIBUTION_INVALID":
		return "The generated wait-online unit does not contain exactly the selected contribution required by its Netplan optional and matching state."
	case "HOST_NETWORK_SHADOW_UNAVAILABLE":
		return "An isolated preview of Netplan's generated files could not be completed; no host network change was authorized."
	case "HOST_NETWORK_GENERATED_METADATA_UNSUPPORTED":
		return "Netplan's generated file ownership or permissions cannot be preserved or do not match the supported backend."
	case "HOST_NETWORK_GENERATED_OUTPUT_CONFLICT":
		return "Netplan generation would change files outside the selected workload interface; resolve the pending configuration before deployment."
	case "TOOL_NOT_AVAILABLE":
		return "Required tool is not available in the remote command environment."
	case "TOOL_NOT_EXECUTABLE":
		return "The remote tool exists but could not be executed."
	case "PERMISSION_DENIED":
		return "The configured account or noninteractive sudo policy does not permit this read."
	case "DEVICE_ACCESS_FAILED":
		return "The tool could not open the queried device; device access was not established."
	case "OPERATION_UNSUPPORTED":
		return "The installed tool or device does not support this read operation."
	case "SSH_TIMEOUT_OR_CANCELLED":
		return "The observation timed out or was cancelled."
	case "SSH_OUTPUT_LIMIT":
		return "The remote response exceeded the bounded output limit."
	case "SSH_RUNTIME_INVALID":
		return "The command returned missing, malformed or ambiguous observation data."
	case "PROPERTY_NOT_EXPOSED":
		return "The requested property is not exposed in the command response."
	case "NOT_APPLICABLE":
		return "This observation does not apply to this device or collection vantage."
	case "SSH_HOST_KEY_REJECTED":
		return "The remote SSH host key did not match the configured trust."
	case "SSH_CONNECT_FAILED":
		return "The configured SSH endpoint could not be reached."
	case "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED":
		return "SSH authentication or session negotiation failed."
	default:
		return "The requested read failed; no reliable observation was obtained."
	}
}

func unavailable(err error, tool string) inventory.Observation {
	var e *Error
	if errors.As(err, &e) && e.Tool != "" {
		tool = e.Tool
	}
	return inventory.Observation{State: "unavailable", Values: []string{}, ReasonCode: Code(err), Message: Reason(err), Tool: tool}
}

func missing(tool string) inventory.Observation {
	return unavailable(&Error{Code: "PROPERTY_NOT_EXPOSED"}, tool)
}

func observed(tool string, values ...string) inventory.Observation {
	return inventory.Observation{State: "observed", Values: values, Tool: tool}
}

// Backend read reasons contain only a fixed operation/tool pair and a bounded
// exit status. The allowlist is also used at the protocol boundary, so remote
// identifiers cannot turn into arbitrary diagnostic text.
func validHostNetworkReadReason(code string) bool {
	_, ok := hostNetworkReadReason(code)
	return ok
}

func hostNetworkReadReason(code string) (string, bool) {
	const prefix = "HOST_NETWORK_READ_"
	if !strings.HasPrefix(code, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(code, prefix)
	switch value {
	case "PCI_BINDING_CHANGED":
		return "The selected interface's PCI binding changed during the read.", true
	case "MANAGEMENT_DEVICE_INVALID":
		return "The SSH management route did not identify a separate usable management interface.", true
	case "NETWORK_FILE_MISSING":
		return "The selected interface has no active networkd network file; configure its supported persistent Netplan definition before using this backend.", true
	case "NETWORKMANAGER_ABSENCE_UNCONFIRMED":
		return "NetworkManager returned an absent-unit status without confirming a not-found, inactive and dead unit; absence is not established.", true
	case "NETWORK_FILE_INVALID":
		return "The active networkd file is missing, is not a regular file, or is a symbolic link.", true
	case "NETWORK_FILE_METADATA_INVALID":
		return "The active networkd file does not have protected root ownership and permissions.", true
	case "NETWORKMANAGER_STATE_INVALID":
		return "NetworkManager returned a successful status without reporting an active state.", true
	}
	operation, exitText, found := strings.Cut(value, "_EXIT_")
	if !found {
		return "", false
	}
	exit, err := strconv.Atoi(exitText)
	if err != nil || exit < 0 || exit > 255 || strconv.Itoa(exit) != exitText {
		return "", false
	}
	var read string
	switch operation {
	case "IDENTITY_DMIDECODE":
		read = "host identity with dmidecode"
	case "BOOT_ID_CAT":
		read = "boot identity with cat"
	case "PCI_VPD_LSPCI":
		read = "PCI VPD with lspci"
	case "PCI_VPD_BASE64":
		read = "PCI VPD encoding with base64"
	case "PCI_BINDING_READLINK", "PCI_BINDING_RECHECK_READLINK":
		read = "PCI binding with readlink"
	case "PHYSICAL_PORT_CAT":
		read = "physical port with cat"
	case "PERMANENT_MAC_ETHTOOL":
		read = "permanent MAC with ethtool"
	case "LINK_IP":
		read = "link state with ip"
	case "ADDRESSES_IP":
		read = "IPv4 addresses with ip"
	case "ROUTES_IP":
		read = "IPv4 routes with ip"
	case "RUNTIME_DIGEST_JQ":
		read = "runtime digest JSON with jq"
	case "RUNTIME_DIGEST_SHA256SUM":
		read = "runtime digest with sha256sum"
	case "RULES4_IP":
		read = "IPv4 policy rules with ip"
	case "RULES6_IP":
		read = "IPv6 policy rules with ip"
	case "DEFAULTS4_IP":
		read = "IPv4 default routes with ip"
	case "DEFAULTS6_IP":
		read = "IPv6 default routes with ip"
	case "MANAGEMENT_ROUTE_IP":
		read = "SSH management route with ip"
	case "MANAGEMENT_ROUTE_JQ":
		read = "SSH management route JSON with jq"
	case "MANAGEMENT_DEVICE_JQ":
		read = "SSH management interface with jq"
	case "MANAGEMENT_ADDRESSES_IP":
		read = "SSH management addresses with ip"
	case "MANAGEMENT_ADDRESSES_JQ":
		read = "SSH management address JSON with jq"
	case "NETWORK_FILE_NETWORKCTL":
		read = "active networkd file with networkctl"
	case "NETWORK_FILE_AWK":
		read = "active networkd file selection with awk"
	case "NETWORK_FILE_METADATA_STAT":
		read = "active networkd file metadata with stat"
	case "MANAGEMENT_ADDRESS_JQ":
		read = "SSH local address verification with jq"
	case "NETPLAN_MERGED_NETPLAN":
		read = "merged Netplan configuration with netplan"
	case "NETPLAN_MERGED_BASE64":
		read = "merged Netplan encoding with base64"
	case "NETPLAN_SOURCE_SOURCE_DIGEST":
		read = "protected Netplan source digest"
	case "OS_RELEASE_AWK":
		read = "OS release with awk"
	case "NETPLAN_VERSION_DPKG_QUERY":
		read = "Netplan version with dpkg-query"
	case "NETWORKD_VERSION_NETWORKCTL":
		read = "networkd version with networkctl"
	case "IP_VERSION_IP":
		read = "iproute2 version with ip"
	case "NETWORKD_STATE_SYSTEMCTL":
		read = "networkd state with systemctl"
	case "NETWORKMANAGER_PROPERTIES_SYSTEMCTL":
		read = "NetworkManager unit properties with systemctl"
	case "NETWORKMANAGER_STATE_SYSTEMCTL":
		read = "NetworkManager state with systemctl"
	case "GENERATOR_CONTEXT_BASH":
		read = "interface MAC matching facts from sysfs with Bash read"
	case "GENERATOR_CONTEXT_READLINK":
		read = "interface driver matching facts with readlink"
	case "GENERATOR_CONTEXT_SHA256SUM":
		read = "interface matching context digest with sha256sum"
	case "FENCE_JQ":
		read = "observation fence JSON with jq"
	case "FENCE_SHA256SUM":
		read = "observation fence with sha256sum"
	case "SNAPSHOT_JSON_JQ":
		read = "snapshot JSON with jq"
	default:
		return "", false
	}
	return fmt.Sprintf("The host networking read failed while checking %s (exit %d); no reliable observation was obtained.", read, exit), true
}
