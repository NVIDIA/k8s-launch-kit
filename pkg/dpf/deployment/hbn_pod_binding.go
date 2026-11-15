// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	hbnServiceIDLabel           = "svc.dpu.nvidia.com/service"
	dpfComponentLabel           = "dpu.nvidia.com/component"
	dpfDPUServiceNameLabel      = "dpu.nvidia.com/dpuservice-name"
	dpfDPUServiceNamespaceLabel = "dpu.nvidia.com/dpuservice-namespace"
	dpfDPUClusterNameLabel      = "dpu.nvidia.com/cluster"
	dpfDPUClusterNamespaceLabel = "dpu.nvidia.com/cluster-namespace"
	dpfServiceManagerComponent  = "dpuservice-manager"
	argoApplicationFinalizer    = "resources-finalizer.argocd.argoproj.io"
	hbnContainerName            = "doca-hbn"
	hbnServiceReferenceLabel    = "svc.dpu.nvidia.com/dpudeployment-service"
	hbnServiceVersionAnnotation = "svc.dpu.nvidia.com/dpuservice-version"
)

// hbnPodBindingIntent contains only expectations derived from the retained
// DPUService, DPUCluster, DPFOperatorConfig and HBN chart.
// ExpectedServiceSpec and ApplicationValues must come from the same retained
// template/config and qualified controller additions, never an unchecked copy
// of the observed Service or Application. The caller owns that compilation.
type hbnPodBindingIntent struct {
	DPUDeploymentName   string
	DPUDeploymentUID    string
	DPUServiceUID       string
	ExpectedServiceSpec map[string]any
	ApplicationValues   map[string]any
	ArgoNamespace       string
	DPUServiceName      string
	DPUServiceNamespace string
	DPUServiceCreatedAt time.Time
	DPUClusterName      string
	DPUClusterNamespace string
	TargetNamespace     string
	ChartRepoURL        string
	ChartName           string
	ChartPath           string
	ChartVersion        string
	ChartReleaseName    string
	ServiceID           string
	TenantNodeName      string
	Image               hbnPodImageIdentity
}

// hbnPodImageIdentity is resolved from the retained OCI image for the selected
// platform. ManifestDigest is the platform manifest, not an image index. Only a
// separately verified config descriptor permits config-only runtime ImageIDs.
// This pure selector performs no registry resolution and must not infer it from
// Pod status or accept it as a user override.
type hbnPodImageIdentity struct {
	Reference      string
	ManifestDigest string
	ConfigDigest   string
}

// hbnPodBindingAnchor compares semantic sources and the actual running instance.
// Resource versions and observation timestamps are deliberately excluded: status
// refreshes do not replace a container or change the retained desired state.
type hbnPodBindingAnchor struct {
	ServiceUID            string
	ServiceGeneration     int64
	ServiceVersion        string
	ServiceSpecDigest     string
	ApplicationName       string
	ApplicationNamespace  string
	ApplicationUID        string
	ApplicationSpecDigest string
	DaemonSetName         string
	DaemonSetNamespace    string
	DaemonSetUID          string
	DaemonSetGeneration   int64
	DaemonSetSpecDigest   string
	PodName               string
	PodNamespace          string
	PodUID                string
	PodGeneration         int64
	PodSpecDigest         string
	PodMetadataDigest     string
	Containers            []hbnContainerBindingAnchor
}

// The pinned chart has one application container, one completed init and one
// restartable init sidecar. Keep their identities explicit and independently
// owned; status pointers must never alias a later API observation.
type hbnContainerBindingAnchor struct {
	Role, Name, ContainerID, ImageID, RuntimeImage string
	RestartCount                                   int32
	Ready, Started, StartedKnown                   bool
	StartedAt, FinishedAt                          string
	ExitCode                                       int32
}

type hbnPodBindingReason string

