// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

const (
	defaultRebootPollInterval = 5 * time.Second
	maxExternalRebootWait     = 2 * time.Hour
	maxExternalSSHCommand     = 2 * time.Minute
)

// NewExternalRebooter binds a bounded external host reboot callback to the
// explicitly resolved configuration and inventory. A host reboot is issued
// only for DPF's SystemLevelReset request; PowerCycle needs a separate true
// power-control backend and is rejected.
func NewExternalRebooter(request config.Request, commandTimeout, rebootWait, pollInterval time.Duration) (deployment.ExternalRebooter, error) {
	if commandTimeout < time.Second || commandTimeout > maxExternalSSHCommand {
		return nil, fmt.Errorf("external SSH command timeout must be between one second and two minutes")
	}
	if rebootWait < time.Second || rebootWait > maxExternalRebootWait {
		return nil, fmt.Errorf("external host reboot wait must be between one second and two hours")
	}
	if pollInterval == 0 {
		pollInterval = defaultRebootPollInterval
	}
	if pollInterval < 0 || pollInterval > time.Minute || pollInterval >= rebootWait {
		return nil, fmt.Errorf("external host reboot poll interval is outside supported bounds")
	}
	return &externalRebooter{request: request, commandTimeout: commandTimeout, rebootWait: rebootWait, pollInterval: pollInterval, openSession: sshprobe.OpenExternalRebootSession}, nil
}

type externalSessionOpener func(context.Context, string, *config.SSHAccess, time.Duration) (sshprobe.ExternalRebootSession, error)

type externalRebooter struct {
	request        config.Request
	commandTimeout time.Duration
	rebootWait     time.Duration
	pollInterval   time.Duration
	openSession    externalSessionOpener
}

