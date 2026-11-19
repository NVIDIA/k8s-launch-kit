// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/url"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

type validationAppWorkload struct{ kind, namespace, name string }

func validationApplicationWorkload(app *unstructured.Unstructured, kind, namespace string) (*validationAppWorkload, error) {
	resources, found, err := unstructured.NestedSlice(app.Object, "status", "resources")
	if err != nil || !found {
		return nil, validationFailure(validation.Unverified, validation.ReasonIncomplete, "System Application has no complete resource inventory.")
	}
	var selected *validationAppWorkload
	for _, raw := range resources {
		resource, ok := raw.(map[string]any)
		if !ok {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "System Application resource inventory is malformed.")
		}
		resourceKind, _, _ := unstructured.NestedString(resource, "kind")
		switch resourceKind {
		case "Deployment", "DaemonSet", "StatefulSet", "ReplicaSet", "Pod", "Job", "CronJob":
		default:
			continue
		}
		group, _, _ := unstructured.NestedString(resource, "group")
		version, _, _ := unstructured.NestedString(resource, "version")
		ns, _, _ := unstructured.NestedString(resource, "namespace")
		name, _, _ := unstructured.NestedString(resource, "name")
		state, _, _ := unstructured.NestedString(resource, "status")
		hook, _, _ := unstructured.NestedBool(resource, "hook")
		pruning, _, _ := unstructured.NestedBool(resource, "requiresPruning")
		if kind == "" || resourceKind != kind || group != "apps" || version != "v1" || ns != namespace || name == "" || state != "Synced" || hook || pruning || selected != nil {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "System Application workload inventory differs from the independently expected role.")
		}
		selected = &validationAppWorkload{kind: kind, namespace: ns, name: name}
	}
	if kind != "" && selected == nil {
		return nil, validationFailure(validation.Failed, validation.ReasonMissing, "System Application inventory lacks the required workload.")
	}
	return selected, nil
}
func (o *validationObserver) observeValidationSystemWorkload(ctx context.Context, management validationManagementReader, tenant *tenantClient, role systemArtifactRole, service, app *unstructured.Unstructured, expected systemArtifactExpectation, nodes map[string]corev1.Node, r *validationRecorder) error {
	selected, err := validationApplicationWorkload(app, expected.WorkloadKind, service.GetNamespace())
	if err != nil {
		return err
	}
	if selected == nil {
		return nil
	}
	var current *unstructured.Unstructured
	var pods []corev1.Pod
	if role.Management {
		current, err = management.Get(ctx, validationObject("apps/v1", selected.kind, selected.namespace, selected.name))
		if err != nil {
			return err
		}
	} else {
		if tenant == nil {
			return r.tenantDependencyError()
		}
		if err := tenant.VerifyIdentity(ctx); err != nil {
			return err
		}
		// The complete bounded list independently corroborates App inventory and
		// retains Pod coverage when an inventory omits or misnames a workload.
		sets, currentPods, listErr := tenant.listRuntimeObjects(ctx, selected.namespace, "")
		if listErr != nil {
			return listErr
		}
		pods = currentPods
		for i := range sets {
			if sets[i].Name == selected.name {
				if current != nil {
					return validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected tenant workload identity is ambiguous.")
				}
				object, convertErr := runtime.DefaultUnstructuredConverter.ToUnstructured(&sets[i])
				if convertErr != nil {
					return convertErr
				}
				current = &unstructured.Unstructured{Object: object}
				current.SetAPIVersion("apps/v1")
				current.SetKind("DaemonSet")
			}
		}
		if current == nil {
			return validationFailure(validation.Failed, validation.ReasonMissing, "A required selected tenant workload is absent.")
		}
	}
	if current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil || current.GetAnnotations()["argocd.argoproj.io/tracking-id"] != app.GetName()+":apps/"+selected.kind+":"+selected.namespace+"/"+selected.name {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System workload lacks the selected Argo tracking identity or live server identity.")
	}
	if !validationSystemWorkloadMatches(expected.Workload, current) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System workload spec differs from the independently projected chart and controller inputs.")
	}
	images, err := systemArtifactImages(current)
	if err != nil || !maps.Equal(images, expected.Images) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "System workload container images differ from the locked chart role projection.")
	}
	if role.Management {
		if err := validationReady(o.desired.rules.ResourceReadiness(current)); err != nil {
			return err
		}
		return verifyValidationDeploymentPods(ctx, management, current)
	}
	if nodes == nil {
		return validationFailure(validation.Unverified, validation.ReasonDependency, "Current selected tenant Node identities could not be bound.")
	}
	set, err := validationDaemonSetFromObject(current)
	if err != nil {
		return err
	}
	if err := validationDaemonSetReady(set, len(nodes)); err != nil {
		return err
	}
	owned, err := validationDaemonSetPodMembership(set, pods, nodes)
	if err != nil {
		return err
	}
	var coverageErr error
	for serial := range nodes {
		err := r.run(validation.CheckID(systemRoleID(role)+"/device/"+url.PathEscape(serial)), func() error {
			selectedPod := owned[serial]
			if selectedPod == nil {
				return validationFailure(validation.Failed, validation.ReasonMissing, "A selected tenant Node has no current system Pod owned by the selected DaemonSet.")
			}
			if err := validationPodNetworkRequestsMatch(selectedPod, &set.Spec.Template); err != nil {
				return err
			}
			// The selected Kamaji TCP explicitly enables AlwaysPullImages.
			if err := validationPodMatchesTemplate(selectedPod, &set.Spec.Template.Spec, "DaemonSet", true); err != nil {
				return err
			}
			return validationPodReady(selectedPod, &set.Spec.Template.Spec)
		})
		coverageErr = validationPodCoverageError(coverageErr, err)
	}

	if err := tenant.VerifyIdentity(ctx); err != nil {
		return err
	}
	after, err := o.validationSelectedTenantNodes(ctx, management, tenant)
	if err != nil {
		return err
	}
	if len(nodes) != len(after) {
		return fmt.Errorf("selected tenant Nodes changed: %w", errValidationResourceChanged)
	}
	for serial, node := range nodes {
		if after[serial].UID != node.UID {
			return fmt.Errorf("selected tenant Node was replaced: %w", errValidationResourceChanged)
		}
	}
	return coverageErr
}
func validationDaemonSetReady(set *appsv1.DaemonSet, count int) error {
	if set == nil || count < 1 || count > math.MaxInt32 || set.Generation <= 0 || set.Status.ObservedGeneration < 0 || set.Status.ObservedGeneration > set.Generation {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "System DaemonSet generation or selected Node count is invalid or refers to a future generation.")
	}
	for _, value := range []int32{set.Status.DesiredNumberScheduled, set.Status.CurrentNumberScheduled, set.Status.UpdatedNumberScheduled, set.Status.NumberReady, set.Status.NumberAvailable, set.Status.NumberMisscheduled, set.Status.NumberUnavailable} {
		if value < 0 {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "System DaemonSet reports an invalid negative rollout count.")
		}
	}
	if set.Status.NumberReady > set.Status.CurrentNumberScheduled || set.Status.NumberAvailable > set.Status.NumberReady || set.Status.UpdatedNumberScheduled > set.Status.CurrentNumberScheduled || int64(set.Status.NumberAvailable)+int64(set.Status.NumberUnavailable) > int64(set.Status.DesiredNumberScheduled) {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "System DaemonSet reports contradictory rollout counts.")
	}
	n := int32(count)
	if set.Status.ObservedGeneration < set.Generation || set.Status.DesiredNumberScheduled != n || set.Status.CurrentNumberScheduled != n || set.Status.UpdatedNumberScheduled != n || set.Status.NumberReady != n || set.Status.NumberAvailable != n || set.Status.NumberMisscheduled != 0 || set.Status.NumberUnavailable != 0 {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "System DaemonSet rollout does not cover every selected tenant Node at the current generation.")
	}
	return nil
}
func verifyValidationDeploymentPods(ctx context.Context, management interface {
	resourceReader
	resourceLister
}, object *unstructured.Unstructured) error {
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &deployment); err != nil {
		return err
	}
	if deployment.Spec.Selector == nil {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "System Deployment has no Pod selector.")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || selector.Empty() {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "System Deployment Pod selector is malformed or empty.")
	}
	// Ownership is independent of labels: selector-filtered lists would hide an
	// active owned ReplicaSet or Pod whose labels no longer match this workload.
	sets, err := management.List(ctx, validationObject("apps/v1", "ReplicaSet", object.GetNamespace(), ""), "")
	if err != nil {
		return err
	}
	var currentSet *appsv1.ReplicaSet
	var currentSelector labels.Selector
	for i := range sets {
		var set appsv1.ReplicaSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sets[i].Object, &set); err != nil {
			return err
		}
		if set.Namespace != deployment.Namespace || !validationControllerOwner(set.OwnerReferences, "Deployment", deployment.Name, deployment.UID) {
			continue
		}
		if set.Spec.Replicas == nil || *set.Spec.Replicas < 0 {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "An owned system ReplicaSet has no valid replica count.")
		}
		if *set.Spec.Replicas == 0 {
			continue // Historical zero-replica revisions do not define the current cohort.
		}
		if !selector.Matches(labels.Set(set.Labels)) || set.Spec.Selector == nil {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An active owned system ReplicaSet violates the selected workload selector.")
		}
		setSelector, selectorErr := metav1.LabelSelectorAsSelector(set.Spec.Selector)
		if selectorErr != nil || setSelector.Empty() || !setSelector.Matches(labels.Set(set.Spec.Template.Labels)) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An active owned system ReplicaSet has an invalid Pod selector or template labels.")
		}
		// Deployment controllers add only the revision hash to the retained Pod
		// template labels and selector. Compare the remaining selector exactly.
		baseSelector := set.Spec.Selector.DeepCopy()
		baseLabels := maps.Clone(set.Spec.Template.Labels)
		if hash, present := baseSelector.MatchLabels[appsv1.DefaultDeploymentUniqueLabelKey]; present {
			if hash == "" || set.Labels[appsv1.DefaultDeploymentUniqueLabelKey] != hash || baseLabels[appsv1.DefaultDeploymentUniqueLabelKey] != hash {
				return validationFailure(validation.Failed, validation.ReasonHealth, "An active owned system ReplicaSet has inconsistent revision labels.")
			}
			delete(baseSelector.MatchLabels, appsv1.DefaultDeploymentUniqueLabelKey)
			delete(baseLabels, appsv1.DefaultDeploymentUniqueLabelKey)
		}
		base, selectorErr := metav1.LabelSelectorAsSelector(baseSelector)
		if selectorErr != nil || base.String() != selector.String() || !maps.Equal(baseLabels, deployment.Spec.Template.Labels) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An active owned system ReplicaSet selector or template labels differ from its Deployment.")
		}
		// The rollout's active ReplicaSet must carry the current complete PodSpec.
		if !reflect.DeepEqual(set.Spec.Template.Spec, deployment.Spec.Template.Spec) {
			return validationFailure(validation.Pending, validation.ReasonProgressing, "System Deployment still has an active ReplicaSet for an older Pod template.")
		}
		if currentSet != nil {
			return validationFailure(validation.Failed, validation.ReasonIncomplete, "System Deployment has multiple active current ReplicaSets.")
		}
		currentSet = &set
		currentSelector = setSelector
	}
	if currentSet == nil || currentSet.UID == "" || currentSet.DeletionTimestamp != nil {
		return validationFailure(validation.Failed, validation.ReasonMissing, "System Deployment has no active owned current ReplicaSet.")
	}
	objects, err := management.List(ctx, validationObject("v1", "Pod", object.GetNamespace(), ""), "")
	if err != nil {
		return err
	}
	count := int32(0)
	for i := range objects {
		var pod corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects[i].Object, &pod); err != nil {
			return err
		}
		if pod.Namespace != deployment.Namespace || !validationControllerOwner(pod.OwnerReferences, "ReplicaSet", currentSet.Name, currentSet.UID) {
			continue
		}
		if !selector.Matches(labels.Set(pod.Labels)) || !currentSelector.Matches(labels.Set(pod.Labels)) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An owned system Pod violates the selected workload selector.")
		}
		if err := validationPodNetworkRequestsMatch(&pod, &deployment.Spec.Template); err != nil {
			return err
		}
		if err := validationPodMatchesTemplate(&pod, &deployment.Spec.Template.Spec, "ReplicaSet", false); err != nil {
			return err
		}
		if err := validationPodReady(&pod, &deployment.Spec.Template.Spec); err != nil {
			return err
		}
		count++
	}
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	if desired < 1 || count != desired {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "Current owned system Pods do not cover the selected Deployment replicas.")
	}
	current, err := management.Get(ctx, validationObject("apps/v1", "Deployment", object.GetNamespace(), object.GetName()))
	if err != nil {
		return err
	}
	if !sameValidationSemantic(object, current) {
		return fmt.Errorf("system Deployment changed: %w", errValidationResourceChanged)
	}
	return nil
}
func validationControllerOwner(owners []metav1.OwnerReference, kind, name string, uid types.UID) bool {
	found := false
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller {
			if found || owner.APIVersion != "apps/v1" || owner.Kind != kind || owner.Name != name || owner.UID != uid {
				return false
			}
			found = true
		}
	}
	return found
}
func validationPodReady(pod *corev1.Pod, template *corev1.PodSpec) error {
	if pod == nil || pod.UID == "" || pod.ResourceVersion == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "A selected system Pod is not currently running and ready.")
	}
	if len(pod.Spec.Containers) != len(template.Containers) || len(pod.Spec.InitContainers) != len(template.InitContainers) || len(pod.Status.ContainerStatuses) != len(template.Containers) || len(pod.Status.InitContainerStatuses) != len(template.InitContainers) {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected system Pod container coverage differs from the workload template.")
	}
	for _, entry := range []struct {
		spec, want []corev1.Container
		statuses   []corev1.ContainerStatus
		init       bool
	}{{pod.Spec.Containers, template.Containers, pod.Status.ContainerStatuses, false}, {pod.Spec.InitContainers, template.InitContainers, pod.Status.InitContainerStatuses, true}} {
		byName := map[string]corev1.ContainerStatus{}
		for _, status := range entry.statuses {
			if status.Name == "" || byName[status.Name].Name != "" {
				return validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected system Pod has duplicate or unnamed container status.")
			}
			byName[status.Name] = status
		}
		for i, container := range entry.spec {
			want := entry.want[i]
			status, ok := byName[container.Name]
			if !ok || container.Name != want.Name || container.Image != want.Image || status.ImageID == "" {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Selected system Pod image or container identity differs from its workload template.")
			}
			sidecar := container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways
			if entry.init && !sidecar {
				if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
					return validationFailure(validation.Pending, validation.ReasonProgressing, "A selected system init container has not completed successfully.")
				}
			} else if !status.Ready || status.State.Running == nil {
				return validationFailure(validation.Pending, validation.ReasonProgressing, "A selected system container is not running and ready.")
			}
		}
	}
	return nil
}

