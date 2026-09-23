// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yaml "sigs.k8s.io/yaml"
)

func isOCPOperatorConfig(obj *unstructured.Unstructured) bool {
	switch obj.GroupVersionKind().Group + "/" + obj.GetKind() {
	case "sriovnetwork.openshift.io/SriovOperatorConfig", "maintenance.nvidia.com/MaintenanceOperatorConfig", "nfd.openshift.io/NodeFeatureDiscovery":
		return true
	}
	return false
}

// applyOCPOperatorConfiguration changes only fields rendered by l8k. The
// Subscription and pre-existing configuration objects keep unrelated fields.
func applyOCPOperatorConfiguration(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, docs [][]byte, dryRun bool) error {
	if cfg.NetworkOperator == nil {
		return fmt.Errorf("networkOperator config is required")
	}
	operatorNamespace := cfg.NetworkOperator.Namespace
	if operatorNamespace == "" {
		operatorNamespace = "nvidia-network-operator"
	}
	maintenanceNamespace := config.DefaultMaintenanceOperatorNamespace
	if cfg.Maintenance != nil && cfg.Maintenance.OperatorNamespace != "" {
		maintenanceNamespace = cfg.Maintenance.OperatorNamespace
	}
	// A CSV without a Subscription has no durable OLM configuration channel.
	// Check this before applying any of the other operator configuration CRs.
	sub, err := findOCPOperatorSubscription(ctx, c, ocpOperator{
		packageName: "nvidia-network-operator", namespace: operatorNamespace,
	})
	if err != nil {
		return err
	}
	if sub == nil {
		return fmt.Errorf("network operator Subscription is required to configure OpenShift maintenance settings")
	}
	// Driver maintenance coordination is supported by the certified Network
	// Operator's Subscription deployment config. SR-IOV retains native draining.
	operatorEnv := map[string]string{
		"MAINTENANCE_OPERATOR_ENABLED":             "true",
		"MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": maintenanceNamespace,
	}
	for _, doc := range docs {
		desired, err := decodeUnstructured(doc)
		if err != nil {
			return err
		}
		if !isOCPOperatorConfig(desired) {
			return fmt.Errorf("unexpected OpenShift operator configuration %s", desired.GetKind())
		}
		key := types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &unstructured.Unstructured{}
			current.SetGroupVersionKind(desired.GroupVersionKind())
			err := c.Get(ctx, key, current)
			if apierrors.IsNotFound(err) {
				opts := []client.CreateOption{}
				if dryRun {
					opts = append(opts, client.DryRunAll)
				}
				return c.Create(ctx, desired.DeepCopy(), opts...)
			}
			if err != nil {
				return err
			}
			before := current.DeepCopy()
			if err := mergeOCPSpec(current, desired); err != nil {
				return err
			}
			if reflect.DeepEqual(before.Object, current.Object) {
				return nil
			}
			opts := []client.UpdateOption{}
			if dryRun {
				opts = append(opts, client.DryRunAll)
			}
			return c.Update(ctx, current, opts...)
		})
		if err != nil {
			return fmt.Errorf("configure %s %s/%s: %w", desired.GetKind(), key.Namespace, key.Name, err)
		}
	}
	changed, err := mergeSubscriptionEnvChanged(ctx, c, operatorNamespace, operatorEnv, dryRun)
	if err != nil {
		return err
	}
	if changed && dryRun {
		ui.FromContext(ctx).Warning("OpenShift Network Operator Subscription rollout is deferred in dry-run")
	}
	if dryRun {
		return nil
	}
	// A retry must also wait when a previous invocation updated the
	// Subscription but ended before OLM adopted its environment.
	return waitForNetworkOperatorSubscriptionRollout(ctx, c, operatorNamespace, operatorEnv)
}

func mergeOCPSpec(current, desired *unstructured.Unstructured) error {
	want, _, _ := unstructured.NestedMap(desired.Object, "spec")
	have, _, _ := unstructured.NestedMap(current.Object, "spec")
	if have == nil {
		have = map[string]interface{}{}
	}
	if desired.GetKind() == "NodeFeatureDiscovery" {
		desiredWorker, _, _ := unstructured.NestedMap(want, "workerConfig")
		existingWorker, _, _ := unstructured.NestedMap(have, "workerConfig")
		if existingWorker == nil {
			existingWorker = map[string]interface{}{}
		}
		desiredData, _ := desiredWorker["configData"].(string)
		existingData, _ := existingWorker["configData"].(string)
		merged, err := mergeNFDConfigData(existingData, desiredData)
		if err != nil {
			return err
		}
		existingWorker["configData"] = merged
		have["workerConfig"] = existingWorker
	} else {
		for k, v := range want {
			have[k] = v
		}
	}
	return unstructured.SetNestedMap(current.Object, have, "spec")
}

