// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

func TestValidationAcceptanceRuntimeNegativeMeasurements(t *testing.T) {
	for _, scenario := range []string{"wrong-asn", "wrong-route", "ordinary-loss", "df-loss", "later-exec-unavailable", "canceled", "cancel-after-read"} {
		t.Run(scenario, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			session.read = func(_ context.Context, _, _ string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
				reads++
				if scenario == "cancel-after-read" {
					cancel()
				}
				if scenario == "later-exec-unavailable" && reads > 1 {
					return nil, errors.New("private-exec-canary")
				}
				if command == hbnReadBGPSummary {
					asn := uint32(65101)
					if scenario == "wrong-asn" {
						asn = 64520
					}
					return runtimeBGPJSON(asn, 65000), nil
				}
				port := "p0_if"
				if scenario == "wrong-route" {
					port = "pf0hpf_if"
				}
				return runtimeFIBJSON(prefix.String(), port), nil
			}
			session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
				value := runtimePassedTraffic(request)
				failed := &value.Ordinary
				if scenario == "df-loss" {
					failed = &value.MTUProbe
				}
				value.State, failed.State, failed.Counters.Received, failed.Counters.LostPct = sshprobe.TrafficProbeFailed, sshprobe.TrafficProbeFailed, 2, 33.3333
				return value, &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE"}
			}
			switch scenario {
			case "wrong-asn":
				value, err := session.ObserveBGP(ctx, "SERIAL-A")
				if err != nil || value.Outcome.State != validation.Failed || value.Outcome.BGP == nil {
					t.Fatalf("ASN mismatch not retained: %+v %v", value, err)
				}
			case "wrong-route":
				value, err := session.ObserveRoute(ctx, validation.DirectedDevicePair{Source: "SERIAL-A", Destination: "SERIAL-B"})
				if err != nil || value.Outcome.State != validation.Failed || value.Outcome.Route == nil {
					t.Fatalf("wrong egress not retained: %+v %v", value, err)
				}
			case "ordinary-loss", "df-loss":
				value, err := session.ProbeTraffic(ctx, validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
				failed, passed := value.Ordinary, value.DF
				if scenario == "df-loss" {
					failed, passed = value.DF, value.Ordinary
				}
				if err != nil || failed.State != validation.Failed || failed.Traffic == nil || failed.Traffic.Received != 2 || passed.State != validation.Passed {
					t.Fatalf("partial traffic lost: %+v %v", value, err)
				}
			case "later-exec-unavailable":
				first, err := session.ObserveBGP(ctx, "SERIAL-A")
				if err != nil || first.Outcome.State != validation.Passed {
					t.Fatal("first source did not pass")
				}
				later, err := session.ObserveBGP(ctx, "SERIAL-B")
				if err != nil || later.Outcome.State != validation.Unverified || reads != 2 || session.poisoned != nil {
					t.Fatalf("unavailable source lost independent evidence: %+v %v", later, err)
				}
			case "canceled", "cancel-after-read":
				if scenario == "canceled" {
					cancel()
				}
				_, err := session.ObserveBGP(ctx, "SERIAL-A")
				if !errors.Is(err, context.Canceled) || scenario == "canceled" && reads != 0 || scenario == "cancel-after-read" && reads != 1 {
					t.Fatalf("cancellation did not fence actual read: reads=%d err=%v", reads, err)
				}
			}
		})
	}
}

func TestValidationAcceptanceSelectedBindingDriftPoisonsEverySource(t *testing.T) {
	for _, scenario := range []string{"service-values", "container-restart", "sidecar-restart", "completed-init-restart", "sidecar-image", "pod-replacement", "interface-replacement", "lease-replaced", "assignment-drift"} {
		t.Run(scenario, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			binding := session.anchor.bindings.anchors["SERIAL-B"]
			binding.Containers = []hbnContainerBindingAnchor{{Role: "container", Name: "doca-hbn", ContainerID: "app-instance"}, {Role: "init", Name: "hbn-init", ContainerID: "init-instance"}, {Role: "sidecar", Name: "hbn-sidecar", ContainerID: "sidecar-instance"}}
			session.anchor.bindings.anchors["SERIAL-B"] = binding
			reads := 0
			session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
				reads++
				return runtimeBGPJSON(65101, 65000), nil
			}
			session.observe = func(context.Context) (validationCurrentCohort, error) {
				value := session.anchor
				if reads == 0 {
					return value, nil
				}
				value.bindings.anchors = maps.Clone(value.bindings.anchors)
				value.bindings.sourceDigests = maps.Clone(value.bindings.sourceDigests)
				selected := value.bindings.anchors["SERIAL-B"]
				selected.Containers = append([]hbnContainerBindingAnchor(nil), selected.Containers...)
				value.leases = append([]hostWorkloadLease(nil), value.leases...)
				switch scenario {
				case "service-values":
					selected.ServiceSpecDigest = "unretained-current-values"
				case "container-restart":
					selected.Containers[0].RestartCount++
					selected.Containers[0].ContainerID = "replacement-app"
				case "sidecar-restart":
					selected.Containers[2].RestartCount++
					selected.Containers[2].ContainerID = "replacement-sidecar"
				case "completed-init-restart":
					selected.Containers[1].RestartCount++
					selected.Containers[1].ContainerID = "replacement-init"
				case "sidecar-image":
					selected.Containers[2].ImageID = "wrong-image"
				case "pod-replacement":
					selected.PodUID = "replacement-pod"
				case "interface-replacement":
					value.workloads = map[string]string{"selected-interface": "replacement"}
				case "lease-replaced":
					value.leases[1].PoolUID = "replacement-pool"
				case "assignment-drift":
					value.leases[1].TenantNodeName = "foreign-node"
				}
				value.bindings.anchors["SERIAL-B"] = selected
				return value, nil
			}
			observed, err := session.ObserveBGP(context.Background(), "SERIAL-A")
			if err == nil || !validationIdentityChanged(err) || observed.Outcome.BGP == nil || reads != 1 || session.poisoned == nil {
				t.Fatalf("other selected source drift not retained/stopped: %+v %v", observed, err)
			}
			if _, err := session.ObserveBGP(context.Background(), "SERIAL-B"); err == nil || reads != 1 {
				t.Fatal("selected exec continued after observed drift")
			}
		})
	}
}

func TestRunAcceptanceReceiptMismatchStopsBeforeMeasurements(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	f.hosts[1].Configuration.RequestID = "wrong-binding"
	fences := fixtureAcceptanceFences(t, f, f.client, testJournal(f.prepared.execution))
	if err := fences.receipts(f.ctx); err == nil || f.commandReads != 0 || f.trafficReads != 0 {
		t.Fatalf("mismatched host transaction receipt accepted: %v", err)
	}
}
