// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type tenantWaitResource struct {
	calls int
	read  func(int) (*unstructured.Unstructured, error)
}

func (r *tenantWaitResource) Get(_ context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	r.calls++
	return r.read(r.calls)
}

type tenantWaitReconciler struct {
	digest string
	calls  int
	verify func(*unstructured.Unstructured, *unstructured.Unstructured, int) error
}

func (r *tenantWaitReconciler) BundleDigest() string { return r.digest }
func (r *tenantWaitReconciler) Ensure(context.Context, ResourceClient, *unstructured.Unstructured) (*unstructured.Unstructured, Disposition, error) {
	return nil, "", errors.New("unexpected Ensure in readiness wait")
}
func (r *tenantWaitReconciler) Verify(desired, current *unstructured.Unstructured) error {
	r.calls++
	if r.verify != nil {
		return r.verify(desired, current, r.calls)
	}
	if desired.GetName() != current.GetName() || desired.GetNamespace() != current.GetNamespace() {
		return errors.New("object identity differs")
	}
	return nil
}

type tenantWaitSourceFunc func(context.Context, TenantReadinessExpected) (tenantSources, error)

func (f tenantWaitSourceFunc) observeTenantSources(ctx context.Context, expected TenantReadinessExpected) (tenantSources, error) {
	return f(ctx, expected)
}

func TestWaitTenantReadyTransitionsPendingToReady(t *testing.T) {
	expected, initial := tenantWaitFixture(t)
	resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) { return initial.observed.DPUCluster.DeepCopy(), nil }}
	verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
	source := &tenantSourceStub{sources: initial}
	source.mutate = func(calls int, sources *tenantSources) {
		version := "Ready"
		if calls == 1 {
			version = "Provisioning"
		}
		sources.observed.TenantControlPlane.Object["status"].(map[string]any)["kubernetesResources"].(map[string]any)["version"].(map[string]any)["status"] = version
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	journal := newTenantWaitJournal(t, verifier.digest)
	got, err := waitTenantReady(ctx, resources, source, expected, initial.observed.DPUCluster, types.UID("dc-uid"), verifier, journal, time.Millisecond)
	if err != nil || got.DPUCluster == nil || got.DPUCluster.GetUID() != "dc-uid" {
		t.Fatalf("waitTenantReady() = %#v, %v", got, err)
	}
	if source.calls != 2 || resources.calls != 3 || verifier.calls != 5 {
		t.Fatalf("unexpected polling/final recheck counts: sources=%d resource=%d verify=%d", source.calls, resources.calls, verifier.calls)
	}
	events := tenantWaitEvents(t, journal)
	if len(events) != 4 || events[1].State != "attempted" || events[2].State != "waiting" || events[2].ReasonCode != "TENANT_CONTROL_PLANE_PROGRESSING" || events[3].State != "ready" {
		t.Fatalf("journal did not retain pending-to-ready sequence: %#v", events)
	}
}

func TestWaitTenantReadyRejectsDPUClusterReplacementAndIntentDrift(t *testing.T) {
	tests := []struct {
		name   string
		read   func(*unstructured.Unstructured, int) *unstructured.Unstructured
		verify func(*unstructured.Unstructured, *unstructured.Unstructured, int) error
		want   string
	}{
		{name: "uid changed before source reads", read: func(dc *unstructured.Unstructured, _ int) *unstructured.Unstructured {
			dc.SetUID("replacement")
			return dc
		}, want: "TENANT_DPUCLUSTER_REPLACED"},
		{name: "intent changed", read: func(dc *unstructured.Unstructured, _ int) *unstructured.Unstructured { return dc }, verify: func(_, _ *unstructured.Unstructured, _ int) error { return errors.New("unsafe object contents") }, want: "TENANT_DPUCLUSTER_INTENT_CHANGED"},
		{name: "uid changed on final check", read: func(dc *unstructured.Unstructured, call int) *unstructured.Unstructured {
			if call == 2 {
				dc.SetUID("replacement")
			}
			return dc
		}, want: "TENANT_DPUCLUSTER_REPLACED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected, fixture := tenantWaitFixture(t)
			resources := &tenantWaitResource{read: func(call int) (*unstructured.Unstructured, error) {
				return test.read(fixture.observed.DPUCluster.DeepCopy(), call), nil
			}}
			verifier := &tenantWaitReconciler{digest: "tenant-bundle", verify: test.verify}
			source := &tenantSourceStub{sources: fixture}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			journal := newTenantWaitJournal(t, verifier.digest)
			_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
			if err == nil || source.calls > 1 {
				t.Fatalf("wait accepted change or kept polling: err=%v source calls=%d", err, source.calls)
			}
			events := tenantWaitEvents(t, journal)
			if last := events[len(events)-1]; last.State != "failed" || last.ReasonCode != test.want {
				t.Fatalf("terminal journal event = %#v, want %s", last, test.want)
			}
		})
	}
}

