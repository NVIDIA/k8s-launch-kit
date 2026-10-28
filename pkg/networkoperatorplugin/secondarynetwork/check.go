// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package secondarynetwork

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Check is operation-local evidence about an omitted external component.
// Skipped means unverified, never a successful health check.
type Check struct {
	Component   string
	Skipped     bool
	Progressing bool
	Failed      bool
	Reason      string
}

// CheckPolicy checks the actual artifact, including a mandatory live-policy
// read to prevent removal of an existing managed section. Installation collision
// inspection is mandatory; health inspection of omitted sections is optional.
// A nil policy performs no API reads.
func CheckPolicy(ctx context.Context, c client.Reader, desired *unstructured.Unstructured, workers []string) ([]Check, error) {
	if desired == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, err := GetPolicy(ctx, c, desired.GetName())
	if err != nil {
		return nil, err
	}
	managed := map[string]bool{}
	for _, component := range []string{Multus, CNIPlugins} {
		managed[component], err = hasManagedSection(desired, component)
		if err != nil {
			return nil, err
		}
		if live != nil && !managed[component] {
			present, err := hasManagedSection(live, component)
			if err != nil {
				return nil, err
			}
			if present {
				return nil, fmt.Errorf("refusing managed-to-external %s transition in NicClusterPolicy %s: removing its section can delete networking; perform a separately controlled migration first", component, desired.GetName())
			}
		}
	}
	inspection, inspectErr := Inspect(ctx, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if inspectErr != nil && (managed[Multus] || managed[CNIPlugins]) {
		return nil, inspectErr
	}
	var checks []Check
	for _, component := range []string{Multus, CNIPlugins} {
		if inspectErr != nil {
			checks = append(checks, Check{Component: component, Skipped: true, Reason: "unverified external prerequisite: " + inspectErr.Error()})
			continue
		}
		observation := inspection.Observation.Multus
		if component == CNIPlugins {
			observation = inspection.Observation.CNIPlugins
		}
		if managed[component] {
			if observation.Status == config.InstallerNotDetected {
				continue
			}
			if observation.Status == config.InstallerOperatorManaged && live != nil && len(observation.Installers) == 1 && observation.Installers[0].PolicyUID == string(live.GetUID()) && observation.Installers[0].PolicyName == live.GetName() {
				continue
			}
			return nil, fmt.Errorf("refusing to install %s: %s (%s); inspect ownership and regenerate with external ownership when platform-managed", component, observation.Status, observation.Reason)
		}
		check := Check{Component: component, Skipped: true, Reason: "unverified external prerequisite: " + observation.Reason}
		switch observation.Status {
		case config.InstallerAmbiguous, config.InstallerOperatorManaged:
			check.Skipped = false
			check.Failed = true
			check.Reason = "external ownership conflict: " + observation.Reason
		case config.InstallerExternal:
			check, err = checkHealth(ctx, c, component, inspection.Installers[component][0], workers)
			if err != nil {
				return nil, err
			}
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func checkHealth(ctx context.Context, c client.Reader, component string, ds appsv1.DaemonSet, workers []string) (Check, error) {
	check := Check{Component: component, Skipped: true, Reason: fmt.Sprintf("unverified external prerequisite: %s/%s detected; required worker coverage unavailable", ds.Namespace, ds.Name)}
	if len(workers) == 0 {
		return check, nil
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ds.Namespace)); err != nil {
		if ctx.Err() != nil {
			return check, ctx.Err()
		}
		check.Reason = "unverified external prerequisite: inspect installer pods: " + err.Error()
		return check, nil
	}
	ready := map[string]bool{}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID != ds.UID || ds.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready[pod.Spec.NodeName] = true
			}
		}
	}
	var missing []string
	for _, worker := range workers {
		// A selector mismatch proves incompatible coverage. Lack of a Ready
		// pod alone can be transient, and must not be labeled incompatibility.
		if len(ds.Spec.Template.Spec.NodeSelector) > 0 {
			node := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKey{Name: worker}, node); err != nil {
				if ctx.Err() != nil {
					return check, ctx.Err()
				}
				check.Reason = "unverified external prerequisite: inspect required worker: " + err.Error()
				return check, nil
			}
			for key, value := range ds.Spec.Template.Spec.NodeSelector {
				if node.Labels[key] != value {
					check.Skipped = false
					check.Failed = true
					check.Reason = fmt.Sprintf("external %s installer %s/%s excludes required worker %s via nodeSelector", component, ds.Namespace, ds.Name, worker)
					return check, nil
				}
			}
		}
		if !ready[worker] {
			missing = append(missing, worker)
		}
	}
	check.Skipped = false
	if ds.DeletionTimestamp != nil || len(missing) > 0 || ds.Status.ObservedGeneration < ds.Generation || ds.Status.DesiredNumberScheduled == 0 || ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled || ds.Status.NumberReady < ds.Status.DesiredNumberScheduled || ds.Status.NumberAvailable < ds.Status.DesiredNumberScheduled {
		sort.Strings(missing)
		check.Progressing = true
		check.Reason = fmt.Sprintf("external %s installer %s/%s is reconciling; required workers without Ready installer pods: %s", component, ds.Namespace, ds.Name, strings.Join(missing, ", "))
		return check, nil
	}
	check.Reason = fmt.Sprintf("external %s installer %s/%s UID %s has Ready pods on all %d required workers; host binary/configuration correctness requires runtime validation", component, ds.Namespace, ds.Name, ds.UID, len(workers))
	return check, nil
}

// CheckRemoval rejects implicit teardown of managed networking, independently
// of whether an installer child currently exists. A nil policy is verified
// absent. Callers must refresh policy evidence before deletion.
func CheckRemoval(policy *unstructured.Unstructured) error {
	if policy == nil {
		return nil
	}
	for _, component := range []string{Multus, CNIPlugins} {
		present, err := hasManagedSection(policy, component)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("refusing to remove NicClusterPolicy %s: its managed %s section can delete networking even when the installer is missing; perform a separately controlled migration first", policy.GetName(), component)
		}
	}
	return nil
}

// hasManagedSection follows the API pointer semantics: absent/null is unset,
// while an empty mapping still expresses managed component intent.
func hasManagedSection(policy *unstructured.Unstructured, component string) (bool, error) {
	secondary, found, err := unstructured.NestedFieldNoCopy(policy.Object, "spec", "secondaryNetwork")
	if err != nil {
		return false, err
	}
	if !found || secondary == nil {
		return false, nil
	}
	sections, ok := secondary.(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("NicClusterPolicy %s spec.secondaryNetwork must be a mapping", policy.GetName())
	}
	raw, found := sections[component]
	if !found || raw == nil {
		return false, nil
	}
	section, ok := raw.(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("NicClusterPolicy %s spec.secondaryNetwork.%s must be a mapping", policy.GetName(), component)
	}
	return section != nil, nil
}
