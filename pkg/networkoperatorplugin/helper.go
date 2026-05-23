// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"time"

	netop "github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// waitHelperDefaultTimeout caps `WaitNicClusterPolicyReady` /
// `WaitNicNodePolicyReady` when the caller doesn't supply a deadline.
// Only applies to the helper / discovery path — the deploy state machine
// in ApplyManifestsFromDir is bounded solely by ctx.Deadline().
const waitHelperDefaultTimeout = 15 * time.Minute

// EnsureNicClusterPolicy checks for existing NicClusterPolicies and creates the provided one if none exist.
// If policies already exist, returns them without creating a new one and without error.
// If no policies exist, creates the provided policy, waits for it to become ready, and returns nil.
func EnsureNicClusterPolicy(ctx context.Context, c client.Client, policy *netop.NicClusterPolicy) ([]netop.NicClusterPolicy, error) {
	list := &netop.NicClusterPolicyList{}
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	if len(list.Items) > 0 {
		return list.Items, nil
	}

	if err := c.Create(ctx, policy); err != nil {
		return nil, err
	}

	return nil, WaitNicClusterPolicyReady(ctx, c, policy.Name)
}

// DeleteNicClusterPolicies deletes the given NicClusterPolicies from the cluster.
func DeleteNicClusterPolicies(ctx context.Context, c client.Client, policies []netop.NicClusterPolicy) error {
	for i := range policies {
		if err := c.Delete(ctx, &policies[i]); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete existing NicClusterPolicy %q: %w", policies[i].Name, err)
			}
		}
		log.Log.Info("Deleted existing NicClusterPolicy for discovery", "name", policies[i].Name)
	}
	return nil
}

// RestoreNicClusterPolicies re-creates previously saved NicClusterPolicies on the cluster.
func RestoreNicClusterPolicies(ctx context.Context, c client.Client, policies []netop.NicClusterPolicy) error {
	for i := range policies {
		// Clear server-set fields so the object can be recreated
		policies[i].ResourceVersion = ""
		policies[i].UID = ""
		policies[i].Generation = 0
		policies[i].CreationTimestamp = metav1.Time{}
		policies[i].ManagedFields = nil
		policies[i].Status = netop.NicClusterPolicyStatus{}
		if err := c.Create(ctx, &policies[i]); err != nil {
			return fmt.Errorf("failed to restore NicClusterPolicy %q: %w", policies[i].Name, err)
		}
		log.Log.Info("Restored NicClusterPolicy after discovery", "name", policies[i].Name)
	}
	return nil
}

// WaitNicClusterPolicyReady polls NicClusterPolicy via the crstate
// registry until it reaches StateSuccess or StateError, with a timeout.
func WaitNicClusterPolicyReady(parentCtx context.Context, c client.Client, name string) error {
	return waitViaRegistry(parentCtx, c, schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicClusterPolicy",
	}, "", name, "NIC Cluster Policy")
}

// WaitNicNodePolicyReady polls NicNodePolicy via the crstate registry
// until it reaches StateSuccess or StateError, with a timeout.
func WaitNicNodePolicyReady(parentCtx context.Context, c client.Client, name string) error {
	return waitViaRegistry(parentCtx, c, schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicNodePolicy",
	}, "", name, fmt.Sprintf("NIC Node Policy %q", name))
}

// waitViaRegistry polls the registry's per-Kind validator until it reports
// a terminal state. The function never re-applies; absent objects are
// treated as in-progress until the timeout. Use ApplyManifestsFromDir's
// state machine when you also need apply-on-not-deployed semantics.
//
// When parentCtx has no deadline, a conservative 15-minute default is
// applied — this helper is also called from the non-deploy `discover`
// path where a bounded wait keeps us from hanging forever on a broken
// cluster. Callers that want an unbounded (or maintenance-window-sized)
// budget should wrap their own ctx via context.WithTimeout before
// invoking and the helper will honor it.
func waitViaRegistry(parentCtx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name, label string) error {
	uiOutput := ui.FromContext(parentCtx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Waiting for %s to become ready", label))

	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, waitHelperDefaultTimeout)
		defer cancel()
	}

	registry := crstate.NewDefault()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)

	ticker := time.NewTicker(deployPollInterval)
	defer ticker.Stop()

	for {
		res, err := registry.Validate(ctx, c, obj)
		if err != nil {
			log.Log.V(1).Info("validate transient failure", "kind", gvk.Kind, "name", name, "error", err.Error())
		} else {
			switch res.State {
			case crstate.StateSuccess:
				progress.Success(fmt.Sprintf("%s is ready", label))
				log.Log.Info("manifest is ready", "kind", gvk.Kind, "name", name)
				return nil
			case crstate.StateError:
				progress.Fail(fmt.Sprintf("%s error: %s", label, res.Reason))
				return fmt.Errorf("%s %q in error state: %s", gvk.Kind, name, res.Reason)
			case crstate.StateInProgress, crstate.StateNotDeployed:
				if res.Reason != "" {
					progress.Update(fmt.Sprintf("%s: %s", label, res.Reason))
				}
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail(fmt.Sprintf("Timeout waiting for %s", label))
			return fmt.Errorf("timeout waiting for %s %q to become ready", gvk.Kind, name)
		case <-ticker.C:
		}
	}
}

// DeleteNicClusterPolicy deletes the NicClusterPolicy by name, ignoring NotFound errors.
func DeleteNicClusterPolicy(ctx context.Context, c client.Client, name string) error {
	obj := &netop.NicClusterPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// PatchNicConfigOperatorIntoPolicy patches an existing NicClusterPolicy to add
// the NicConfigurationOperator section via server-side apply, preserving all
// other fields (ofedDriver, secondaryNetwork, etc.).
func PatchNicConfigOperatorIntoPolicy(ctx context.Context, c client.Client, policyName string, nicConfigOp *netop.NicConfigurationOperatorSpec) error {
	// Build a minimal apply-configuration containing only the field we want to own.
	patch := &netop.NicClusterPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "mellanox.com/v1alpha1",
			Kind:       "NicClusterPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: netop.NicClusterPolicySpec{
			NicConfigurationOperator: nicConfigOp,
		},
	}

	return c.Patch(ctx, patch, client.Apply, client.FieldOwner("l8k-discovery"), client.ForceOwnership)
}

// RemoveNicConfigOperatorFromPolicy removes the NicConfigurationOperator section
// that was added during discovery by fetching the policy and updating it.
func RemoveNicConfigOperatorFromPolicy(ctx context.Context, c client.Client, policyName string) error {
	policy := &netop.NicClusterPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get NicClusterPolicy %q for cleanup: %w", policyName, err)
	}

	policy.Spec.NicConfigurationOperator = nil
	if err := c.Update(ctx, policy); err != nil {
		return fmt.Errorf("failed to remove NicConfigurationOperator from %q: %w", policyName, err)
	}

	log.Log.Info("Removed NicConfigurationOperator from NicClusterPolicy after discovery", "name", policyName)
	return nil
}
