// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestValidationDiagnosticActualOpenRuntimeIdentityEntryFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		tenant   bool
		cause    error
		category validation.FatalCategory
	}{
		{"management unauthorized", false, apierrors.NewUnauthorized("PRIVATE-ENTRY-CAUSE"), validation.FatalAccess},
		{"management unknown", false, errors.New("PRIVATE-ENTRY-CAUSE"), validation.FatalObservation},
		{"tenant forbidden", true, apierrors.NewForbidden(schema.GroupResource{Resource: "dpuclusters"}, "selected", errors.New("PRIVATE-ENTRY-CAUSE")), validation.FatalAccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			native, _, _ := physicalValidationFixture(t)
			managementCalls, tenantReads := 0, 0
			native.newManagement = func(context.Context) (validationManagementReader, error) {
				managementCalls++
				if !test.tenant {
					return nil, test.cause
				}
				return &validationReadFake{get: func(_ context.Context, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
					tenantReads++
					if object.GetKind() != "DPUCluster" {
						t.Error("entry reached an unexpected resource")
					}
					return nil, test.cause
				}}, nil
			}
			result := runValidationRuntimeEntryFailure(t, func(ctx context.Context) error {
				session, err := native.OpenRuntime(ctx, nil)
				if session != nil || err == nil || !errors.Is(err, test.cause) {
					t.Fatal("entry did not preserve its source failure and early return")
				}
				return err
			})
			if managementCalls != 1 || tenantReads != map[bool]int{false: 0, true: 1}[test.tenant] || result.Fatal != test.category || result.Diagnostic == nil || result.Diagnostic.Stage != validation.StageRuntimeIdentity {
				t.Fatalf("entry classification: management=%d tenant=%d fatal=%s diagnostic=%+v", managementCalls, tenantReads, result.Fatal, result.Diagnostic)
			}
			if strings.Contains(result.Diagnostic.Summary, "PRIVATE-") {
				t.Fatal("entry diagnostic exposed unknown source text")
			}
			for _, row := range result.Checks {
				if strings.HasPrefix(string(row.ID), "runtime/") && (row.Outcome.BGP != nil || row.Outcome.Route != nil || row.Outcome.Traffic != nil) {
					t.Fatal("failed entry performed runtime probes")
				}
			}
		})
	}
}

func TestValidationDiagnosticActualOpenRuntimePhysicalEntryFailure(t *testing.T) {
	native, _, _ := physicalValidationFixture(t)
	managementCalls := 0
	native.physical.bmc = func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		return inventory.Device{}, nil, errors.New("PRIVATE-PHYSICAL-CAUSE")
	}
	native.newManagement = func(context.Context) (validationManagementReader, error) {
		managementCalls++
		return nil, errors.New("unexpected management read")
	}
	result := runValidationRuntimeEntryFailure(t, func(ctx context.Context) error {
		session, err := native.OpenRuntime(ctx, nil)
		if session != nil || err == nil {
			t.Fatal("physical failure did not stop entry")
		}
		return err
	})
	if managementCalls != 0 || result.Fatal != validation.FatalAccess || result.Diagnostic == nil || result.Diagnostic.Stage != validation.StageRuntimePhysical || result.ExitCode() != 3 {
		t.Fatalf("physical entry category/diagnostic lost: management=%d fatal=%s diagnostic=%+v", managementCalls, result.Fatal, result.Diagnostic)
	}
}

func TestValidationDiagnosticInitialHostNetworkFenceFailure(t *testing.T) {
	session, _ := standaloneValidationSession()
	hosts := []validationHostRequest{session.hosts["SERIAL-A"]}
	hosts[0].selected.hostTarget.SSH = &config.SSHAccess{}
	reads := 0
	result := runValidationRuntimeEntryFailure(t, func(ctx context.Context) error {
		fence, err := openCurrentHostNetworkFence(ctx, hosts, time.Second, func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkValidation, error) {
			reads++
			return sshprobe.HostNetworkValidation{}, &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED"}
		})
		if fence != nil || err == nil {
			t.Fatal("initial host qualification did not stop on authentication failure")
		}
		return validationRuntimeStageError(ctx, validation.StageRuntimeHostNetwork, err)
	})
	if reads != 1 || result.Fatal != validation.FatalAccess || result.ExitCode() != 3 || result.Diagnostic == nil || result.Diagnostic.Stage != validation.StageRuntimeHostNetwork || !strings.Contains(result.Diagnostic.Summary, "SSH authentication") {
		t.Fatalf("initial host diagnostic lost: reads=%d fatal=%s diagnostic=%+v", reads, result.Fatal, result.Diagnostic)
	}
}

func TestValidationDiagnosticPhysicalFinalFenceRetainsMeasurements(t *testing.T) {
	session, _ := standaloneValidationSession()
	plan := correctionSessionPlan(t, session)
	session.read = func(_ context.Context, _ string, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
		if command == hbnReadBGPSummary {
			asn := uint32(65101)
			if pod == "hbn-b" {
				asn = 65102
			}
			return runtimeBGPJSON(asn, 65000), nil
		}
		return runtimeFIBJSON(prefix.String(), "p0_if"), nil
	}
	traffic, physical := 0, 0
	session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
		traffic++
		return runtimePassedTraffic(request), nil
	}
	cause := &validation.ObservationError{Category: validation.FatalAccess, Cause: errors.New("PRIVATE-FINAL-PHYSICAL-CAUSE")}
	session.verifyPhysical = func(context.Context) error { physical++; return cause }
	observer := &runtimeCorrectionObserver{open: func(context.Context) (validation.RuntimeSession, error) { return session, nil }}
	observer.snapshot = func() validation.ResourceSnapshot {
		snapshot := validation.ResourceSnapshot{}
		for _, check := range plan.Input().Checks {
			snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: validation.Passed})
		}
		return snapshot
	}
	runner, err := validation.NewRunner(plan, observer, validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if traffic != len(session.scope.Traffic) || physical != 1 || result.Fatal != validation.FatalAccess || result.Diagnostic == nil || result.Diagnostic.Stage != validation.StageRuntimePhysical || !errors.Is(result.Cause, cause) {
		t.Fatalf("physical final category/diagnostic lost: traffic=%d physical=%d fatal=%s diagnostic=%+v", traffic, physical, result.Fatal, result.Diagnostic)
	}
	for _, row := range result.Checks {
		if strings.HasPrefix(string(row.ID), "runtime/") && (row.Outcome.State != validation.Unverified || row.PriorOutcome == nil || row.PriorOutcome.State != validation.Passed) {
			t.Fatalf("final physical failure lost earlier runtime facts: %+v", row)
		}
	}
	if err := session.VerifyCohort(context.Background()); err == nil || physical != 1 {
		t.Fatal("poisoned final fence repeated physical reads")
	}
}
