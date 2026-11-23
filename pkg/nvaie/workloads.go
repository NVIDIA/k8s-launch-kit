// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// WorkloadObservation records controller counters and coverage by independently
// observed Pods owned by the same workload.
type WorkloadObservation struct {
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	Image              string `json:"image"`
	Version            string `json:"version,omitempty"`
	ImageStatus        Status `json:"imageStatus"`
	Desired            int32  `json:"desired"`
	Observed           int32  `json:"observed"`
	Updated            int32  `json:"updated"`
	Ready              int32  `json:"ready"`
	Available          int32  `json:"available"`
	OwnedReadyPods     int32  `json:"ownedReadyPods"`
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Complete           bool   `json:"complete"`
	Deleting           bool   `json:"deleting,omitempty"`
	nodeSelector       map[string]string
	nodeName           string
	identity           workloadIdentity
	container          string
	normalizedImage    string
}

func observeWorkloads(ctx context.Context, client kubernetes.Interface, observations map[string][]ComponentObservation) (map[string][]WorkloadObservation, []Check) {
	result := map[string][]WorkloadObservation{}
	var checks []Check
	deployments, err := client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		checks = append(checks, check("deployments", Unknown, fmt.Sprintf("List Deployments: %v", err)))
	} else {
		for _, deployment := range deployments.Items {
			desired := int32(1)
			if deployment.Spec.Replicas != nil {
				desired = *deployment.Spec.Replicas
			}
			recordWorkload(result, deployment.Spec.Template.Spec.Containers, WorkloadObservation{
				Kind: "Deployment", Name: deployment.Namespace + "/" + deployment.Name, Desired: desired,
				identity: objectIdentity("Deployment", &deployment),
				Observed: deployment.Status.Replicas, Updated: deployment.Status.UpdatedReplicas,
				Ready: deployment.Status.ReadyReplicas, Available: deployment.Status.AvailableReplicas,
				Generation: deployment.Generation, ObservedGeneration: deployment.Status.ObservedGeneration,
				Deleting: deployment.DeletionTimestamp != nil,
			})
		}
	}
	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		checks = append(checks, check("daemonsets", Unknown, fmt.Sprintf("List DaemonSets: %v", err)))
	} else {
		for _, daemonSet := range daemonSets.Items {
			// CurrentNumberScheduled describes Nodes with an expected Pod rather
			// than including misscheduled Pods, which also block completeness.
			observed := daemonSet.Status.CurrentNumberScheduled + daemonSet.Status.NumberMisscheduled
			recordWorkload(result, daemonSet.Spec.Template.Spec.Containers, WorkloadObservation{
				Kind: "DaemonSet", Name: daemonSet.Namespace + "/" + daemonSet.Name,
				identity:     objectIdentity("DaemonSet", &daemonSet),
				nodeSelector: daemonSet.Spec.Template.Spec.NodeSelector, nodeName: daemonSet.Spec.Template.Spec.NodeName,
				Desired: daemonSet.Status.DesiredNumberScheduled, Observed: observed,
				Updated: daemonSet.Status.UpdatedNumberScheduled,
				Ready:   daemonSet.Status.NumberReady, Available: daemonSet.Status.NumberAvailable,
				Generation: daemonSet.Generation, ObservedGeneration: daemonSet.Status.ObservedGeneration,
				Deleting: daemonSet.DeletionTimestamp != nil,
			})
		}
	}
	replicaSets, err := observeReplicaSetOwners(ctx, client, result)
	if err != nil {
		checks = append(checks, check("replicasets", Unknown, fmt.Sprintf("List ReplicaSets for identified Deployments: %v", err)))
	}
	addOwnedReadyPodCounts(result, observations, replicaSets)
	return result, checks
}