func (r *externalRebooter) RebootExternal(ctx context.Context, request deployment.ActivationRequest) (deployment.ActivationObservation, error) {
	if ctx == nil {
		return deployment.ActivationObservation{}, fmt.Errorf("external host reboot requires a context")
	}
	if r == nil || r.openSession == nil || r.commandTimeout <= 0 || r.rebootWait <= 0 || r.pollInterval <= 0 {
		return deployment.ActivationObservation{}, fmt.Errorf("external host reboot adapter is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return deployment.ActivationObservation{}, err
	}
	if err := request.VerifyResetSafetyFresh(); err != nil {
		return deployment.ActivationObservation{}, err
	}
	binding, err := resolveRebootBinding(r.request, request)
	if err != nil {
		return deployment.ActivationObservation{}, err
	}
	for _, node := range request.DPUNodes {
		if node.RebootMethod != "SystemLevelReset" {
			if node.RebootMethod == "PowerCycle" {
				return deployment.ActivationObservation{}, fmt.Errorf("DPUNode %q requires PowerCycle; host SSH cannot perform the required DPU power cycle", node.NodeName)
			}
			return deployment.ActivationObservation{}, fmt.Errorf("DPUNode %q has an unsupported external reboot method", node.NodeName)
		}
	}

	operationCtx, cancel := context.WithTimeout(ctx, r.rebootWait)
	defer cancel()
	// A disconnect or timeout can happen after systemd has accepted the reboot.
	// The action is therefore sent once and never retried; only reads reconnect.
	// The pre-action deadline covers dial, authentication, host identity and
	// command submission. Recovery reads retain the independent operation budget.
	actionCtx, actionCancel := context.WithDeadline(operationCtx, request.ResetSafetyExpiresAt)
	defer actionCancel()
	actionSession, err := r.openSession(actionCtx, binding.target.Endpoint, binding.target.SSH, r.commandTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return deployment.ActivationObservation{}, ctx.Err()
		}
		if freshErr := request.VerifyResetSafetyFresh(); freshErr != nil {
			return deployment.ActivationObservation{}, freshErr
		}
		return deployment.ActivationObservation{}, fmt.Errorf("open authenticated host reboot session: %w", err)
	}
	before, err := actionSession.ObserveHost(actionCtx, binding.pci)
	if err != nil {
		_ = actionSession.Close()
		if ctx.Err() != nil {
			return deployment.ActivationObservation{}, ctx.Err()
		}
		if freshErr := request.VerifyResetSafetyFresh(); freshErr != nil {
			return deployment.ActivationObservation{}, freshErr
		}
		return deployment.ActivationObservation{}, fmt.Errorf("verify selected host immediately before reboot: %w", err)
	}
	if !sameUUID(before.SystemUUID, request.HostSystemUUID) || before.AdapterSerial != request.HostAdapterSerial {
		_ = actionSession.Close()
		return deployment.ActivationObservation{}, fmt.Errorf("SSH host or selected adapter identity differs from the activation request")
	}
	if ctx.Err() != nil {
		_ = actionSession.Close()
		return deployment.ActivationObservation{}, ctx.Err()
	}
	if err := request.VerifyResetSafetyFresh(); err != nil {
		_ = actionSession.Close()
		return deployment.ActivationObservation{}, err
	}
	actionErr := actionSession.RebootSystemLevel(actionCtx)
	// Only an explicit pre-submission transport result proves the command
	// did not run. A timeout after dispatch remains uncertain and is observed.
	if sshprobe.Code(actionErr) == "SSH_REBOOT_NOT_STARTED" {
		_ = actionSession.Close()
		if ctx.Err() != nil {
			return deployment.ActivationObservation{}, ctx.Err()
		}
		if err := request.VerifyResetSafetyFresh(); err != nil {
			return deployment.ActivationObservation{}, err
		}
		return deployment.ActivationObservation{}, fmt.Errorf("external reboot command was not submitted (%s)", sshprobe.Code(actionErr))
	}
	closeErr := actionSession.Close()
	if closeErr != nil && actionErr == nil {
		actionErr = closeErr
	}

	timer := time.NewTimer(r.pollInterval)
	defer timer.Stop()
	for {
		select {
		case <-operationCtx.Done():
			if ctx.Err() != nil {
				return deployment.ActivationObservation{}, ctx.Err()
			}
			if actionErr != nil {
				return deployment.ActivationObservation{}, fmt.Errorf("host reboot was not confirmed before wait expired (action result %s)", sshprobe.Code(actionErr))
			}
			return deployment.ActivationObservation{}, fmt.Errorf("host reboot was not confirmed before wait expired")
		case <-timer.C:
		}

		after, observeErr := r.observe(operationCtx, binding)
		if observeErr != nil {
			if fatalRebootObservation(observeErr) {
				return deployment.ActivationObservation{}, fmt.Errorf("reconnect to the selected host with its configured SSH trust: %w", observeErr)
			}
			resetTimer(timer, r.pollInterval)
			continue
		}
		if !sameUUID(after.SystemUUID, request.HostSystemUUID) || after.AdapterSerial != request.HostAdapterSerial {
			return deployment.ActivationObservation{}, fmt.Errorf("reconnected SSH endpoint reports a different host or selected adapter identity")
		}
		if after.BootID != before.BootID {
			return deployment.ActivationObservation{
				// UUIDs are case-insensitive hexadecimal identifiers. Preserve the
				// inventory spelling because the lifecycle receipt compares it
				// against that same retained identity string.
				HostSystemUUID:    request.HostSystemUUID,
				HostAdapterSerial: after.AdapterSerial,
				PreviousBootID:    before.BootID,
				CurrentBootID:     after.BootID,
				Reconnected:       true,
			}, nil
		}
		resetTimer(timer, r.pollInterval)
	}
}

type rebootBinding struct {
	target config.Target
	pci    string
}

