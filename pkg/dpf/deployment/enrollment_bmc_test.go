// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func enrollmentBMCInputs(t *testing.T) ([]generation.ResolvedDevice, []config.Target) {
	t.Helper()
	t.Setenv("ENROLLMENT_TEST_PASSWORD", "credential-canary")
	t.Setenv("ENROLLMENT_TEST_OTHER_PASSWORD", "other-canary")
	in, err := config.Decode([]byte(`credentials:
  admin: {username: admin, passwordRef: {env: ENROLLMENT_TEST_PASSWORD}}
  other: {username: admin, passwordRef: {env: ENROLLMENT_TEST_OTHER_PASSWORD}}
discovery:
  probes: {mode: inventory}
  dpus:
    - {bmcEndpoint: 'https://192.0.2.1', credentialRef: admin}
    - {bmcEndpoint: 'https://192.0.2.2:8443', credentialRef: admin}
    - {bmcEndpoint: 'https://192.0.2.3', credentialRef: other}
  hosts:
    - {bmcEndpoint: 'https://192.0.2.4', credentialRef: admin}
`), "/site/input.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := config.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	return []generation.ResolvedDevice{
		{Serial: "CARD-A", PSID: "MT_0000001774", BMCIP: "192.0.2.1", BMCPort: 443},
		{Serial: "CARD-B", PSID: "MT_0000001774", BMCIP: "192.0.2.2", BMCPort: 8443},
	}, r.Targets
}

func TestBindEnrollmentBMCsSelectsExactRetainedScope(t *testing.T) {
	devices, targets := enrollmentBMCInputs(t)
	selected, password, err := bindEnrollmentBMCs(devices, targets)
	if err != nil || password != "credential-canary" || len(selected) != 2 {
		t.Fatalf("selected binding failed: %v", err)
	}
	for i, item := range selected {
		if item.device.Serial != devices[i].Serial || item.target.Endpoint != targets[i].Endpoint {
			t.Fatal("binding changed the selected identity or endpoint")
		}
	}
	// Explicit default ports and an empty root path have the same origin.
	targets[0].Endpoint = "https://192.0.2.1:443/"
	if _, _, err := bindEnrollmentBMCs(devices, targets); err != nil {
		t.Fatal(err)
	}
}

func TestBindEnrollmentBMCsRejectsIncompatibleInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]generation.ResolvedDevice, []config.Target) ([]generation.ResolvedDevice, []config.Target)
	}{
		{"no devices", func(_ []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			return nil, ts
		}},
		{"missing access", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			return ds, ts[1:]
		}},
		{"different password", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[1].Access = ts[2].Access
			return ds, ts
		}},
		{"missing credentials", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[0].Access = config.Access{}
			return ds, ts
		}},
		{"duplicate endpoint", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[2].Endpoint = "https://192.0.2.1:443"
			return ds, ts
		}},
		{"duplicate serial name", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ds[1].Serial = "card-a"
			return ds, ts
		}},
		{"duplicate selected endpoint", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ds[1].BMCIP, ds[1].BMCPort = ds[0].BMCIP, ds[0].BMCPort
			return ds, ts
		}},
		{"port drift", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ds[0].BMCPort = 8443
			return ds, ts
		}},
		{"IPv6", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ds[0].BMCIP = "2001:db8::1"
			return ds, ts
		}},
		{"DNS endpoint", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[0].Endpoint = "https://bmc.example"
			return ds, ts
		}},
		{"credentials in URL", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[0].Endpoint = "https://admin:credential-canary@192.0.2.1"
			return ds, ts
		}},
		{"query in URL", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ts[0].Endpoint += "?credential-canary"
			return ds, ts
		}},
		{"invalid selected name", func(ds []generation.ResolvedDevice, ts []config.Target) ([]generation.ResolvedDevice, []config.Target) {
			ds[0].Serial = "card_a"
			return ds, ts
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, ts := enrollmentBMCInputs(t)
			ds, ts = tc.edit(ds, ts)
			selected, password, err := bindEnrollmentBMCs(ds, ts)
			if err == nil || selected != nil || password != "" {
				t.Fatal("invalid binding returned usable enrollment credentials")
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatal("binding error exposed credentials")
			}
		})
	}
}

type enrollmentCollectorFunc func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error)

func (f enrollmentCollectorFunc) Collect(ctx context.Context, target config.Target, timeout time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
	return f(ctx, target, timeout)
}

