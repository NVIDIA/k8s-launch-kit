// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitForOCPCreatedConfigurationDeletion(ctx context.Context, c client.Client, target *unstructured.Unstructured) error {
	err := wait.PollUntilContextCancel(ctx, defaultCleanPollInterval, true, func(ctx context.Context) (bool, error) {
		current := target.DeepCopy()
		err := c.Get(ctx, client.ObjectKeyFromObject(target), current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return current.GetUID() != target.GetUID(), nil
	})
	if err != nil {
		return fmt.Errorf("wait for OpenShift %s %s finalizers: %w; retry clean with the same bundle after inspecting its controller", target.GetKind(), client.ObjectKeyFromObject(target), err)
	}
	return nil
}

// The restore marker remains on the Subscription until OLM has adopted its env
// and the actual managed Deployments have rolled out. Interrupted waits recheck
// this marker on retry even though the field deltas were already restored.
func completeOCPSubscriptionRestore(ctx context.Context, c client.Client, target *unstructured.Unstructured, cohort string) error {
	original, err := readOCPConfigurationJournal(target)
	if err != nil {
		return err
	}
	if original == nil || len(original.RestoreEnv) == 0 {
		return fmt.Errorf("cannot restore Subscription without a durable pending marker")
	}
	expected := original.RestoreEnv
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, defaultCleanPollInterval, true, func(ctx context.Context) (bool, error) {
		sub := target.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(target), sub); err != nil {
			return false, err
		}
		journal, err := readOCPConfigurationJournal(sub)
		if err != nil {
			return false, err
		}
		if sub.GetUID() != target.GetUID() || journal == nil || !reflect.DeepEqual(journal.RestoreEnv, expected) {
			return false, fmt.Errorf("the Subscription incarnation or pending restoration provenance changed; inspect it with its owner")
		}
		if !containsOCPString(journal.Cohorts, cohort) {
			return false, fmt.Errorf("the Subscription restoration claim changed")
		}
		if err := ocpSubscriptionRestoreStillDesired(sub, journal.RestoreEnv); err != nil {
			return false, err
		}
		csv, err := ocpRestoreSubscriptionCSV(ctx, c, sub)
		if err != nil {
			return false, err
		}
		deployments, _, err := unstructured.NestedSlice(csv.Object, "spec", "install", "spec", "deployments")
		if err != nil {
			return false, err
		}
		if len(deployments) == 0 {
			return false, fmt.Errorf("network operator CSV has no managed deployments")
		}
		for _, entry := range deployments {
			definition, ok := entry.(map[string]interface{})
			if !ok {
				return false, fmt.Errorf("invalid CSV deployment definition")
			}
			name, _ := definition["name"].(string)
			if name == "" {
				return false, fmt.Errorf("unnamed CSV deployment definition")
			}
			deployment := &unstructured.Unstructured{}
			deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
			if err := c.Get(ctx, client.ObjectKey{Namespace: sub.GetNamespace(), Name: name}, deployment); apierrors.IsNotFound(err) {
				return false, nil
			} else if err != nil {
				return false, err
			}
			adopted, err := ocpDeploymentRestoredEnv(deployment, definition, journal.RestoreEnv)
			if err != nil {
				return false, err
			}
			if !adopted || !deploymentRolledOut(deployment) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("OpenShift Subscription %s restoration rollout unverified: %w; rollback provenance is retained, inspect OLM/controllers and retry clean with the same bundle", client.ObjectKeyFromObject(target), err)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sub := target.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(target), sub); err != nil {
			return err
		}
		journal, err := readOCPConfigurationJournal(sub)
		if err != nil {
			return err
		}
		if sub.GetUID() != target.GetUID() || journal == nil || !reflect.DeepEqual(journal.RestoreEnv, expected) || !containsOCPString(journal.Cohorts, cohort) {
			return fmt.Errorf("the Subscription incarnation or restoration provenance changed before completion; inspect it with its owner")
		}
		if err := ocpSubscriptionRestoreStillDesired(sub, journal.RestoreEnv); err != nil {
			return err
		}
		before := sub.DeepCopy()
		journal.RestoreEnv = nil
		if len(journal.Fields) == 0 {
			journal = nil
		}
		if err := writeOCPConfigurationJournal(sub, journal); err != nil {
			return err
		}
		return c.Patch(ctx, sub, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}
func ocpSubscriptionRestoreStillDesired(sub *unstructured.Unstructured, expected map[string]ocpConfigurationValue) error {
	view, err := ocpConfigurationView(sub)
	if err != nil {
		return err
	}
	for name, want := range expected {
		have, err := ocpConfigurationValueAt(view, []string{"env", name})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(have, want) {
			return fmt.Errorf("recorded Subscription environment %s changed during restoration; resolve with its owner before retrying", name)
		}
	}
	return nil
}
func ocpDeploymentRestoredEnv(deployment *unstructured.Unstructured, csvDeployment map[string]interface{}, expected map[string]ocpConfigurationValue) (bool, error) {
	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return false, err
	}
	if len(containers) == 0 {
		return false, nil
	}
	defaults, _, err := unstructured.NestedSlice(csvDeployment, "spec", "template", "spec", "containers")
	if err != nil {
		return false, err
	}
	for _, entry := range containers {
		container, ok := entry.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid operator Deployment container")
		}
		for name, setting := range expected {
			want := setting
			if !setting.Present {
				// Removing an OLM override restores the CSV's per-container default,
				// which can itself define the same variable. Absence is not always right.
				foundContainer := false
				for _, entry := range defaults {
					original, ok := entry.(map[string]interface{})
					if !ok {
						return false, fmt.Errorf("invalid CSV container")
					}
					if original["name"] == container["name"] {
						foundContainer = true
						want = ocpContainerEnv(original, name)
					}
				}
				if !foundContainer {
					return false, fmt.Errorf("CSV has no environment baseline for operator container")
				}
			}
			have := ocpContainerEnv(container, name)
			if have.Present != want.Present {
				return false, nil
			}
			if have.Present {
				a, aok := have.Value.(map[string]interface{})
				b, bok := want.Value.(map[string]interface{})
				if !aok || !bok {
					return false, fmt.Errorf("invalid restored environment value")
				}
				av, _ := a["value"].(string)
				bv, _ := b["value"].(string)
				if av != bv || !reflect.DeepEqual(a["valueFrom"], b["valueFrom"]) {
					return false, nil
				}
			}
		}
	}
	return true, nil
}
func ocpContainerEnv(container map[string]interface{}, name string) ocpConfigurationValue {
	env, _ := container["env"].([]interface{})
	for _, entry := range env {
		item, ok := entry.(map[string]interface{})
		if ok && item["name"] == name {
			return ocpConfigurationValue{Present: true, Value: item}
		}
	}
	return ocpConfigurationValue{}
}

func ocpRestoreSubscriptionCSV(ctx context.Context, c client.Client, sub *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	name, _, err := unstructured.NestedString(sub.Object, "status", "installedCSV")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(name, "nvidia-network-operator.") {
		return nil, fmt.Errorf("cannot restore settings because the Subscription has no valid installed Network Operator CSV")
	}
	csv := &unstructured.Unstructured{}
	csv.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "ClusterServiceVersion"})
	if err := c.Get(ctx, client.ObjectKey{Namespace: sub.GetNamespace(), Name: name}, csv); err != nil {
		return nil, err
	}
	return csv, nil
}
