// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

// MinimumHostWorkloadMTU is the qualified IPv4 backend minimum. Lower MTUs can
// remove unowned IPv6 link state, which this transaction does not restore.
const MinimumHostWorkloadMTU = 1280

// HostNetworkIdentity identifies one physical host PF, never a representor.
type HostNetworkIdentity struct {
	SystemUUID    string `json:"systemUUID"`
	AdapterSerial string `json:"adapterSerial"`
	PCIAddress    string `json:"pciAddress"`
	InterfaceName string `json:"interfaceName"`
	PermanentMAC  string `json:"permanentMAC"`
	PhysicalPort  string `json:"physicalPort"`
}

// HostNetworkRequest contains explicit profile and freshly bound lease values.
// LeaseBinding is the caller's SHA-256 binding of its fresh cohort/lease receipt;
// transport does not interpret Kubernetes objects or reserve an allocation.
type HostNetworkRequest struct {
	Identity          HostNetworkIdentity `json:"identity"`
	LeaseBinding      string              `json:"leaseBinding"`
	MTU               int                 `json:"mtu"`
	HostAddressPrefix string              `json:"hostAddressPrefix"`
	Gateway           string              `json:"gateway"`
	WorkloadCIDR      string              `json:"workloadCIDR"`
}

// HostNetworkReceipt distinguishes observed configuration from traffic or
// reboot-persistence proof. It never contains credentials or raw Netplan input.
type HostNetworkReceipt struct {
	RequestID        string `json:"requestID"`
	State            string `json:"state"`
	ReasonCode       string `json:"reasonCode,omitempty"`
	JournalPath      string `json:"journalPath,omitempty"`
	FragmentPath     string `json:"fragmentPath,omitempty"`
	FragmentSHA256   string `json:"fragmentSHA256,omitempty"`
	OldMTU           int    `json:"oldMTU,omitempty"`
	ChangedMTU       bool   `json:"changedMTU"`
	AddedAddress     bool   `json:"addedAddress"`
	AddedRoute       bool   `json:"addedRoute"`
	RuntimeVerified  bool   `json:"runtimeVerified"`
	FileVerified     bool   `json:"fileVerified"`
	ExternalVerified bool   `json:"externalVerified"`
	VerifiedRepeat   bool   `json:"verifiedRepeat"`
	Blocking         bool   `json:"blocking"`
}

// HostNetworkError carries only a stable reason and whether manual inspection
// is required. Unknown command acknowledgement never authorizes a retry.
type HostNetworkError struct {
	ReasonCode string
	Blocking   bool
	ackCause   string
	cause      error
}

func (e *HostNetworkError) Error() string { return e.ReasonCode }
func (e *HostNetworkError) Unwrap() error {
	return errors.Join(&Error{Code: e.ReasonCode, Stage: "host-network"}, e.cause)
}

type hostNetworkRemote interface {
	Run(context.Context, string, string) (string, error)
	RunWithInput(context.Context, string, string, []byte) (string, error)
	RunWithInputTimeout(context.Context, string, string, []byte, time.Duration) (string, error)
}

type hostNetworkSession struct {
	remote hostNetworkRemote
	sudo   bool
	secret []byte
}

//go:embed host_network_backend.sh
var hostNetworkBackend string

var hostNetworkToken = regexp.MustCompile(`^[0-9a-f]{64}$`)
var hostNetworkLeaseBinding = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)
var hostNetworkLogicalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
var hostNetworkReason = regexp.MustCompile(`^HOST_NETWORK_[A-Z0-9_]{1,80}$`)