func resolveRebootBinding(access config.Request, activation deployment.ActivationRequest) (rebootBinding, error) {
	if activation.HostInputRef == "" || activation.HostEndpoint == "" || activation.HostAdapterKey == "" || activation.HostAdapterSerial == "" || !validSystemUUID(activation.HostSystemUUID) || len(activation.DPUNodes) == 0 {
		return rebootBinding{}, fmt.Errorf("activation request has incomplete host identity or selected DPU scope")
	}
	endpoint, err := parseSSHEndpoint(activation.HostEndpoint)
	if err != nil || endpoint != activation.HostEndpoint {
		return rebootBinding{}, fmt.Errorf("activation host endpoint is not canonical")
	}
	var target *config.Target
	for i := range access.Targets {
		candidate := &access.Targets[i]
		if candidate.Role != "hosts" || candidate.InputRef != activation.HostInputRef {
			continue
		}
		if target != nil {
			return rebootBinding{}, fmt.Errorf("activation host source maps to multiple configured SSH targets")
		}
		target = candidate
	}
	if target == nil || target.Endpoint != endpoint || target.SSH == nil {
		return rebootBinding{}, fmt.Errorf("activation host does not match one explicitly configured host SSH target")
	}

	inventorySnapshot := access.Input.Site.Inventory
	if inventorySnapshot == nil || inventorySnapshot.FormatVersion != inventory.FormatVersion {
		return rebootBinding{}, fmt.Errorf("activation requires the current selected physical inventory")
	}
	var host *inventory.Device
	for i := range inventorySnapshot.Hosts {
		candidate := &inventorySnapshot.Hosts[i]
		if candidate.InputRef != activation.HostInputRef {
			continue
		}
		if host != nil {
			return rebootBinding{}, fmt.Errorf("activation host identity is ambiguous in inventory")
		}
		host = candidate
	}
	if host == nil || host.ObservationState != "observed" || host.ID == "" || host.SSHEndpoint != endpoint || !sameUUID(host.SystemUUID, activation.HostSystemUUID) {
		return rebootBinding{}, fmt.Errorf("activation host identity differs from configured discovery inventory")
	}
	selectedAdapter, err := uniqueInventoryAdapter(host, activation.HostAdapterKey)
	if err != nil || selectedAdapter.IdentityState != "known" || selectedAdapter.SerialNumber != activation.HostAdapterSerial || !validSystemUUID(activation.HostSystemUUID) {
		return rebootBinding{}, fmt.Errorf("activation adapter identity differs from configured discovery inventory")
	}
	if err := validateActivationDPUs(inventorySnapshot, *host, *selectedAdapter, activation.DPUNodes); err != nil {
		return rebootBinding{}, err
	}
	return rebootBinding{target: *target, pci: selectedAdapter.PCIAddress}, nil
}

func uniqueInventoryAdapter(host *inventory.Device, key string) (*inventory.Adapter, error) {
	var selected *inventory.Adapter
	for i := range host.Adapters {
		adapter := &host.Adapters[i]
		if adapter.Key != key {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("activation adapter key is ambiguous")
		}
		selected = adapter
	}
	if selected == nil || selected.PCIAddress == "" {
		return nil, fmt.Errorf("activation adapter PCI identity is unavailable")
	}
	return selected, nil
}

func validateActivationDPUs(inv *inventory.Inventory, host inventory.Device, adapter inventory.Adapter, nodes []deployment.ActivationNode) error {
	selected := map[string]bool{}
	selectedHostAdapter := false
	for _, node := range nodes {
		if node.NodeName == "" || node.NodeUID == "" || len(node.SelectedDPUs) == 0 || selectedNodeDuplicate(nodes, node) {
			return fmt.Errorf("activation DPUNode scope is incomplete or ambiguous")
		}
		for _, dpu := range node.SelectedDPUs {
			if dpu.SerialNumber == "" || dpu.DPUName == "" || dpu.DPUUID == "" || selected[dpu.SerialNumber] {
				return fmt.Errorf("activation DPU identity is incomplete or duplicated")
			}
			selected[dpu.SerialNumber] = true
			device, err := uniqueInventoryDPU(inv, dpu.SerialNumber)
			if err != nil {
				return err
			}
			association, err := uniqueDPUAssociation(inv, *device)
			if err != nil || association.HostID != host.ID {
				return fmt.Errorf("selected DPU %q is not associated with the activation host", dpu.SerialNumber)
			}
			associatedHostAdapter, err := uniqueInventoryAdapter(&host, association.HostAdapterKey)
			if err != nil || associatedHostAdapter.IdentityState != "known" || associatedHostAdapter.SerialNumber != dpu.SerialNumber {
				return fmt.Errorf("selected DPU %q does not map to one current host adapter", dpu.SerialNumber)
			}
			if associatedHostAdapter.Key == adapter.Key {
				selectedHostAdapter = true
			}
		}
	}
	if !selectedHostAdapter {
		return fmt.Errorf("the activation adapter is not associated with a selected DPU")
	}
	for _, association := range inv.Associations {
		if association.HostID != host.ID {
			continue
		}
		if association.DPUID == "" || association.SerialNumber == "" || association.HostAdapterKey == "" {
			return fmt.Errorf("activation host has an incomplete DPU association")
		}
		device, err := uniqueInventoryDPU(inv, association.SerialNumber)
		if err != nil || device.ID != association.DPUID || device.SerialNumber != association.SerialNumber {
			return fmt.Errorf("activation host has an inconsistent DPU association")
		}
		associatedAdapter, err := uniqueInventoryAdapter(&host, association.HostAdapterKey)
		if err != nil || associatedAdapter.IdentityState != "known" || associatedAdapter.SerialNumber != device.SerialNumber {
			return fmt.Errorf("activation host has an inconsistent adapter association")
		}
		if !selected[device.SerialNumber] {
			return fmt.Errorf("activation omits a selected DPU associated with the rebooted host")
		}
	}
	return nil
}