func TestEnrollmentBMCChecksFreshIdentityAndAdministrator(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*inventory.Device) error
		want string
	}{
		{"verified", func(*inventory.Device) error { return nil }, ""},
		{"PSID not exposed", func(d *inventory.Device) error {
			d.PSID = ""
			d.Observations["psid"] = inventory.Observation{State: "unknown", ReasonCode: "PSID_NOT_EXPOSED"}
			return nil
		}, ""},
		{"PSID conflicting source", func(d *inventory.Device) error {
			d.Observations["psid"] = inventory.Observation{State: "unavailable", ReasonCode: "PSID_CONFLICT"}
			return nil
		}, "ENROLLMENT_BMC_PSID_CHANGED"},
		{"PSID observation mismatch", func(d *inventory.Device) error {
			d.Observations["psid"] = inventory.Observation{State: "observed", Values: []string{"MT_OTHER"}}
			return nil
		}, "ENROLLMENT_BMC_PSID_CHANGED"},
		{"PSID mismatch", func(d *inventory.Device) error { d.PSID = "MT_OTHER"; return nil }, "ENROLLMENT_BMC_PSID_CHANGED"},
		{"serial mismatch", func(d *inventory.Device) error { d.SerialNumber = "CARD-X"; return nil }, "ENROLLMENT_BMC_IDENTITY_CHANGED"},
		{"identity incomplete", func(d *inventory.Device) error { d.ID = ""; return nil }, "ENROLLMENT_BMC_IDENTITY_CHANGED"},
		{"admin absent", func(d *inventory.Device) error { d.Observations = nil; return nil }, "ENROLLMENT_BMC_ADMIN_REQUIRED"},
		{"admin rejected", func(d *inventory.Device) error {
			d.Observations["bmc-administrator"] = inventory.Observation{State: "observed", ReasonCode: "BMC_ADMIN_REQUIRED"}
			return nil
		}, "ENROLLMENT_BMC_ADMIN_REQUIRED"},
		{"collector or cleanup error", func(*inventory.Device) error { return fmt.Errorf("credential-canary raw remote body") }, "ENROLLMENT_BMC_IDENTITY_UNVERIFIED"},
		{"cancellation", func(*inventory.Device) error { return context.Canceled }, "ENROLLMENT_BMC_IDENTITY_UNVERIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devices, targets := enrollmentBMCInputs(t)
			selected, _, err := bindEnrollmentBMCs(devices, targets)
			if err != nil {
				t.Fatal(err)
			}
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			calls := 0
			collector := enrollmentCollectorFunc(func(_ context.Context, target config.Target, timeout time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
				if target.Endpoint != targets[calls].Endpoint || timeout != time.Second {
					t.Fatal("probe changed selected endpoint scope or timeout")
				}
				d := inventory.Device{ObservationState: "observed", ID: "physical", SerialNumber: devices[calls].Serial, PSID: devices[calls].PSID,
					Observations: map[string]inventory.Observation{
						"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"},
						"psid":              {State: "observed", ReasonCode: "PSID_OBSERVED", Values: []string{devices[calls].PSID}},
					}}
				calls++
				err := tc.edit(&d)
				return d, nil, err
			})
			err = collectEnrollmentBMCs(ctx, selected, time.Second, journal, collector)
			if tc.want == "" {
				if err != nil || calls != 2 {
					t.Fatalf("selected checks did not finish: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || calls != 1 {
				t.Fatalf("failure did not stop enrollment: calls=%d err=%v", calls, err)
			}
			if tc.name == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation classification lost")
			}
			if strings.Contains(file.String()+fmt.Sprint(err), "canary") {
				t.Fatal("secret leaked into diagnostic output")
			}
		})
	}
}

func TestEnrollmentBMCChecksDoNotProbeWithoutDeadlineOrDurableJournal(t *testing.T) {
	devices, targets := enrollmentBMCInputs(t)
	selected, _, err := bindEnrollmentBMCs(devices, targets)
	if err != nil {
		t.Fatal(err)
	}
	collector := enrollmentCollectorFunc(func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		t.Fatal("probe attempted without durable bounded execution")
		return inventory.Device{}, nil, nil
	})
	if err := collectEnrollmentBMCs(context.Background(), selected, time.Second, &executionJournal{}, collector); err == nil {
		t.Fatal("unbounded check accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := collectEnrollmentBMCs(ctx, selected, time.Second, &executionJournal{}, collector); err == nil {
		t.Fatal("unusable journal accepted")
	}
}
