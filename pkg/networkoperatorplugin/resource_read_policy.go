// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resourceReadPolicy shares existing readiness classifiers across deployment and
// standalone validation. The latter adds platform checks without apply history.
// This interface exposes no deployment or configuration effects.
type resourceReadPolicy interface {
	readiness() *crstate.Registry
	check(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error)
}

type kubernetesResourceReadPolicy struct{ registry *crstate.Registry }
type openShiftResourceReadPolicy struct{ registry *crstate.Registry }

func resourceReadPolicyFor(platform hostplatform.Selection) (resourceReadPolicy, error) {
	if err := platform.Validate(); err != nil {
		return nil, err
	}
	if platform == hostplatform.OpenShift {
		return openShiftResourceReadPolicy{registry: crstate.NewOpenShift()}, nil
	}
	return kubernetesResourceReadPolicy{registry: crstate.NewDefault()}, nil
}

func (p kubernetesResourceReadPolicy) readiness() *crstate.Registry { return p.registry }
func (p openShiftResourceReadPolicy) readiness() *crstate.Registry  { return p.registry }

func (p kubernetesResourceReadPolicy) check(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (crstate.Result, error) {
	return p.registry.Validate(ctx, c, obj)
}

func (p openShiftResourceReadPolicy) check(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (crstate.Result, error) {
	result, err := p.registry.Validate(ctx, c, obj)
	if err == nil && result.State == crstate.StateSuccess && isOCPSriovNetwork(obj) {
		result.State, result.Reason = validateOCPNetworkAttachment(ctx, c, obj)
	}
	return result, err
}

// Operator configuration Kinds retain their specialized validation on either
// platform, including the existing classification of Get failures as missing.
func validateOperatorConfiguration(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (crstate.CRState, string) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, live); err != nil {
		return crstate.StateNotDeployed, err.Error()
	}
	merged := live.DeepCopy()
	if err := mergeOCPConfiguration(merged, obj); err != nil {
		return crstate.StateError, err.Error()
	}
	if !reflect.DeepEqual(merged.Object, live.Object) {
		return crstate.StateError, "operator configuration differs from generated required fields"
	}
	return crstate.StateSuccess, "required operator configuration matches"
}