func selectedNodeDuplicate(nodes []deployment.ActivationNode, selected deployment.ActivationNode) bool {
	count := 0
	for _, node := range nodes {
		if node.NodeName == selected.NodeName || node.NodeUID == selected.NodeUID {
			count++
		}
	}
	return count > 1
}

func uniqueInventoryDPU(inv *inventory.Inventory, serial string) (*inventory.Device, error) {
	var selected *inventory.Device
	for i := range inv.DPUs {
		device := &inv.DPUs[i]
		if device.SerialNumber != serial {
			continue
		}
		if selected != nil || device.ID == "" || device.ObservationState != "observed" {
			return nil, fmt.Errorf("selected DPU %q identity is ambiguous or unavailable", serial)
		}
		selected = device
	}
	if selected == nil {
		return nil, fmt.Errorf("selected DPU %q is absent from current inventory", serial)
	}
	return selected, nil
}

func uniqueDPUAssociation(inv *inventory.Inventory, dpu inventory.Device) (*inventory.Association, error) {
	var selected *inventory.Association
	for i := range inv.Associations {
		association := &inv.Associations[i]
		if association.DPUID != dpu.ID && association.SerialNumber != dpu.SerialNumber {
			continue
		}
		if selected != nil || association.DPUID != dpu.ID || association.SerialNumber != dpu.SerialNumber || association.HostID == "" || association.HostAdapterKey == "" {
			return nil, fmt.Errorf("selected DPU %q association is missing, ambiguous or conflicting", dpu.SerialNumber)
		}
		selected = association
	}
	if selected == nil {
		return nil, fmt.Errorf("selected DPU %q association is unavailable", dpu.SerialNumber)
	}
	return selected, nil
}

func (r *externalRebooter) observe(ctx context.Context, binding rebootBinding) (sshprobe.HostIdentity, error) {
	session, err := r.openSession(ctx, binding.target.Endpoint, binding.target.SSH, r.commandTimeout)
	if err != nil {
		return sshprobe.HostIdentity{}, err
	}
	defer session.Close()
	identity, err := session.ObserveHost(ctx, binding.pci)
	if err != nil {
		return sshprobe.HostIdentity{}, err
	}
	return identity, nil
}

func fatalRebootObservation(err error) bool {
	switch sshprobe.Code(err) {
	case "SSH_HOST_KEY_REJECTED", "SSH_TRUST_INVALID", "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", "SSH_CONFIGURATION_INVALID", "SSH_IDENTITY_INVALID":
		return true
	default:
		return false
	}
}

func sameUUID(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func validSystemUUID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return strings.Trim(strings.ToLower(value), "0-") != "" && strings.Trim(strings.ToLower(value), "f-") != ""
}

func parseSSHEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "ssh" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() == "" {
		return "", fmt.Errorf("invalid SSH endpoint")
	}
	return endpoint, nil
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}
