// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestActivationRebootReceiptsCoverEverySelectedHost(t *testing.T) {
	fixture, activation, _ := hostNetworkStageFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	if len(activation.Reboots) != 2 {
		t.Fatalf("activation has %d verified host reboot receipts, want 2: %#v", len(activation.Reboots), activation.Reboots)
	}

	wantHosts := make(map[string][]string, 2)
	for _, selected := range activation.selected {
		wantHosts[selected.host.InputRef] = append(wantHosts[selected.host.InputRef], selected.nodeName)
	}
	gotHosts := make(map[string][]string, len(activation.Reboots))
	for _, receipt := range activation.Reboots {
		if receipt.PreviousBoot != "before" || receipt.CurrentBoot != "after" {
			t.Errorf("reboot receipt for %q has unverified boot transition: %#v", receipt.HostInputRef, receipt)
		}
		gotHosts[receipt.HostInputRef] = receipt.NodeNames
	}
	if !reflect.DeepEqual(gotHosts, wantHosts) {
		t.Fatalf("per-host reboot receipts=%v, want selected node mapping %v", gotHosts, wantHosts)
	}

	summary := activationSummary(activation)
	if summary == nil || len(summary.RebootedHosts) != 2 {
		t.Fatalf("activation summary did not retain both rebooted hosts: %#v", summary)
	}
	gotSummary := append([]string(nil), summary.RebootedHosts...)
	wantSummary := make([]string, 0, len(wantHosts))
	for host := range wantHosts {
		wantSummary = append(wantSummary, host)
	}
	if !equalActivationStringSet(gotSummary, wantSummary) {
		t.Fatalf("summary rebooted hosts=%v, want %v", gotSummary, wantSummary)
	}
	if fixture.client.guardCalls != 2 {
		t.Fatalf("reboot guards created=%d, want one per physical host", fixture.client.guardCalls)
	}
}

func TestActivationRebootReceiptSurvivesDPFAcknowledgementFailure(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	fixture.client.rejectRebootAckRemoval = true
	var callbacks int
	rebooter := RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		callbacks++
		for _, node := range request.DPUNodes {
			fixture.client.completeReboot(node.NodeName)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial,
			PreviousBootID: "boot-before", CurrentBootID: "boot-after", Reconnected: true}, nil
	})

	receipt, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
		fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter)
	if err == nil || !strings.Contains(err.Error(), "EXTERNAL_REBOOT_ACK_FAILED") {
		t.Fatalf("activation error=%v, want external reboot acknowledgement failure", err)
	}
	if callbacks != 1 || len(receipt.Reboots) != 1 {
		t.Fatalf("callbacks=%d receipt=%#v, want one physically verified reboot retained before failed acknowledgement", callbacks, receipt.Reboots)
	}
	if receipt.Reboots[0].PreviousBoot != "boot-before" || receipt.Reboots[0].CurrentBoot != "boot-after" || receipt.Reboots[0].HostInputRef != fixture.host.InputRef {
		t.Fatalf("retained physical reboot receipt is incomplete: %#v", receipt.Reboots[0])
	}
	summary := activationSummary(receipt)
	if summary == nil || !equalActivationStringSet(summary.RebootedHosts, []string{fixture.host.InputRef}) {
		t.Fatalf("summary lost the completed physical reboot when DPF acknowledgement failed: %#v", summary)
	}
	if strings.Contains(fixture.journal.file.(*managementMemoryFile).String(), "EXTERNAL_REBOOT_ACKNOWLEDGED") {
		t.Fatal("journal claimed DPF acknowledgement after the fixture rejected it")
	}
}

func equalActivationStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func TestActivationDoesNotReportUnverifiedRebootCallbackResults(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	tests := []struct {
		name string
		call RebooterFunc
	}{
		{
			name: "callback error",
			call: func(context.Context, ActivationRequest) (ActivationObservation, error) {
				return ActivationObservation{}, errors.New("callback outcome unavailable")
			},
		},
		{
			name: "unchanged boot id",
			call: func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
				return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial,
					PreviousBootID: "same", CurrentBootID: "same", Reconnected: true}, nil
			},
		},
		{
			name: "no reconnect",
			call: func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
				return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial,
					PreviousBootID: "before", CurrentBootID: "after", Reconnected: false}, nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
			receipt, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
				fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, tc.call)
			if err == nil {
				t.Fatal("unverified reboot callback result was reported as successful")
			}
			if len(receipt.Reboots) != 0 {
				t.Fatalf("unverified callback result produced physical reboot receipts: %#v", receipt.Reboots)
			}
			summary := activationSummary(receipt)
			if summary != nil && len(summary.RebootedHosts) != 0 {
				t.Fatalf("unverified callback result appeared in activation summary: %#v", summary.RebootedHosts)
			}
			if strings.Contains(fixture.journal.file.(*managementMemoryFile).String(), "EXTERNAL_HOST_REBOOT_VERIFIED") {
				t.Fatal("journal claimed a verified reboot for an unverified callback result")
			}
		})
	}
}