func TestWaitTenantReadyHandlesMissingDeniedAndRetryableSources(t *testing.T) {
	for _, test := range []struct {
		name         string
		first        error
		wantCalls    int
		wantTerminal bool
	}{
		{name: "missing companion waits then recovers", first: apierrors.NewNotFound(schema.GroupResource{Group: "kamaji.clastix.io", Resource: "tenantcontrolplanes"}, "tenant"), wantCalls: 2},
		{name: "temporary service unavailable retries", first: apierrors.NewServiceUnavailable("api temporarily unavailable"), wantCalls: 2},
		{name: "forbidden is terminal", first: apierrors.NewForbidden(schema.GroupResource{Group: "kamaji.clastix.io", Resource: "tenantcontrolplanes"}, "tenant", errors.New("untrusted server details")), wantCalls: 1, wantTerminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected, fixture := tenantWaitFixture(t)
			resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) { return fixture.observed.DPUCluster.DeepCopy(), nil }}
			verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
			calls := 0
			source := tenantWaitSourceFunc(func(context.Context, TenantReadinessExpected) (tenantSources, error) {
				calls++
				if calls == 1 {
					return tenantSources{}, test.first
				}
				return fixture, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			journal := newTenantWaitJournal(t, verifier.digest)
			_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
			if test.wantTerminal {
				if err == nil || calls != 1 {
					t.Fatalf("forbidden source was retried: calls=%d err=%v", calls, err)
				}
				events := tenantWaitEvents(t, journal)
				if last := events[len(events)-1]; last.ReasonCode != "TENANT_SOURCE_READ_FORBIDDEN" || last.State != "failed" {
					t.Fatalf("wrong forbidden result: %#v", last)
				}
				if strings.Contains(err.Error(), "untrusted server details") {
					t.Fatal("raw API error escaped")
				}
				return
			}
			if err != nil || calls != test.wantCalls {
				t.Fatalf("source retry result: calls=%d err=%v", calls, err)
			}
			events := tenantWaitEvents(t, journal)
			if len(events) != 4 || events[2].State != "waiting" || events[3].State != "ready" {
				t.Fatalf("retry receipts = %#v", events)
			}
		})
	}
}

func TestWaitTenantReadyRejectsMalformedOrReplacedTenantSources(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*TenantReadinessObserved)
		want   string
	}{
		{name: "source DPUCluster UID changed", mutate: func(observed *TenantReadinessObserved) { observed.DPUCluster.SetUID("replacement") }, want: "TENANT_DPUCLUSTER_REPLACED"},
		{name: "tenant network spec conflicts", mutate: func(observed *TenantReadinessObserved) {
			observed.TenantControlPlane.Object["spec"].(map[string]any)["networkProfile"].(map[string]any)["address"] = "192.0.2.99"
		}, want: "TENANT_CONTROL_PLANE_SPEC_MISMATCH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected, fixture := tenantWaitFixture(t)
			resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) { return fixture.observed.DPUCluster.DeepCopy(), nil }}
			verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
			source := tenantWaitSourceFunc(func(context.Context, TenantReadinessExpected) (tenantSources, error) {
				current := cloneTenantReadinessObserved(fixture.observed)
				test.mutate(&current)
				return tenantSources{observed: current}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			journal := newTenantWaitJournal(t, verifier.digest)
			_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
			if err == nil {
				t.Fatal("accepted changed or malformed source state")
			}
			events := tenantWaitEvents(t, journal)
			if last := events[len(events)-1]; last.State != "failed" || last.ReasonCode != test.want {
				t.Fatalf("terminal event = %#v, want %s", last, test.want)
			}
		})
	}
}

func TestWaitTenantReadyDoesNotRecreateMissingDPUCluster(t *testing.T) {
	expected, fixture := tenantWaitFixture(t)
	reads, sourceCalls := 0, 0
	resources := &tenantWaitResource{read: func(int) (*unstructured.Unstructured, error) {
		reads++
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "provisioning.dpu.nvidia.com", Resource: "dpuclusters"}, expected.Name)
	}}
	source := tenantWaitSourceFunc(func(context.Context, TenantReadinessExpected) (tenantSources, error) {
		sourceCalls++
		return tenantSources{}, nil
	})
	verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	journal := newTenantWaitJournal(t, verifier.digest)
	_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
	if err == nil || reads != 1 || sourceCalls != 0 {
		t.Fatalf("missing DPUCluster was recreated/retried: reads=%d sources=%d err=%v", reads, sourceCalls, err)
	}
	events := tenantWaitEvents(t, journal)
	if last := events[len(events)-1]; last.State != "failed" || last.ReasonCode != "TENANT_DPUCLUSTER_MISSING" {
		t.Fatalf("missing cluster receipt = %#v", last)
	}
}