const (
	hbnBindingServiceInvalid     hbnPodBindingReason = "HBN_DPUSERVICE_INVALID"
	hbnBindingInputInvalid       hbnPodBindingReason = "HBN_POD_BINDING_INPUT_INVALID"
	hbnBindingApplicationInvalid hbnPodBindingReason = "HBN_APPLICATION_INVALID"
	hbnBindingApplicationStale   hbnPodBindingReason = "HBN_APPLICATION_STATUS_STALE"
	hbnBindingApplicationSource  hbnPodBindingReason = "HBN_APPLICATION_SOURCE_MISMATCH"
	hbnBindingApplicationTarget  hbnPodBindingReason = "HBN_APPLICATION_TARGET_MISMATCH"
	hbnBindingResourceMissing    hbnPodBindingReason = "HBN_APPLICATION_DAEMONSET_MISSING"
	hbnBindingResourceAmbiguous  hbnPodBindingReason = "HBN_APPLICATION_DAEMONSET_AMBIGUOUS"
	hbnBindingResourceUnknown    hbnPodBindingReason = "HBN_APPLICATION_DAEMONSET_UNKNOWN"
	hbnBindingDaemonSetMissing   hbnPodBindingReason = "HBN_DAEMONSET_MISSING"
	hbnBindingDaemonSetAmbiguous hbnPodBindingReason = "HBN_DAEMONSET_AMBIGUOUS"
	hbnBindingServiceMismatch    hbnPodBindingReason = "HBN_DAEMONSET_SERVICE_MISMATCH"
	hbnBindingPodMissing         hbnPodBindingReason = "HBN_POD_MISSING"
	hbnBindingPodAmbiguous       hbnPodBindingReason = "HBN_POD_AMBIGUOUS"
	hbnBindingPodStale           hbnPodBindingReason = "HBN_POD_STALE"
	hbnBindingPodNotReady        hbnPodBindingReason = "HBN_POD_NOT_READY"
	hbnBindingPodImageMismatch   hbnPodBindingReason = "HBN_POD_IMAGE_MISMATCH"
)

type hbnPodBindingError struct{ reason hbnPodBindingReason }

func (e *hbnPodBindingError) Error() string { return string(e.reason) }

// validateHBNPodBinding checks the observed management DPUService against
// retained desired state, then uses its exact-values Argo Application to name
// the tenant DaemonSet. It selects one currently running container on the
// chosen node without assuming any cross-cluster owner reference. observedAt
// is the caller's time after fetching the App; it bounds status timestamps but
// does not replace the caller's fresh before/after source and instance fences.
func validateHBNPodBinding(intent hbnPodBindingIntent, service, application *unstructured.Unstructured, daemonSets []appsv1.DaemonSet, pods []corev1.Pod, observedAt time.Time) (hbnPodBindingAnchor, error) {
	if !validHBNBindingIntent(intent) || observedAt.IsZero() {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingInputInvalid)
	}
	if err := validateHBNService(intent, service); err != nil {
		return hbnPodBindingAnchor{}, err
	}
	resourceName, err := validateHBNApplication(intent, application, observedAt)
	if err != nil {
		return hbnPodBindingAnchor{}, err
	}
	var matches []appsv1.DaemonSet
	for i := range daemonSets {
		ds := &daemonSets[i]
		if ds.Namespace == intent.TargetNamespace && ds.Name == resourceName {
			matches = append(matches, *ds.DeepCopy())
		}
	}
	if len(matches) == 0 {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingDaemonSetMissing)
	}
	if len(matches) != 1 {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingDaemonSetAmbiguous)
	}
	ds := &matches[0]
	if ds.UID == "" || ds.ResourceVersion == "" || ds.DeletionTimestamp != nil || ds.Generation < 1 ||
		ds.Status.ObservedGeneration != ds.Generation || ds.Status.DesiredNumberScheduled < 1 ||
		ds.Status.UpdatedNumberScheduled != ds.Status.DesiredNumberScheduled || ds.Status.NumberReady != ds.Status.DesiredNumberScheduled || ds.Status.NumberUnavailable != 0 {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingResourceUnknown)
	}
	if ds.Labels[hbnServiceIDLabel] != intent.ServiceID || ds.Spec.Template.Labels[hbnServiceIDLabel] != intent.ServiceID ||
		ds.Labels[hbnServiceReferenceLabel] != hbnContainerName || ds.Spec.Template.Labels[hbnServiceReferenceLabel] != hbnContainerName {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingServiceMismatch)
	}
	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(ds.Spec.Template.Labels)) {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingResourceUnknown)
	}
	container, found := namedContainer(ds.Spec.Template.Spec.Containers, hbnContainerName)
	if !found || container.Image != intent.Image.Reference {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingPodImageMismatch)
	}
	var selected []corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != intent.TargetNamespace || pod.Spec.NodeName != intent.TenantNodeName || !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		selected = append(selected, *pod.DeepCopy())
	}
	if len(selected) == 0 {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingPodMissing)
	}
	if len(selected) != 1 {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingPodAmbiguous)
	}
	pod := &selected[0]
	if pod.UID == "" || pod.ResourceVersion == "" || pod.DeletionTimestamp != nil || !podOwnedByDaemonSet(pod, ds) ||
		pod.Labels[hbnServiceIDLabel] != intent.ServiceID || pod.Labels[hbnServiceReferenceLabel] != hbnContainerName {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingPodStale)
	}
	podContainer, found := namedContainer(pod.Spec.Containers, hbnContainerName)
	if !found || podContainer.Image != intent.Image.Reference {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingPodImageMismatch)
	}
	containers, err := hbnPodContainers(pod, &ds.Spec.Template.Spec, intent.Image)
	if err != nil {
		return hbnPodBindingAnchor{}, err
	}
	anchor := hbnPodBindingAnchor{
		ServiceUID: string(service.GetUID()), ServiceGeneration: service.GetGeneration(), ServiceVersion: service.GetAnnotations()[hbnServiceVersionAnnotation],
		ServiceSpecDigest:     hbnBindingDigest(service.Object["spec"]),
		ApplicationSpecDigest: hbnBindingDigest(application.Object["spec"]),
		DaemonSetGeneration:   ds.Generation, DaemonSetSpecDigest: hbnBindingDigest(ds.Spec),
		PodGeneration: pod.Generation, PodSpecDigest: hbnBindingDigest(pod.Spec), PodMetadataDigest: hbnBindingDigest([]any{pod.Labels, pod.Annotations}),
		Containers:      containers,
		ApplicationName: application.GetName(), ApplicationNamespace: application.GetNamespace(),
		ApplicationUID: string(application.GetUID()),
		DaemonSetName:  ds.Name, DaemonSetNamespace: ds.Namespace, DaemonSetUID: string(ds.UID),
		PodName: pod.Name, PodNamespace: pod.Namespace, PodUID: string(pod.UID),
	}
	if anchor.ServiceSpecDigest == "" || anchor.ApplicationSpecDigest == "" || anchor.DaemonSetSpecDigest == "" || anchor.PodSpecDigest == "" || anchor.PodMetadataDigest == "" {
		return hbnPodBindingAnchor{}, bindingError(hbnBindingInputInvalid)
	}
	return anchor, nil
}