// Complete owner membership precedes label and selected-node joins. No selector
// may hide a contradictory current owner-bound Pod from coverage checks.
func validationDaemonSetPodMembership(set *appsv1.DaemonSet, pods []corev1.Pod, nodes map[string]corev1.Node) (map[string]*corev1.Pod, error) {
	if set == nil || set.UID == "" || set.Name == "" || set.Namespace == "" || set.Spec.Selector == nil {
		return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected DaemonSet identity or selector is incomplete.")
	}
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(set.Spec.Template.Labels)) {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Selected DaemonSet has an invalid Pod selector or template labels.")
	}
	serialByNode := map[string]string{}
	for serial, node := range nodes {
		if serial == "" || node.Name == "" || node.UID == "" || serialByNode[node.Name] != "" {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected DaemonSet Node membership is incomplete or ambiguous.")
		}
		serialByNode[node.Name] = serial
	}
	result := map[string]*corev1.Pod{}
	for i := range pods {
		pod := &pods[i]
		participates := false
		for _, owner := range pod.OwnerReferences {
			if owner.UID == set.UID && owner.Controller != nil && *owner.Controller {
				participates = true
			}
		}
		if !participates {
			continue
		}
		if pod.Namespace != set.Namespace || !validationControllerOwner(pod.OwnerReferences, "DaemonSet", set.Name, set.UID) || !selector.Matches(labels.Set(pod.Labels)) {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "An owned DaemonSet Pod violates the selected current owner, namespace or workload selector.")
		}
		serial := serialByNode[pod.Spec.NodeName]
		if serial == "" {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected DaemonSet has an owned Pod outside the selected Node cohort.")
		}
		if result[serial] != nil {
			return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "More than one selected DaemonSet Pod is active on a tenant Node.")
		}
		result[serial] = pod
	}
	return result, nil
}

func validationPodCoverageError(previous, current error) error {
	rank := func(err error) int {
		if err == nil {
			return 0
		}
		if check, ok := err.(*validationCheckError); ok {
			switch check.state {
			case validation.Pending:
				return 1
			case validation.Unverified:
				return 2
			case validation.Failed:
				return 3
			}
		}
		return 4
	}
	if rank(current) > rank(previous) {
		return current
	}
	return previous
}

func validationDaemonSetFromObject(object *unstructured.Unstructured) (*appsv1.DaemonSet, error) {
	if object == nil {
		return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected DaemonSet observation is missing.")
	}
	var set appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &set); err != nil {
		return nil, validationFailure(validation.Failed, validation.ReasonIncomplete, "Selected DaemonSet observation has malformed generation, rollout counts or typed fields.")
	}
	return &set, nil
}