func TestWaitTenantReadyPersistsCancellationAndStopsOnJournalFailure(t *testing.T) {
	t.Run("cancellation records last reason", func(t *testing.T) {
		expected, fixture := tenantWaitFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) { return fixture.observed.DPUCluster.DeepCopy(), nil }}
		verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
		calls := 0
		source := tenantWaitSourceFunc(func(context.Context, TenantReadinessExpected) (tenantSources, error) {
			calls++
			cancel()
			pending := fixture
			pending.observed.TenantControlPlane = fixture.observed.TenantControlPlane.DeepCopy()
			pending.observed.TenantControlPlane.Object["status"].(map[string]any)["kubernetesResources"].(map[string]any)["version"].(map[string]any)["status"] = "Provisioning"
			return pending, nil
		})
		journal := newTenantWaitJournal(t, verifier.digest)
		_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancel result: err=%v calls=%d", err, calls)
		}
		events := tenantWaitEvents(t, journal)
		last := events[len(events)-1]
		if last.State != "failed" || last.ReasonCode != "TENANT_READINESS_CANCELED" || !strings.Contains(last.Message, "TENANT_CONTROL_PLANE_PROGRESSING") {
			t.Fatalf("cancellation was not durably classified: %#v", last)
		}
	})
	t.Run("timeout records last reason", func(t *testing.T) {
		expected, fixture := tenantWaitFixture(t)
		resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) { return fixture.observed.DPUCluster.DeepCopy(), nil }}
		verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
		source := tenantWaitSourceFunc(func(context.Context, TenantReadinessExpected) (tenantSources, error) {
			pending := fixture
			pending.observed.TenantControlPlane = fixture.observed.TenantControlPlane.DeepCopy()
			pending.observed.TenantControlPlane.Object["status"].(map[string]any)["kubernetesResources"].(map[string]any)["version"].(map[string]any)["status"] = "Provisioning"
			return pending, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		journal := newTenantWaitJournal(t, verifier.digest)
		_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Second)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout result: %v", err)
		}
		events := tenantWaitEvents(t, journal)
		last := events[len(events)-1]
		if last.State != "failed" || last.ReasonCode != "TENANT_READINESS_TIMEOUT" || !strings.Contains(last.Message, "TENANT_CONTROL_PLANE_PROGRESSING") {
			t.Fatalf("timeout did not retain last reason: %#v", last)
		}
	})
	t.Run("journal failure prevents API reads", func(t *testing.T) {
		expected, fixture := tenantWaitFixture(t)
		file := &failingJournalFile{writeErr: errors.New("disk full")}
		journal := &executionJournal{file: file, digest: "tenant-bundle"}
		reads := 0
		resources := &tenantWaitResource{read: func(_ int) (*unstructured.Unstructured, error) {
			reads++
			return fixture.observed.DPUCluster.DeepCopy(), nil
		}}
		verifier := &tenantWaitReconciler{digest: journal.digest}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := waitTenantReady(ctx, resources, &tenantSourceStub{sources: fixture}, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
		if err == nil || reads != 0 {
			t.Fatalf("read continued without a durable start receipt: err=%v reads=%d", err, reads)
		}
	})
}

func tenantWaitFixture(t *testing.T) (TenantReadinessExpected, tenantSources) {
	t.Helper()
	expected, observed := readyTenantFixture(t)
	return expected, tenantSources{observed: observed}
}

func newTenantWaitJournal(t *testing.T, digest string) *executionJournal {
	t.Helper()
	journal, err := newExecutionJournal(filepath.Join(t.TempDir(), "tenant.jsonl"), digest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.close() })
	return journal
}

func tenantWaitEvents(t *testing.T, journal *executionJournal) []executionEvent {
	t.Helper()
	if err := journal.file.Sync(); err != nil {
		t.Fatal(err)
	}
	file, ok := journal.file.(*os.File)
	if !ok {
		t.Fatal("test journal does not use a file")
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var events []executionEvent
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var event executionEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestWaitTenantReadyRechecksFinalStatusAndCancellation(t *testing.T) {
	for _, scenario := range []string{"status regresses", "canceled during final read"} {
		t.Run(scenario, func(t *testing.T) {
			expected, fixture := tenantWaitFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			resources := &tenantWaitResource{read: func(call int) (*unstructured.Unstructured, error) {
				dc := fixture.observed.DPUCluster.DeepCopy()
				if call == 2 {
					if scenario == "status regresses" {
						_ = unstructured.SetNestedField(dc.Object, "Creating", "status", "phase")
					} else {
						cancel()
					}
				}
				return dc, nil
			}}
			source := &tenantSourceStub{sources: fixture}
			verifier := &tenantWaitReconciler{digest: "tenant-bundle"}
			journal := newTenantWaitJournal(t, verifier.digest)
			_, err := waitTenantReady(ctx, resources, source, expected, fixture.observed.DPUCluster, "dc-uid", verifier, journal, time.Millisecond)
			events := tenantWaitEvents(t, journal)
			if scenario == "status regresses" {
				if err != nil || source.calls != 2 || resources.calls != 4 {
					t.Fatalf("final status regression was ignored: calls=%d/%d err=%v", source.calls, resources.calls, err)
				}
				if len(events) != 4 || events[2].State != "waiting" || events[3].State != "ready" {
					t.Fatalf("missing regression/recovery evidence: %#v", events)
				}
			} else {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
				if last := events[len(events)-1]; last.State != "failed" || last.ReasonCode != "TENANT_READINESS_CANCELED" {
					t.Fatalf("cancellation became readiness: %#v", last)
				}
			}
		})
	}
}