func validHBNBindingIntent(intent hbnPodBindingIntent) bool {
	return intent.ArgoNamespace != "" && intent.DPUServiceName != "" && intent.DPUServiceNamespace != "" &&
		!intent.DPUServiceCreatedAt.IsZero() && intent.DPUClusterName != "" && intent.DPUClusterNamespace != "" &&
		intent.TargetNamespace != "" && intent.ChartRepoURL != "" && intent.ChartName == "doca-hbn" &&
		intent.ChartVersion != "" && intent.ServiceID != "" && intent.TenantNodeName != "" && intent.Image.valid() &&
		intent.DPUDeploymentName != "" && intent.DPUDeploymentUID != "" && intent.DPUServiceUID != "" && len(intent.ExpectedServiceSpec) > 0 && len(intent.ApplicationValues) > 0 &&
		intent.TargetNamespace == intent.DPUServiceNamespace
}

func validateHBNApplication(intent hbnPodBindingIntent, application *unstructured.Unstructured, observedAt time.Time) (string, error) {
	if application == nil || application.GetKind() != "Application" || application.GetAPIVersion() != "argoproj.io/v1alpha1" ||
		application.GetNamespace() != intent.ArgoNamespace || application.GetName() != dpfApplicationName(intent.DPUClusterName, intent.DPUServiceName) ||
		application.GetUID() == "" || application.GetResourceVersion() == "" || application.GetDeletionTimestamp() != nil {
		return "", bindingError(hbnBindingApplicationInvalid)
	}
	labels := application.GetLabels()
	if labels == nil || labels[dpfComponentLabel] != dpfServiceManagerComponent ||
		labels[dpfDPUServiceNameLabel] != intent.DPUServiceName || labels[dpfDPUServiceNamespaceLabel] != intent.DPUServiceNamespace ||
		labels[dpfDPUClusterNameLabel] != intent.DPUClusterName || labels[dpfDPUClusterNamespaceLabel] != intent.DPUClusterNamespace {
		return "", bindingError(hbnBindingApplicationInvalid)
	}
	finalizerFound := false
	for _, finalizer := range application.GetFinalizers() {
		if finalizer == argoApplicationFinalizer {
			finalizerFound = true
		}
	}
	if !finalizerFound {
		return "", bindingError(hbnBindingApplicationInvalid)
	}
	if err := validateHBNApplicationSpec(intent, application); err != nil {
		return "", err
	}
	status, found, err := unstructured.NestedMap(application.Object, "status")
	if err != nil || !found {
		return "", bindingError(hbnBindingApplicationStale)
	}
	syncStatus, _, _ := unstructured.NestedString(status, "sync", "status")
	healthStatus, _, _ := unstructured.NestedString(status, "health", "status")
	reconciledAtString, _, reconciledErr := unstructured.NestedString(status, "reconciledAt")
	reconciledAt, timeErr := time.Parse(time.RFC3339Nano, reconciledAtString)
	if syncStatus != "Synced" || healthStatus != "Healthy" || reconciledErr != nil || timeErr != nil ||
		reconciledAt.After(observedAt) || reconciledAt.Before(intent.DPUServiceCreatedAt) {
		return "", bindingError(hbnBindingApplicationStale)
	}
	resources, found, resourceErr := unstructured.NestedSlice(status, "resources")
	if resourceErr != nil || !found {
		return "", bindingError(hbnBindingResourceUnknown)
	}
	matchingResources := 0
	resourceName := ""
	for _, raw := range resources {
		resource, ok := raw.(map[string]any)
		if !ok {
			return "", bindingError(hbnBindingResourceUnknown)
		}
		kind, kindFound, kindErr := unstructured.NestedString(resource, "kind")
		if kindErr != nil || !kindFound || kind == "" {
			return "", bindingError(hbnBindingResourceUnknown)
		}
		if kind != "DaemonSet" {
			continue
		}
		matchingResources++
		group, _, groupErr := unstructured.NestedString(resource, "group")
		version, _, versionErr := unstructured.NestedString(resource, "version")
		namespace, _, nsErr := unstructured.NestedString(resource, "namespace")
		name, _, nameErr := unstructured.NestedString(resource, "name")
		syncState, _, syncErr := unstructured.NestedString(resource, "status")
		// Argo appTree health can omit per-resource health. When supplied it
		// must be Healthy; the actual DaemonSet and Pod checks remain mandatory.
		resourceHealth, healthFound, healthErr := unstructured.NestedString(resource, "health", "status")
		_, healthPresent := resource["health"]
		hook, _, hookErr := unstructured.NestedBool(resource, "hook")
		pruning, _, pruningErr := unstructured.NestedBool(resource, "requiresPruning")
		if groupErr != nil || versionErr != nil || nsErr != nil || nameErr != nil || syncErr != nil || healthErr != nil || hookErr != nil || pruningErr != nil ||
			group != "apps" || version != "v1" || namespace != intent.TargetNamespace || name == "" || syncState != "Synced" || healthPresent && (!healthFound || resourceHealth != "Healthy") || hook || pruning {
			return "", bindingError(hbnBindingResourceUnknown)
		}
		resourceName = name
	}
	if matchingResources == 0 {
		return "", bindingError(hbnBindingResourceMissing)
	}
	if matchingResources != 1 {
		return "", bindingError(hbnBindingResourceAmbiguous)
	}
	return resourceName, nil
}

