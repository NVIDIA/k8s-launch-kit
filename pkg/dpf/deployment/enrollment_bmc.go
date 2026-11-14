// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"k8s.io/apimachinery/pkg/util/validation"
)

// enrollmentBMC is private runtime state. Access credentials never become part
// of retained generation, inventory, ownership digests or execution reports.
type enrollmentBMC struct {
	device generation.ResolvedDevice
	target config.Target
}

// The HBN BlueField-4 cohort uses admin; DPF 26.8's crawler consumes one shared
// password per namespace (its account fallback is root then admin).
// Reject incompatible runtime inputs before any management or BMC operation.
func bindEnrollmentBMCs(devices []generation.ResolvedDevice, targets []config.Target) ([]enrollmentBMC, string, error) {
	if len(devices) == 0 {
		return nil, "", fmt.Errorf("enrollment requires selected DPU identities")
	}
	byEndpoint := map[netip.AddrPort]config.Target{}
	for _, target := range targets {
		if target.Role != "dpus" {
			continue
		}
		endpoint, err := enrollmentEndpoint(target.Endpoint)
		if err != nil {
			return nil, "", err
		}
		if _, duplicate := byEndpoint[endpoint]; duplicate {
			return nil, "", fmt.Errorf("enrollment requires unique DPU BMC endpoints")
		}
		byEndpoint[endpoint] = target
	}
	selected := make([]enrollmentBMC, 0, len(devices))
	serials, endpoints := map[string]bool{}, map[netip.AddrPort]bool{}
	password := ""
	for _, device := range devices {
		serial := strings.ToLower(device.Serial)
		ip, err := netip.ParseAddr(device.BMCIP)
		if !redfish.ValidSerial(device.Serial) || len("dpu-node-"+serial) > 48 || len(validation.IsDNS1123Subdomain(serial)) != 0 || device.Serial != strings.TrimSpace(device.Serial) || device.PSID == "" || err != nil || !ip.Is4() || device.BMCPort < 1 || device.BMCPort > 65535 {
			return nil, "", fmt.Errorf("selected DPU enrollment identity is incomplete or invalid")
		}
		endpoint := netip.AddrPortFrom(ip, uint16(device.BMCPort))
		if serials[serial] || endpoints[endpoint] {
			return nil, "", fmt.Errorf("selected DPU serials and enrollment endpoints must be unique")
		}
		serials[serial], endpoints[endpoint] = true, true
		target, found := byEndpoint[endpoint]
		if !found {
			return nil, "", fmt.Errorf("a selected DPU has no matching runtime BMC access; repeat discovery or correct the site inputs")
		}
		username, candidate := target.Access.Credentials()
		if username != "admin" || candidate == "" {
			return nil, "", fmt.Errorf("DPF discovery requires the admin account and a nonempty shared BMC password")
		}
		if password != "" && password != candidate {
			return nil, "", fmt.Errorf("DPF discovery requires the same BMC password for all selected devices in the namespace")
		}
		password = candidate
		// Do not retain caller-owned maps or unrelated SSH access in this boundary.
		device.SelectorLabels = nil
		target.SSH = nil
		selected = append(selected, enrollmentBMC{device: device, target: target})
	}
	return selected, password, nil
}

func enrollmentEndpoint(raw string) (netip.AddrPort, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return netip.AddrPort{}, fmt.Errorf("enrollment requires credential-free HTTPS BMC endpoints with literal IPv4 addresses")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.Is4() {
		return netip.AddrPort{}, fmt.Errorf("enrollment requires literal IPv4 BMC endpoints matching the retained plan")
	}
	port := uint64(443)
	if u.Port() != "" {
		port, err = strconv.ParseUint(u.Port(), 10, 16)
	}
	if err != nil || port == 0 || strings.HasSuffix(u.Host, ":") {
		return netip.AddrPort{}, fmt.Errorf("enrollment BMC endpoint has an invalid port")
	}
	return netip.AddrPortFrom(ip, uint16(port)), nil
}

type enrollmentBMCCollector interface {
	Collect(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error)
}

func verifyEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, requestTimeout time.Duration, journal *executionJournal) error {
	return collectEnrollmentBMCs(ctx, selected, requestTimeout, journal, redfish.Collector{})
}

func collectEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, requestTimeout time.Duration, journal *executionJournal, collector enrollmentBMCCollector) error {
	if ctx == nil || journal == nil || collector == nil || len(selected) == 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return fmt.Errorf("enrollment BMC checks require selected devices, a report and bounded request controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return fmt.Errorf("enrollment BMC checks require a deadline")
	}
	for _, selected := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		event := executionEvent{Stage: generation.StageEnrollment, Operation: "bmc-identity", Subject: selected.device.Serial,
			State: "attempted", ReasonCode: "ENROLLMENT_BMC_CHECK_STARTED", Message: "Verify the selected BMC administrator session and physical serial before controller enrollment."}
		if err := journal.record(event); err != nil {
			return err
		}
		observed, _, err := collector.Collect(ctx, selected.target, requestTimeout)
		event.State, event.ReasonCode = "failed", "ENROLLMENT_BMC_IDENTITY_UNVERIFIED"
		event.Message = "The selected BMC could not be verified, or its session cleanup failed; enrollment is stopped."
		switch {
		case err != nil:
			// Collector error chains may include remote text. Preserve cancellation
			// classification only; diagnostics never contain credentials or bodies.
		case observed.ObservationState != "observed" || observed.ID == "" || observed.SerialNumber != selected.device.Serial:
			event.ReasonCode, event.Message = "ENROLLMENT_BMC_IDENTITY_CHANGED", "The fresh physical DPU identity differs from the selected device; repeat discovery and generation."
		case observed.Observations["bmc-administrator"].State != "observed" || observed.Observations["bmc-administrator"].ReasonCode != "BMC_ADMIN_VERIFIED":
			event.ReasonCode, event.Message = "ENROLLMENT_BMC_ADMIN_REQUIRED", "The selected BMC account lacks verified Administrator privileges; enrollment is stopped."
		case !enrollmentPSIDMatches(observed, selected.device.PSID):
			event.ReasonCode, event.Message = "ENROLLMENT_BMC_PSID_CHANGED", "The fresh DPU PSID conflicts with the selected hardware or its source is inconsistent; repeat discovery and generation."
		default:
			event.State, event.ReasonCode = "confirmed", "ENROLLMENT_BMC_IDENTITY_VERIFIED"
			event.Message = "The selected physical serial and BMC Administrator session were verified and the session was closed."
		}
		if recordErr := journal.record(event); recordErr != nil {
			return recordErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if event.State != "confirmed" {
			var cause error
			if errors.Is(err, context.Canceled) {
				cause = context.Canceled
			} else if errors.Is(err, context.DeadlineExceeded) {
				cause = context.DeadlineExceeded
			}
			return reconcileError(event.ReasonCode, event.Message, cause)
		}
	}
	return nil
}

func enrollmentPSIDMatches(observed inventory.Device, expected string) bool {
	if observed.PSID != "" && !strings.EqualFold(observed.PSID, expected) {
		return false
	}
	psid := observed.Observations["psid"]
	switch psid.State {
	case "observed":
		return len(psid.Values) == 1 && strings.EqualFold(psid.Values[0], expected)
	case "unknown":
		// The retained PSID may have come from the identity-associated host
		// adapter. Absence of a Redfish property does not contradict that fact.
		return psid.ReasonCode == "PSID_NOT_EXPOSED" || psid.ReasonCode == "PSID_READER_UNSUPPORTED"
	default:
		return false
	}
}