func mergeNFDConfigData(existing, desired string) (string, error) {
	var have, want map[string]interface{}
	if existing != "" {
		if err := yaml.Unmarshal([]byte(existing), &have); err != nil {
			return "", fmt.Errorf("decode existing NFD configData: %w", err)
		}
	}
	if err := yaml.Unmarshal([]byte(desired), &want); err != nil {
		return "", fmt.Errorf("decode desired NFD configData: %w", err)
	}
	if have == nil {
		have = map[string]interface{}{}
	}
	wantedSources, _ := want["sources"].(map[string]interface{})
	existingSources, _ := have["sources"].(map[string]interface{})
	if existingSources == nil {
		existingSources = map[string]interface{}{}
	}
	wantedPCI, _ := wantedSources["pci"].(map[string]interface{})
	existingPCI, _ := existingSources["pci"].(map[string]interface{})
	if existingPCI == nil {
		existingPCI = map[string]interface{}{}
	}
	for k, v := range wantedPCI {
		if k == "deviceClassWhitelist" || k == "deviceLabelFields" {
			merged, err := mergeNFDStringList(existingPCI[k], v)
			if err != nil {
				return "", fmt.Errorf("merge NFD sources.pci.%s: %w", k, err)
			}
			existingPCI[k] = merged
			continue
		}
		existingPCI[k] = v
	}
	existingSources["pci"] = existingPCI
	have["sources"] = existingSources
	out, err := yaml.Marshal(have)
	return string(out), err
}

func mergeNFDStringList(existing, desired interface{}) ([]interface{}, error) {
	merged := []interface{}{}
	seen := map[string]bool{}
	for _, value := range []interface{}{existing, desired} {
		if value == nil {
			continue
		}
		list, ok := value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("expected a list, got %T", value)
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected a string, got %T", item)
			}
			if !seen[name] {
				merged = append(merged, name)
				seen[name] = true
			}
		}
	}
	return merged, nil
}

func mergeSubscriptionEnv(ctx context.Context, c client.Client, namespace string, desired map[string]string, dryRun bool) error {
	_, err := mergeSubscriptionEnvChanged(ctx, c, namespace, desired, dryRun)
	return err
}

func mergeSubscriptionEnvChanged(ctx context.Context, c client.Client, namespace string, desired map[string]string, dryRun bool) (bool, error) {
	if namespace == "" {
		namespace = "nvidia-network-operator"
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changed = false
		sub, err := findOCPOperatorSubscription(ctx, c, ocpOperator{packageName: "nvidia-network-operator", namespace: namespace})
		if err != nil {
			return err
		}
		if sub == nil {
			return fmt.Errorf("existing Network Operator Subscription required in %s", namespace)
		}
		env, _, _ := unstructured.NestedSlice(sub.Object, "spec", "config", "env")
		for name, value := range desired {
			found := false
			for i, entry := range env {
				item, ok := entry.(map[string]interface{})
				if !ok {
					continue
				}
				if item["name"] != name {
					continue
				}
				found = true
				if item["value"] != value || item["valueFrom"] != nil {
					delete(item, "valueFrom")
					item["value"] = value
					env[i] = item
					changed = true
				}
			}
			if !found {
				env = append(env, map[string]interface{}{"name": name, "value": value})
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if err := unstructured.SetNestedSlice(sub.Object, env, "spec", "config", "env"); err != nil {
			return err
		}
		opts := []client.UpdateOption{}
		if dryRun {
			opts = append(opts, client.DryRunAll)
		}
		return c.Update(ctx, sub, opts...)
	})
	return changed, err
}

// waitForNetworkOperatorSubscriptionRollout waits only after a real
// Subscription change. OLM propagates spec.config.env to its managed
// Deployment asynchronously, so a successful Subscription update alone does
// not mean the operator has adopted the requested maintenance settings.
func waitForNetworkOperatorSubscriptionRollout(ctx context.Context, c client.Client, namespace string, desired map[string]string) error {
	if namespace == "" {
		namespace = "nvidia-network-operator"
	}
	csv, err := installedCSV(ctx, c, ocpOperator{packageName: "nvidia-network-operator", namespace: namespace})
	if err != nil {
		return err
	}
	deployments, _, _ := unstructured.NestedSlice(csv.Object, "spec", "install", "spec", "deployments")
	if len(deployments) == 0 {
		return fmt.Errorf("network operator CSV %s has no managed deployments", csv.GetName())
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		allReady := true
		for _, entry := range deployments {
			item, ok := entry.(map[string]interface{})
			if !ok {
				return fmt.Errorf("network operator CSV %s has an invalid deployment entry", csv.GetName())
			}
			name, _ := item["name"].(string)
			if name == "" {
				return fmt.Errorf("network operator CSV %s has an unnamed deployment", csv.GetName())
			}
			deployment := &unstructured.Unstructured{}
			deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
			if err := c.Get(waitCtx, types.NamespacedName{Namespace: namespace, Name: name}, deployment); err != nil {
				if apierrors.IsNotFound(err) {
					allReady = false
					continue
				}
				return err
			}
			if !deploymentHasEnv(deployment, desired) || !deploymentRolledOut(deployment) {
				allReady = false
			}
		}
		if allReady {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("network operator Subscription rollout did not adopt maintenance settings: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func deploymentHasEnv(deployment *unstructured.Unstructured, desired map[string]string) bool {
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return false
	}
	for _, container := range containers {
		fields, ok := container.(map[string]interface{})
		if !ok {
			return false
		}
		env, _ := fields["env"].([]interface{})
		for name, value := range desired {
			found := false
			for _, item := range env {
				variable, ok := item.(map[string]interface{})
				if ok && variable["name"] == name && variable["value"] == value {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func deploymentRolledOut(deployment *unstructured.Unstructured) bool {
	replicas, found, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
	if !found {
		replicas = 1
	}
	generation := deployment.GetGeneration()
	observed, _, _ := unstructured.NestedInt64(deployment.Object, "status", "observedGeneration")
	updated, _, _ := unstructured.NestedInt64(deployment.Object, "status", "updatedReplicas")
	available, _, _ := unstructured.NestedInt64(deployment.Object, "status", "availableReplicas")
	return observed >= generation && updated >= replicas && available >= replicas
}