func validateHBNApplicationSpec(intent hbnPodBindingIntent, application *unstructured.Unstructured) error {
	sources, sourcesFound, sourcesErr := unstructured.NestedSlice(application.Object, "spec", "sources")
	comparedSources, comparedSourcesFound, comparedSourcesErr := unstructured.NestedSlice(application.Object, "status", "sync", "comparedTo", "sources")
	if sourcesErr != nil || comparedSourcesErr != nil || sourcesFound && len(sources) != 0 || comparedSourcesFound && len(comparedSources) != 0 {
		return bindingError(hbnBindingApplicationSource)
	}
	source, found, sourceErr := unstructured.NestedMap(application.Object, "spec", "source")
	comparedSource, comparedFound, comparedErr := unstructured.NestedMap(application.Object, "status", "sync", "comparedTo", "source")
	if sourceErr != nil || comparedErr != nil || !found || !comparedFound || !jsonEqual(source, comparedSource) {
		return bindingError(hbnBindingApplicationStale)
	}
	repoURL, _, repoErr := unstructured.NestedString(source, "repoURL")
	chart, _, chartErr := unstructured.NestedString(source, "chart")
	path, _, pathErr := unstructured.NestedString(source, "path")
	revision, _, revisionErr := unstructured.NestedString(source, "targetRevision")
	releaseName, _, releaseErr := unstructured.NestedString(source, "helm", "releaseName")
	if repoErr != nil || chartErr != nil || pathErr != nil || revisionErr != nil || releaseErr != nil ||
		repoURL != intent.ChartRepoURL || chart != intent.ChartName || path != intent.ChartPath || revision != intent.ChartVersion || releaseName != intent.ChartReleaseName {
		return bindingError(hbnBindingApplicationSource)
	}
	// ValuesObject is the pinned DPF NewApplication path. Disallow alternate
	// Helm inputs that could override the retained effective values.
	expectedHelm := map[string]any{"valuesObject": intent.ApplicationValues}
	if intent.ChartReleaseName != "" {
		expectedHelm["releaseName"] = intent.ChartReleaseName
	}
	expectedSource := map[string]any{"repoURL": intent.ChartRepoURL, "chart": intent.ChartName, "targetRevision": intent.ChartVersion, "helm": expectedHelm}
	if intent.ChartPath != "" {
		expectedSource["path"] = intent.ChartPath
	}
	if !jsonEqual(source, expectedSource) {
		return bindingError(hbnBindingApplicationSource)
	}
	destination, found, destinationErr := unstructured.NestedMap(application.Object, "spec", "destination")
	comparedDestination, comparedFound, comparedErr := unstructured.NestedMap(application.Object, "status", "sync", "comparedTo", "destination")
	if destinationErr != nil || comparedErr != nil || !found || !comparedFound || !jsonEqual(destination, comparedDestination) {
		return bindingError(hbnBindingApplicationStale)
	}
	cluster, _, clusterErr := unstructured.NestedString(destination, "name")
	namespace, _, namespaceErr := unstructured.NestedString(destination, "namespace")
	server, serverFound, serverErr := unstructured.NestedString(destination, "server")
	if clusterErr != nil || namespaceErr != nil || serverErr != nil || cluster != intent.DPUClusterName || namespace != intent.TargetNamespace || serverFound && server != "" {
		return bindingError(hbnBindingApplicationTarget)
	}
	return nil
}

