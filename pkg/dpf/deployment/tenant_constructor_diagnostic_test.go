// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type identityJournalHook struct {
	managementMemoryFile
	hook      func()
	triggered bool
}

func (f *identityJournalHook) Write(data []byte) (int, error) {
	n, err := f.managementMemoryFile.Write(data)
	var event executionEvent
	if json.Unmarshal(data, &event) == nil && event.ReasonCode == "TENANT_API_INSPECTION_STARTED" {
		f.triggered = true
		f.hook()
	}
	return n, err
}

func TestRetainedTenantConstructorPreservesProducedIdentityDiagnostic(t *testing.T) {
	state := &hostLeaseManagementState{}
	unavailable := atomic.Bool{}
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","reason":"ServiceUnavailable","code":503,"message":"private-constructor-canary"}`))
			return
		}
		if r.URL.Path == "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1" {
			writeObservationJSON(w, &metav1.APIResourceList{GroupVersion: "provisioning.dpu.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Name: "dpuclusters", Kind: "DPUCluster", Namespaced: true}}})
			return
		}
		state.serveManagement(w, r)
	})
	plan, reconciler := prepareHostLeaseObservationPlan(t, api)
	for _, scenario := range []string{"cancel", "unavailable", "UID drift"} {
		t.Run(scenario, func(t *testing.T) {
			unavailable.Store(false)
			harness := newHostLeaseObservationHarnessFromPlan(t, state, api, plan, reconciler)
			_, desired, err := tenantPlanInputs(plan)
			if err != nil {
				t.Fatal(err)
			}
			current, err := reconciler.markedDesired(desired)
			if err != nil {
				t.Fatal(err)
			}
			state.mu.Lock()
			old := state.sources.observed.DPUCluster
			current.SetUID(old.GetUID())
			current.SetResourceVersion("1")
			current.SetGeneration(old.GetGeneration())
			current.Object["status"] = old.DeepCopy().Object["status"]
			state.sources.observed.DPUCluster = current
			state.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			file := &identityJournalHook{hook: func() {
				switch scenario {
				case "cancel":
					cancel()
				case "unavailable":
					unavailable.Store(true)
				case "UID drift":
					state.mu.Lock()
					state.sources.observed.DPUCluster.SetUID("replacement")
					for _, object := range state.sources.observed.Keepalived {
						setOwnerReference(t, object, current.GetName(), "replacement")
					}
					setOwnerReference(t, state.sources.observed.TenantControlPlane, current.GetName(), "replacement")
					state.mu.Unlock()
				}
			}}
			journal := &executionJournal{file: file, digest: plan.Digest()}
			_, err = connectRetainedTenant(ctx, harness.management, plan, current.GetUID(), reconciler, journal, time.Millisecond, time.Second)
			want := "TENANT_API_IDENTITY_CHANGED"
			switch scenario {
			case "cancel":
				want = "TENANT_API_IDENTITY_CANCELED"
			case "unavailable":
				want = "TENANT_API_IDENTITY_UNVERIFIED"
			}
			if !file.triggered || err == nil || !strings.Contains(file.String(), `"reasonCode":"`+want+`"`) || strings.Contains(file.String()+err.Error(), "private-constructor-canary") {
				t.Fatalf("hit=%v err=%v journal=%s", file.triggered, err, file.String())
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation")
			}
			if api.writes.Load() != 0 {
				t.Fatal("constructor failure wrote to management")
			}
		})
	}
}
