// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

// ExternalRebootSession is a narrowly scoped, explicitly side-effecting host
// session. It exposes fixed identity reads and one host SystemLevelReset; it
// does not accept arbitrary commands or implement DPF PowerCycle.
type ExternalRebootSession interface {
	ObserveHost(context.Context, string) (HostIdentity, error)
	RebootSystemLevel(context.Context) error
	Close() error
}

// HostIdentity contains only the facts needed to bind a host reboot to the
// selected machine and BlueField PCI function.
type HostIdentity struct {
	SystemUUID    string
	AdapterSerial string
	BootID        string
}

// OpenExternalRebootSession uses the same explicit endpoint, credentials and
// known-hosts verification as inventory SSH, but returns the fixed-action
// interface above instead of the read-only arbitrary-command session. Calls
// on a session must be sequential.
func OpenExternalRebootSession(ctx context.Context, endpoint string, access *config.SSHAccess, timeout time.Duration) (ExternalRebootSession, error) {
	if ctx == nil || access == nil {
		return nil, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "external-session"}
	}
	user, password := access.Credentials()
	key := access.PrivateKey()
	knownHosts := access.KnownHosts()
	auth := credentials{user: user, password: password, key: key, knownHosts: knownHosts}
	runner, err := newReconnectingRunner(ctx, endpoint, auth, timeout)
	clear(auth.key)
	clear(auth.knownHosts)
	clear(key)
	clear(knownHosts)
	auth.password = ""
	if err != nil {
		return nil, err
	}
	session := &externalRebootSession{runner: runner, sudo: access.Sudo}
	if session.sudo && len(auth.key) == 0 && password != "" {
		session.sudoPassword = []byte(password)
	}
	return session, nil
}

type externalRebootSession struct {
	runner       *reconnectingRunner
	rebootIssued bool
	identityRead bool
	sudo         bool
	sudoPassword []byte
}

const externalRebootStage = "external-reboot-system-level"

var externalPCIAddress = regexp.MustCompile(`(?i)^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

func (s *externalRebootSession) ObserveHost(ctx context.Context, pciAddress string) (HostIdentity, error) {
	if s == nil || s.runner == nil || ctx == nil || s.rebootIssued || !externalPCIAddress.MatchString(pciAddress) {
		return HostIdentity{}, &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "external-identity"}
	}
	uuid, err := s.runPrivileged(ctx, "external-host-uuid", "dmidecode -s system-uuid")
	if err != nil {
		return HostIdentity{}, err
	}
	uuid = strings.TrimSpace(uuid)
	if !validUUID(uuid) {
		return HostIdentity{}, &Error{Code: "SSH_IDENTITY_INVALID", Stage: "external-host-uuid"}
	}
	bootID, err := s.runner.Run(ctx, "external-host-boot-id", "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return HostIdentity{}, err
	}
	bootID = strings.TrimSpace(bootID)
	if !validUUID(bootID) {
		return HostIdentity{}, &Error{Code: "SSH_IDENTITY_INVALID", Stage: "external-host-boot-id"}
	}
	vpdText, err := s.runPrivileged(ctx, "external-host-adapter-vpd", "lspci -D -vv -s "+strings.ToLower(pciAddress))
	if err != nil {
		return HostIdentity{}, err
	}
	serial, _, err := vpd(vpdText)
	if err != nil || serial == "" {
		return HostIdentity{}, &Error{Code: "SSH_IDENTITY_INVALID", Stage: "external-host-adapter-vpd"}
	}
	s.identityRead = true
	return HostIdentity{SystemUUID: strings.ToLower(uuid), AdapterSerial: serial, BootID: strings.ToLower(bootID)}, nil
}

func (s *externalRebootSession) RebootSystemLevel(ctx context.Context) error {
	if s == nil || s.runner == nil || ctx == nil || s.rebootIssued || !s.identityRead {
		return &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "external-reboot"}
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return &Error{Code: "SSH_CONFIGURATION_INVALID", Stage: "external-reboot"}
	}
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		return &Error{Code: "SSH_REBOOT_NOT_STARTED", Stage: "external-reboot"}
	}
	s.rebootIssued = true
	_, err := s.runPrivileged(ctx, externalRebootStage, "systemctl reboot")
	if err != nil {
		return err
	}
	return nil
}

func (s *externalRebootSession) runPrivileged(ctx context.Context, stage, command string) (string, error) {
	if !s.sudo {
		return s.runner.Run(ctx, stage, command)
	}
	if len(s.sudoPassword) == 0 {
		return s.runner.Run(ctx, stage, "sudo -n "+command)
	}
	input := append(bytes.Clone(s.sudoPassword), '\n')
	defer clear(input)
	return s.runner.RunWithInput(ctx, stage, "sudo -S -p '' "+command, input)
}

func (s *externalRebootSession) Close() error {
	if s == nil || s.runner == nil {
		return nil
	}
	clear(s.sudoPassword)
	s.sudoPassword = nil
	if err := s.runner.Close(); err != nil {
		return fmt.Errorf("close external SSH session: %w", err)
	}
	return nil
}