// dpfApplicationName mirrors the pinned DPF GetApplicationName helper,
// including its five-hex digest suffix for names longer than DNS-1035's limit.
func dpfApplicationName(clusterName, dpuServiceName string) string {
	name := clusterName + "-" + dpuServiceName
	if len(name) <= 63 {
		return name
	}
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	_ = encoder.Encode(clusterName)
	_ = encoder.Encode(dpuServiceName)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(encoded.String())))
	return name[:57] + "-" + digest[:5]
}

func bindingError(reason hbnPodBindingReason) error { return &hbnPodBindingError{reason: reason} }

func podOwnedByDaemonSet(pod *corev1.Pod, ds *appsv1.DaemonSet) bool {
	if pod == nil || ds == nil {
		return false
	}
	controllers := 0
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			controllers++
			if owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.Name != ds.Name || owner.UID != ds.UID {
				return false
			}
		}
	}
	return controllers == 1
}

func namedContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	var match corev1.Container
	found := false
	for _, container := range containers {
		if container.Name != name {
			continue
		}
		if found {
			return corev1.Container{}, false
		}
		match, found = container, true
	}
	return match, found
}

// hbnPodContainers validates the complete pinned chart container cohort. A
// completed ordinary init is intentionally not required to be running/Started;
// a restartable init sidecar is. Unknown extra containers fail closed rather
// than becoming unchecked expectations copied from the observed DaemonSet.
func hbnPodContainers(pod *corev1.Pod, template *corev1.PodSpec, image hbnPodImageIdentity) ([]hbnContainerBindingAnchor, error) {
	bad := func(reason hbnPodBindingReason) ([]hbnContainerBindingAnchor, error) {
		return nil, bindingError(reason)
	}
	if pod == nil || template == nil || pod.Status.Phase != corev1.PodRunning {
		return bad(hbnBindingPodNotReady)
	}
	readyConditions := 0
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			if condition.Status != corev1.ConditionTrue {
				return bad(hbnBindingPodNotReady)
			}
			readyConditions++
		}
	}
	if readyConditions != 1 {
		return bad(hbnBindingPodNotReady)
	}
	if len(template.Containers) != 1 || len(template.InitContainers) != 2 || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 2 ||
		len(pod.Spec.EphemeralContainers) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return bad(hbnBindingPodStale)
	}
	if len(pod.Status.ContainerStatuses) != 1 || len(pod.Status.InitContainerStatuses) != 2 {
		return bad(hbnBindingPodNotReady)
	}
	required := []struct{ role, name string }{{"container", hbnContainerName}, {"init", "hbn-init"}, {"restartable-init", "hbn-sidecar"}}
	anchors := make([]hbnContainerBindingAnchor, 0, len(required))
	seenIDs := map[string]bool{}
	for _, expected := range required {
		desired, actual, statuses := template.Containers, pod.Spec.Containers, pod.Status.ContainerStatuses
		if expected.role != "container" {
			desired, actual, statuses = template.InitContainers, pod.Spec.InitContainers, pod.Status.InitContainerStatuses
		}
		wanted, wantFound := namedContainer(desired, expected.name)
		container, found := namedContainer(actual, expected.name)
		if !wantFound || !found {
			return bad(hbnBindingPodStale)
		}
		if wanted.Image != image.Reference || container.Image != image.Reference {
			return bad(hbnBindingPodImageMismatch)
		}
		for _, spec := range []corev1.Container{wanted, container} {
			if expected.role == "restartable-init" {
				if spec.RestartPolicy == nil || *spec.RestartPolicy != corev1.ContainerRestartPolicyAlways {
					return bad(hbnBindingPodStale)
				}
			} else if spec.RestartPolicy != nil {
				return bad(hbnBindingPodStale)
			}
		}
		var current corev1.ContainerStatus
		count := 0
		for _, status := range statuses {
			if status.Name == expected.name {
				current, count = status, count+1
			}
		}
		if count != 1 || current.Image == "" || current.ImageID == "" || current.RestartCount < 0 || !hbnContainerIDValid(current.ContainerID) || seenIDs[current.ContainerID] {
			return bad(hbnBindingPodNotReady)
		}
		seenIDs[current.ContainerID] = true
		if !image.matches(current.ImageID) {
			return bad(hbnBindingPodImageMismatch)
		}
		anchor := hbnContainerBindingAnchor{Role: expected.role, Name: expected.name, ContainerID: current.ContainerID, ImageID: current.ImageID,
			RuntimeImage: current.Image, RestartCount: current.RestartCount, Ready: current.Ready, StartedKnown: current.Started != nil}
		if current.Started != nil {
			anchor.Started = *current.Started
		}
		if expected.role == "init" {
			terminated := current.State.Terminated
			if terminated == nil || current.State.Running != nil || current.State.Waiting != nil || terminated.ExitCode != 0 || terminated.StartedAt.IsZero() || terminated.FinishedAt.IsZero() ||
				terminated.FinishedAt.Before(&terminated.StartedAt) || terminated.ContainerID != "" && terminated.ContainerID != current.ContainerID {
				return bad(hbnBindingPodNotReady)
			}
			anchor.StartedAt, anchor.FinishedAt, anchor.ExitCode = terminated.StartedAt.UTC().Format(time.RFC3339Nano), terminated.FinishedAt.UTC().Format(time.RFC3339Nano), terminated.ExitCode
		} else {
			running := current.State.Running
			if !current.Ready || current.Started == nil || !*current.Started || running == nil || running.StartedAt.IsZero() || current.State.Waiting != nil || current.State.Terminated != nil {
				return bad(hbnBindingPodNotReady)
			}
			anchor.StartedAt = running.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		anchors = append(anchors, anchor)
	}
	return anchors, nil
}