func recordWorkload(results map[string][]WorkloadObservation, containers []corev1.Container, observation WorkloadObservation) {
	observation.Complete = !observation.Deleting && observation.Generation <= observation.ObservedGeneration &&
		observation.Desired == observation.Observed && observation.Desired == observation.Updated &&
		observation.Desired == observation.Ready && observation.Desired == observation.Available
	// A reconciled zero-replica template carries no active release intent.
	// Pods still reported active are assessed independently.
	if observation.Desired == 0 && observation.Complete {
		return
	}
	for _, container := range containers {
		component, version, image := identifyImage(container.Image, container.Name)
		detector, supported := detectorFor(component)
		if !supported || observation.Kind != detector.kind {
			continue
		}
		instance := observation
		instance.Image = container.Image
		instance.normalizedImage = normalizedImage(image)
		instance.container = container.Name
		instance.Version = version
		instance.ImageStatus = Pass
		if version == "" || hasDigest(image) {
			instance.ImageStatus = Unknown
		}
		results[component] = append(results[component], instance)
	}
}

type workloadIdentity struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

func objectIdentity(kind string, object metav1.Object) workloadIdentity {
	return workloadIdentity{kind: kind, namespace: object.GetNamespace(), name: object.GetName(), uid: object.GetUID()}
}

func controllingWorkload(object metav1.Object) workloadIdentity {
	ref := metav1.GetControllerOfNoCopy(object)
	if ref == nil || ref.APIVersion != "apps/v1" || ref.UID == "" {
		return workloadIdentity{}
	}
	return workloadIdentity{kind: ref.Kind, namespace: object.GetNamespace(), name: ref.Name, uid: ref.UID}
}

func observeReplicaSetOwners(ctx context.Context, client kubernetes.Interface, workloads map[string][]WorkloadObservation) (map[workloadIdentity]workloadIdentity, error) {
	deployments := map[workloadIdentity]bool{}
	for _, instances := range workloads {
		for _, instance := range instances {
			if instance.Kind == "Deployment" {
				deployments[instance.identity] = true
			}
		}
	}
	if len(deployments) == 0 {
		return nil, nil
	}
	replicaSets, err := client.AppsV1().ReplicaSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := map[workloadIdentity]workloadIdentity{}
	for _, replicaSet := range replicaSets.Items {
		owner := controllingWorkload(&replicaSet)
		if deployments[owner] {
			result[objectIdentity("ReplicaSet", &replicaSet)] = owner
		}
	}
	return result, nil
}

type readyPodGroup struct {
	owner     workloadIdentity
	container string
	image     string
}

func addOwnedReadyPodCounts(workloads map[string][]WorkloadObservation, observations map[string][]ComponentObservation, replicaSets map[workloadIdentity]workloadIdentity) {
	for component, instances := range workloads {
		readyPods := map[readyPodGroup]map[types.UID]bool{}
		var attributed []ComponentObservation
		for _, observation := range observations[component] {
			owner := observation.owner
			if owner.kind == "ReplicaSet" {
				owner = replicaSets[owner]
			}
			if owner.uid == "" || observation.podUID == "" {
				continue
			}
			attributedToController := false
			for _, instance := range instances {
				if instance.identity == owner && instance.container == observation.Container {
					attributedToController = true
					break
				}
			}
			if !attributedToController {
				continue
			}
			attributed = append(attributed, observation)
			if observation.Ready == nil || !*observation.Ready || observation.ImageStatus != Pass {
				continue
			}
			key := readyPodGroup{owner: owner, container: observation.Container, image: observation.normalizedImage}
			if readyPods[key] == nil {
				readyPods[key] = map[types.UID]bool{}
			}
			readyPods[key][observation.podUID] = true
		}
		observations[component] = attributed
		for i := range instances {
			instance := &instances[i]
			key := readyPodGroup{owner: instance.identity, container: instance.container, image: instance.normalizedImage}
			instance.OwnedReadyPods = int32(len(readyPods[key]))
		}
	}
	// Pod images without a verified supported controller are not installation proof.
	for component := range observations {
		if len(workloads[component]) == 0 {
			delete(observations, component)
		}
	}
}
