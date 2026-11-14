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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type resourceReadFunc func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)

func (f resourceReadFunc) Get(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return f(ctx, desired)
}

func waitConfig(ready bool) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.dpu.nvidia.com/v1alpha1", "kind": "DPFOperatorConfig",
		"metadata": map[string]any{"name": "dpfoperatorconfig", "namespace": "dpf", "uid": "config-uid", "generation": int64(1)},
		"status":   map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": status, "observedGeneration": int64(1), "reason": "Reconciling"}}},
	}}
}

func TestWaitNativeReadyRechecksIntentAndLiveStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reads, verifies := 0, 0
	var outcomes []Readiness
	client := resourceReadFunc(func(_ context.Context, input *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		reads++
		if input.GetName() != "dpfoperatorconfig" {
			t.Fatal("desired object was changed by a previous read")
		}
		input.SetName("mutation")
		return waitConfig(reads >= 2), nil
	})
	obj, err := waitNativeReady(ctx, client, waitConfig(false), "config-uid", func(desired, current *unstructured.Unstructured) error {
		verifies++
		desired.SetName("mutation")
		current.SetUID("mutation")
		return nil
	}, time.Millisecond, func(state Readiness, _ bool) error { outcomes = append(outcomes, state); return nil })
	if err != nil || reads != 2 || verifies != 2 || obj == nil || obj.GetUID() != "config-uid" || len(outcomes) != 2 || outcomes[0].Ready || !outcomes[1].Ready {
		t.Fatalf("wait result: obj=%v err=%v reads=%d verifies=%d outcomes=%+v", obj, err, reads, verifies, outcomes)
	}
}

func TestWaitNativeReadyRejectsReplacementAndIntentDrift(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		current := waitConfig(true)
		if replacement {
			current.SetUID("replacement-uid")
		}
		var outcome Readiness
		obj, err := waitNativeReady(ctx, resourceReadFunc(func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			return current, nil
		}), waitConfig(false), "config-uid",
			func(*unstructured.Unstructured, *unstructured.Unstructured) error {
				return errors.New("secret-data-from-live-object")
			}, time.Millisecond,
			func(state Readiness, _ bool) error { outcome = state; return nil })
		cancel()
		if err == nil || obj != nil || outcome.Ready || strings.Contains(err.Error(), "secret-data") {
			t.Fatalf("accepted replacement/drift or leaked object data: %v %v %+v", obj, err, outcome)
		}
	}
}

func TestWaitNativeReadyRetriesOnlyTemporaryReadFailure(t *testing.T) {
	for _, transient := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		reads := 0
		client := resourceReadFunc(func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			reads++
			if reads == 1 {
				if transient {
					return nil, apierrors.NewTooManyRequests("secret response", 0)
				}
				return nil, apierrors.NewNotFound(schema.GroupResource{Group: "operator.dpu.nvidia.com", Resource: "dpfoperatorconfigs"}, "dpfoperatorconfig")
			}
			return waitConfig(true), nil
		})
		_, err := waitNativeReady(ctx, client, waitConfig(false), "config-uid", func(*unstructured.Unstructured, *unstructured.Unstructured) error { return nil }, time.Millisecond, func(Readiness, bool) error { return nil })
		cancel()
		if transient && (err != nil || reads != 2) || !transient && (err == nil || reads != 1) {
			t.Fatalf("unexpected retry: transient=%v reads=%d err=%v", transient, reads, err)
		}
	}
}

func TestWaitNativeReadyStopsOnReportFailureAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		reads := 0
		reportFailure := errors.New("report disk full")
		var states []string
		_, err := waitNativeReady(ctx, resourceReadFunc(func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			reads++
			return waitConfig(false), nil
		}), waitConfig(false), "config-uid", func(*unstructured.Unstructured, *unstructured.Unstructured) error { return nil }, time.Hour, func(state Readiness, _ bool) error {
			states = append(states, state.ReasonCode)
			if canceled {
				cancel()
				return nil
			}
			return reportFailure
		})
		cancel()
		if reads != 1 || canceled && (!errors.Is(err, context.Canceled) || states[len(states)-1] != "READINESS_CANCELED") || !canceled && !errors.Is(err, reportFailure) {
			t.Fatalf("wait failed to stop: reads=%d err=%v states=%v", reads, err, states)
		}
	}
}

func TestWaitNativeReadyStopsOnTerminalControllerOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(*unstructured.Unstructured)
	}{
		{name: "controller failure", code: "RESOURCE_TERMINAL_FAILURE", mutate: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["reason"] = "Failed"
		}},
		{name: "deleting", code: "RESOURCE_DELETING", mutate: func(o *unstructured.Unstructured) {
			o.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-14T00:00:00Z"
		}},
		{name: "invalid status", code: "RESOURCE_STATUS_INVALID", mutate: func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]any)["observedGeneration"] = int64(2)
		}},
		{name: "deployment rollout failed", code: "DEPLOYMENT_ROLLOUT_FAILED", mutate: func(o *unstructured.Unstructured) {
			o.SetAPIVersion("apps/v1")
			o.SetKind("Deployment")
			o.Object["spec"] = map[string]any{"replicas": int64(1)}
			o.Object["status"] = map[string]any{"observedGeneration": int64(1), "updatedReplicas": int64(1), "readyReplicas": int64(0), "availableReplicas": int64(0), "conditions": []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			current := waitConfig(false)
			tc.mutate(current)
			reads, observations := 0, 0
			obj, err := waitNativeReady(ctx, resourceReadFunc(func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				reads++
				return current.DeepCopy(), nil
			}), current.DeepCopy(), "config-uid", func(*unstructured.Unstructured, *unstructured.Unstructured) error { return nil }, time.Hour,
				func(state Readiness, terminal bool) error {
					observations++
					if !terminal || state.Ready || state.ReasonCode != tc.code {
						t.Errorf("incorrect terminal result: %+v, terminal=%v", state, terminal)
					}
					return nil
				})
			if obj != nil || err == nil || errors.Is(err, context.DeadlineExceeded) || reads != 1 || observations != 1 {
				t.Fatalf("terminal outcome was retried or lost: obj=%v err=%v reads=%d observations=%d", obj, err, reads, observations)
			}
		})
	}
}

func TestSoftwareWaitTracksConditionsAndStopsOnTerminalReason(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal-%v", terminal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			reads := 0
			current := func(complete bool) *unstructured.Unstructured {
				status, reason := "False", "Reconciling"
				if complete {
					status, reason = "True", "Success"
				}
				if terminal {
					status, reason = "False", "Failed"
				}
				obj := dpfObject("BlueFieldSoftware", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Downloaded", status, reason, 2), conditionMap("Ready", "True", "Success", 2)}})
				obj.SetUID("software-uid")
				return obj
			}
			client := resourceReadFunc(func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				reads++
				return current(reads > 1), nil
			})
			_, err := waitNativeReady(ctx, client, current(false), "software-uid", func(*unstructured.Unstructured, *unstructured.Unstructured) error { return nil }, time.Millisecond, func(Readiness, bool) error { return nil })
			if terminal {
				if err == nil || reads != 1 || !strings.Contains(err.Error(), "RESOURCE_TERMINAL_FAILURE") || !strings.Contains(err.Error(), "Controller reason: Failed") {
					t.Fatalf("terminal wait: reads=%d err=%v", reads, err)
				}
			} else if err != nil || reads != 2 {
				t.Fatalf("pending wait: reads=%d err=%v", reads, err)
			}
		})
	}
}
