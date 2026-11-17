// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type resourceReader interface {
	Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// waitNativeReady refreshes a resource after its successful create-or-verify.
// It never creates a disappeared object, accepts a replacement UID, or trusts
// Ready without verifying retained intent. The caller must separately verify
// companion resources and data-plane behavior; notably DPUCluster Ready alone
// cannot establish tenant convergence.
func waitNativeReady(ctx context.Context, client resourceReader, desired *unstructured.Unstructured, uid types.UID,
	verify func(*unstructured.Unstructured, *unstructured.Unstructured) error, interval time.Duration,
	observe func(Readiness, bool) error,
) (*unstructured.Unstructured, error) {
	if ctx == nil || client == nil || desired == nil || uid == "" || verify == nil || observe == nil || interval <= 0 {
		return nil, fmt.Errorf("readiness wait requires a context, resource, established UID, intent verifier, positive interval and result recorder")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("readiness wait requires an execution deadline")
	}
	wanted := desired.DeepCopy()
	last := Readiness{ReasonCode: "RESOURCE_NOT_OBSERVED", Message: "The resource has not been observed during this wait."}
	for {
		if err := ctx.Err(); err != nil {
			return nil, finishReadinessWait(observe, last, err)
		}
		current, err := client.Get(ctx, wanted.DeepCopy())
		if err != nil {
			if ctx.Err() != nil {
				return nil, finishReadinessWait(observe, last, ctx.Err())
			}
			if !retryableRead(err) {
				state := result("RESOURCE_READ_FAILED", "The required resource could not be read; no replacement will be created.")
				return nil, errors.Join(observe(state, true), safeAPIError("observe required resource", err))
			}
			last = result("RESOURCE_READ_RETRY", "The API is temporarily unavailable or throttled; readiness remains unverified.")
		} else {
			if current == nil || current.GetUID() != uid {
				state := result("RESOURCE_IDENTITY_CHANGED", "The resource identity changed during the wait; stop and inspect the replacement.")
				return nil, errors.Join(observe(state, true), fmt.Errorf("required resource identity changed"))
			}
			if err := verify(wanted.DeepCopy(), current.DeepCopy()); err != nil {
				state := result("RESOURCE_INTENT_CHANGED", "The live resource no longer matches the retained deployment intent or ownership.")
				// A verifier may include an object value in its error. Persist only
				// the bounded classification, never arbitrary resource contents.
				return nil, errors.Join(observe(state, true), fmt.Errorf("required resource intent or ownership differs"))
			}
			last = EvaluateReadiness(current)
			switch last.ReasonCode {
			case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED", "DEPLOYMENT_ROLLOUT_FAILED":
				return nil, errors.Join(observe(last, true), fmt.Errorf("%s: %s", last.ReasonCode, last.Message))
			}
			if last.Ready {
				if err := ctx.Err(); err != nil {
					return nil, finishReadinessWait(observe, last, err)
				}
				if err := observe(last, true); err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, finishReadinessWait(observe, last, err)
				}
				return current.DeepCopy(), nil
			}
		}
		if err := observe(last, false); err != nil {
			return nil, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, finishReadinessWait(observe, last, ctx.Err())
		case <-timer.C:
		}
	}
}

func retryableRead(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err)
}

func finishReadinessWait(observe func(Readiness, bool) error, last Readiness, cause error) error {
	code, message := "READINESS_TIMEOUT", "Readiness was not established before the execution deadline."
	if errors.Is(cause, context.Canceled) {
		code, message = "READINESS_CANCELED", "The readiness wait was canceled."
	}
	err := observe(result(code, message), true)
	return errors.Join(err, fmt.Errorf("%s; last check: %s: %w", message, last.ReasonCode, cause))
}