// ObserveHostWorkloadNetwork is read-only and requires no fabricated lease.
// Raw merged Netplan bytes remain private to the in-process observation.
func ObserveHostWorkloadNetwork(ctx context.Context, target config.Target, identity HostNetworkIdentity, timeout time.Duration) (observation HostNetworkObservation, err error) {
	if err := validateHostNetworkAccess(ctx, target, identity, timeout); err != nil {
		return HostNetworkObservation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, closeSession, err := openHostNetworkSession(ctx, target, timeout)
	if err != nil {
		return HostNetworkObservation{}, err
	}
	defer func() {
		if closeErr := closeSession(); closeErr != nil && err == nil {
			err = networkError("HOST_NETWORK_SSH_CLEANUP_UNCONFIRMED", false)
		}
	}()
	observation, err = session.observe(ctx, identity)
	clear(observation.merged)
	observation.merged = nil
	observation.raw = hostNetworkSnapshot{}
	return observation, err
}

// ConfigureHostWorkloadNetwork performs one fixed host-local transaction. The
// caller owns fresh lease/cohort fences before and after this operation. A
// returned error never authorizes automatic replay or arbitrary cleanup.
func ConfigureHostWorkloadNetwork(ctx context.Context, target config.Target, request HostNetworkRequest, timeout time.Duration) (receipt HostNetworkReceipt, err error) {
	return hostWorkloadNetwork(ctx, target, request, timeout, "configure")
}

// InspectHostWorkloadNetwork verifies an exact prior transaction without any
// configuration fallback. A missing/partial transaction cannot trigger writes.
func InspectHostWorkloadNetwork(ctx context.Context, target config.Target, request HostNetworkRequest, timeout time.Duration) (HostNetworkReceipt, error) {
	return hostWorkloadNetwork(ctx, target, request, timeout, "inspect")
}

func hostWorkloadNetwork(ctx context.Context, target config.Target, request HostNetworkRequest, timeout time.Duration, action string) (receipt HostNetworkReceipt, err error) {
	if err := validateHostNetworkAccess(ctx, target, request.Identity, timeout); err != nil {
		return HostNetworkReceipt{}, err
	}
	if !hostNetworkLeaseBinding.MatchString(request.LeaseBinding) {
		return HostNetworkReceipt{}, networkError("HOST_NETWORK_LEASE_BINDING_INVALID", false)
	}
	if err := validateHostNetworkDesired(request); err != nil {
		return HostNetworkReceipt{}, err
	}
	// The compound transaction owns the caller's existing operation deadline.
	// Dial and snapshot commands retain their separate short request timeout.
	session, closeSession, err := openHostNetworkSession(ctx, target, timeout)
	if err != nil {
		return HostNetworkReceipt{}, err
	}
	defer func() {
		if closeErr := closeSession(); closeErr != nil && err == nil {
			receipt.Blocking = true
			receipt.ReasonCode = "HOST_NETWORK_SSH_CLEANUP_UNCONFIRMED"
			err = networkError(receipt.ReasonCode, true)
		}
	}()
	return session.configureAction(ctx, request, action)
}

func validateHostNetworkAccess(ctx context.Context, target config.Target, identity HostNetworkIdentity, timeout time.Duration) error {
	if ctx == nil || target.Role != "hosts" || target.SSH == nil || timeout < time.Second || timeout > 5*time.Minute {
		return networkError("HOST_NETWORK_INPUT_INVALID", false)
	}
	if _, ok := ctx.Deadline(); !ok {
		return networkError("HOST_NETWORK_DEADLINE_REQUIRED", false)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validUUID(identity.SystemUUID) || identity.AdapterSerial == "" || len(identity.AdapterSerial) > 128 || strings.ContainsAny(identity.AdapterSerial, "\x00\r\n\t ") ||
		!externalPCIAddress.MatchString(identity.PCIAddress) || !hostInterfaceName.MatchString(identity.InterfaceName) ||
		strings.HasPrefix(identity.InterfaceName, "-") || identity.InterfaceName == "." || identity.InterfaceName == ".." || identity.PhysicalPort != "p0" {
		return networkError("HOST_NETWORK_IDENTITY_INVALID", false)
	}
	if _, err := parsePermanentMAC("Permanent address: " + identity.PermanentMAC); err != nil {
		return networkError("HOST_NETWORK_IDENTITY_INVALID", false)
	}
	return nil
}

func validateHostNetworkDesired(r HostNetworkRequest) error {
	if r.MTU < MinimumHostWorkloadMTU {
		return networkError("HOST_NETWORK_MTU_UNSUPPORTED", false)
	}
	host, hErr := netip.ParsePrefix(r.HostAddressPrefix)
	gateway, gErr := netip.ParseAddr(r.Gateway)
	workload, wErr := netip.ParsePrefix(r.WorkloadCIDR)
	if r.MTU < 1 || r.MTU > 65535 || hErr != nil || gErr != nil || wErr != nil || !host.Addr().Is4() || !gateway.Is4() || !workload.Addr().Is4() ||
		host.Bits() > 30 || workload.Bits() == 0 || workload.Bits() >= host.Bits() || workload != workload.Masked() || !host.Contains(gateway) || host.Addr() == gateway ||
		host.Addr() == host.Masked().Addr() || gateway == host.Masked().Addr() || host.Addr() == hostLastAddress(host) || gateway == hostLastAddress(host) ||
		!workload.Contains(host.Masked().Addr()) || !workload.Contains(hostLastAddress(host)) {
		return networkError("HOST_NETWORK_DESIRED_INVALID", false)
	}
	for _, address := range []netip.Addr{host.Addr(), gateway, workload.Addr(), hostLastAddress(workload)} {
		if !address.IsGlobalUnicast() || address.IsLoopback() || address.IsLinkLocalUnicast() {
			return networkError("HOST_NETWORK_DESIRED_INVALID", false)
		}
	}
	return nil
}

func hostLastAddress(prefix netip.Prefix) netip.Addr {
	base := prefix.Masked().Addr().As4()
	value := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	value += uint32(1)<<uint(32-prefix.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

func openHostNetworkSession(ctx context.Context, target config.Target, timeout time.Duration) (*hostNetworkSession, func() error, error) {
	user, password := target.SSH.Credentials()
	key := target.SSH.PrivateKey()
	auth := credentials{user: user, password: password, key: key, knownHosts: target.SSH.KnownHosts()}
	r, err := newReconnectingRunner(ctx, target.Endpoint, auth, timeout)
	clear(auth.key)
	clear(auth.knownHosts)
	if err != nil {
		return nil, nil, err
	}
	s := &hostNetworkSession{remote: r, sudo: target.SSH.Sudo}
	if s.sudo && len(key) == 0 && password != "" {
		s.secret = []byte(password)
	}
	return s, func() error { clear(s.secret); return r.Close() }, nil
}

func (s *hostNetworkSession) run(ctx context.Context, action string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 64<<10 {
		return nil, networkError("HOST_NETWORK_INPUT_INVALID", false)
	}
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C /bin/bash -c '" + strings.ReplaceAll(hostNetworkBackend, "'", "'\\''") + "' -- " + action + " " + base64.StdEncoding.EncodeToString(data)
	var input []byte
	if s.sudo {
		if len(s.secret) == 0 {
			command = "/usr/bin/sudo -n " + command
		} else {
			command = "/usr/bin/sudo -S -p '' " + command
			input = append(bytes.Clone(s.secret), '\n')
			defer clear(input)
		}
	}
	var text string
	stage := "host-network-" + action
	if action == "configure" || action == "inspect" {
		deadline, bounded := ctx.Deadline()
		if !bounded {
			return nil, networkError("HOST_NETWORK_DEADLINE_REQUIRED", false)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		text, err = s.remote.RunWithInputTimeout(ctx, stage, command, input, remaining)
	} else if len(input) > 0 {
		text, err = s.remote.RunWithInput(ctx, stage, command, input)
	} else {
		text, err = s.remote.Run(ctx, stage, command)
	}
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

func networkError(reason string, blocking bool) error {
	if !hostNetworkReason.MatchString(reason) || strings.HasPrefix(reason, "HOST_NETWORK_READ_") && !validHostNetworkReadReason(reason) {
		reason = "HOST_NETWORK_RESPONSE_INVALID"
	}
	return &HostNetworkError{ReasonCode: reason, Blocking: blocking}
}

func unknownHostNetworkReceipt(id, key, reason string) HostNetworkReceipt {
	return HostNetworkReceipt{RequestID: id, State: "unknown", ReasonCode: reason, Blocking: true,
		JournalPath: "/var/lib/l8k-host-network/" + key + ".json", FragmentPath: "/etc/netplan/99-l8k-workload-" + key + ".yaml"}
}

func networkDigest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeNetworkJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return networkError("HOST_NETWORK_RESPONSE_INVALID", false)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("HOST_NETWORK_RESPONSE_INVALID")
	}
	return nil
}