func validateHBNService(intent hbnPodBindingIntent, service *unstructured.Unstructured) error {
	if service == nil || service.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || service.GetKind() != "DPUService" ||
		service.GetName() != intent.DPUServiceName || service.GetNamespace() != intent.DPUServiceNamespace || string(service.GetUID()) != intent.DPUServiceUID ||
		service.GetResourceVersion() == "" || service.GetGeneration() < 1 || service.GetDeletionTimestamp() != nil ||
		!service.GetCreationTimestamp().Time.Equal(intent.DPUServiceCreatedAt) || service.GetAnnotations()[hbnServiceVersionAnnotation] == "" ||
		!jsonEqual(service.Object["spec"], intent.ExpectedServiceSpec) {
		return bindingError(hbnBindingServiceInvalid)
	}
	owners := service.GetOwnerReferences()
	if len(owners) != 1 || owners[0].APIVersion != "svc.dpu.nvidia.com/v1alpha1" || owners[0].Kind != "DPUDeployment" || owners[0].Name != intent.DPUDeploymentName ||
		string(owners[0].UID) != intent.DPUDeploymentUID || owners[0].Controller == nil || !*owners[0].Controller ||
		service.GetLabels()[activationParentLabel] != intent.DPUServiceNamespace+"_"+intent.DPUDeploymentName ||
		service.GetLabels()[hbnServiceReferenceLabel] != hbnContainerName {
		return bindingError(hbnBindingServiceInvalid)
	}
	id, _, err := unstructured.NestedString(service.Object, "spec", "serviceID")
	if err != nil || id != intent.ServiceID {
		return bindingError(hbnBindingServiceInvalid)
	}
	source, found, err := unstructured.NestedMap(service.Object, "spec", "helmChart", "source")
	if err != nil || !found {
		return bindingError(hbnBindingServiceInvalid)
	}
	// GetArgoRepoURL removes the OCI scheme in pinned DPF. Service values/spec
	// are independently retained; the App cannot validate stale values against itself.
	if strings.TrimPrefix(stringValue(source, "repoURL"), "oci://") != intent.ChartRepoURL || stringValue(source, "chart") != intent.ChartName ||
		stringValue(source, "version") != intent.ChartVersion || stringValue(source, "path") != intent.ChartPath || stringValue(source, "releaseName") != intent.ChartReleaseName {
		return bindingError(hbnBindingServiceInvalid)
	}
	return nil
}

func stringValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func hbnBindingDigest(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func hbnContainerIDValid(id string) bool {
	runtime, instance, found := strings.Cut(id, "://")
	return found && runtime != "" && instance != "" && len(id) <= 256 && !strings.ContainsAny(id, " \t\r\n\x00")
}

func (image hbnPodImageIdentity) valid() bool {
	_, digest, found := strings.Cut(image.Reference, "@")
	return found && validSHA256Digest(digest) && validSHA256Digest(image.ManifestDigest) &&
		(image.ConfigDigest == "" || validSHA256Digest(image.ConfigDigest)) && hbnImageRepository(image.Reference) != ""
}

func hbnImageRepository(reference string) string {
	repository, _, _ := strings.Cut(reference, "@")
	if tag := strings.LastIndex(repository, ":"); tag > strings.LastIndex(repository, "/") {
		repository = repository[:tag]
	}
	if repository == "" || strings.ContainsAny(repository, " \t\r\n\x00") || strings.Contains(repository, "://") {
		return ""
	}
	return repository
}

func (image hbnPodImageIdentity) matches(id string) bool {
	// RepoDigest identifies a verified platform manifest. It may differ from an
	// index in Reference. Config-only CRI IDs need the verified config mapping.
	id = strings.TrimPrefix(id, "docker-pullable://")
	if strings.Contains(id, "@") {
		return id == hbnImageRepository(image.Reference)+"@"+image.ManifestDigest
	}
	for _, prefix := range []string{"docker://", "containerd://"} {
		id = strings.TrimPrefix(id, prefix)
	}
	return image.ConfigDigest != "" && validSHA256Digest(id) && id == image.ConfigDigest
}

// sameHBNPodBinding is the caller's before/after semantic instance fence. It
// deliberately permits resourceVersion/status-time churn while rejecting a
// restart, replacement, desired revision/spec change or runtime image change.
func sameHBNPodBinding(before, after hbnPodBindingAnchor) bool {
	return before.PodUID != "" && len(before.Containers) != 0 && reflect.DeepEqual(before, after)
}
