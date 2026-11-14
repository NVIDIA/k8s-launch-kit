// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package managementprobe reads bounded DPF management-cluster facts. It never
// reads Secrets or tenant kubeconfigs and never mutates Kubernetes resources.
package managementprobe

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	pageSize = int64(256)
	maxPages = 16
	maxItems = int(pageSize) * maxPages
)

type CollectionState string

const (
	CollectionComplete    CollectionState = "complete"
	CollectionIncomplete  CollectionState = "incomplete"
	CollectionAbsent      CollectionState = "absent"
	CollectionUnavailable CollectionState = "unavailable"
)

type Issue struct {
	Source    string `json:"source" yaml:"source"`
	Resource  string `json:"resource,omitempty" yaml:"resource,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Field     string `json:"field,omitempty" yaml:"field,omitempty"`
	Code      string `json:"code" yaml:"code"`
	Message   string `json:"message" yaml:"message"`
}

type ResourceSet[T any] struct {
	State CollectionState `json:"state" yaml:"state"`
	Items []T             `json:"items" yaml:"items"`
}

type Snapshot struct {
	TenantStorage   *TenantStorageResult        `json:"tenantStorage,omitempty" yaml:"tenantStorage,omitempty"`
	Namespace       string                      `json:"namespace" yaml:"namespace"`
	ObservedAt      time.Time                   `json:"observedAt" yaml:"observedAt"`
	Deployments     ResourceSet[Deployment]     `json:"deployments" yaml:"deployments"`
	OperatorConfigs ResourceSet[OperatorConfig] `json:"operatorConfigs" yaml:"operatorConfigs"`
	DPUClusters     ResourceSet[DPUCluster]     `json:"dpuClusters" yaml:"dpuClusters"`
	DPUDevices      ResourceSet[DPUDevice]      `json:"dpuDevices" yaml:"dpuDevices"`
	DPUs            ResourceSet[DPU]            `json:"dpus" yaml:"dpus"`
	StorageClasses  ResourceSet[StorageClass]   `json:"storageClasses" yaml:"storageClasses"`
	CRDs            []CRD                       `json:"crds" yaml:"crds"`
	Issues          []Issue                     `json:"issues,omitempty" yaml:"issues,omitempty"`
}

type Condition struct {
	Type               string `json:"type" yaml:"type"`
	Status             string `json:"status" yaml:"status"`
	Reason             string `json:"reason,omitempty" yaml:"reason,omitempty"`
	ObservedGeneration *int64 `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Name       string `json:"name" yaml:"name"`
	UID        string `json:"uid" yaml:"uid"`
	Controller bool   `json:"controller" yaml:"controller"`
}

type Deployment struct {
	Name               string      `json:"name" yaml:"name"`
	UID                string      `json:"uid" yaml:"uid"`
	Generation         int64       `json:"generation" yaml:"generation"`
	ObservedGeneration int64       `json:"observedGeneration" yaml:"observedGeneration"`
	StatusFreshness    string      `json:"statusFreshness" yaml:"statusFreshness"`
	Replicas           *int32      `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	ReadyReplicas      int32       `json:"readyReplicas" yaml:"readyReplicas"`
	AvailableReplicas  int32       `json:"availableReplicas" yaml:"availableReplicas"`
	Images             []string    `json:"images" yaml:"images"`
	Conditions         []Condition `json:"conditions" yaml:"conditions"`
}

type ObjectSummary struct {
	Name               string           `json:"name" yaml:"name"`
	Namespace          string           `json:"namespace" yaml:"namespace"`
	UID                string           `json:"uid" yaml:"uid"`
	Generation         int64            `json:"generation" yaml:"generation"`
	ObservedGeneration int64            `json:"observedGeneration" yaml:"observedGeneration"`
	StatusFreshness    string           `json:"statusFreshness" yaml:"statusFreshness"`
	Owners             []OwnerReference `json:"owners" yaml:"owners"`
	Conditions         []Condition      `json:"conditions" yaml:"conditions"`
}

type OperatorConfig struct {
	ObjectSummary  `yaml:",inline"`
	DeploymentMode *string `json:"deploymentMode,omitempty" yaml:"deploymentMode,omitempty"`
}

type DPUCluster struct {
	ObjectSummary             `yaml:",inline"`
	ResourceVersion           string             `json:"resourceVersion" yaml:"resourceVersion"`
	Type                      *string            `json:"type,omitempty" yaml:"type,omitempty"`
	Phase                     *string            `json:"phase,omitempty" yaml:"phase,omitempty"`
	NodesCount                *int64             `json:"nodesCount,omitempty" yaml:"nodesCount,omitempty"`
	Version                   *string            `json:"version,omitempty" yaml:"version,omitempty"`
	VIP                       *string            `json:"vip,omitempty" yaml:"vip,omitempty"`
	Interface                 *string            `json:"interface,omitempty" yaml:"interface,omitempty"`
	VirtualRouterID           *int64             `json:"virtualRouterID,omitempty" yaml:"virtualRouterID,omitempty"`
	KeepalivedPresent         *bool              `json:"keepalivedPresent,omitempty" yaml:"keepalivedPresent,omitempty"`
	NodeSelector              *map[string]string `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	LaunchKitTenantLabelKey   *string            `json:"launchKitTenantLabelKey,omitempty" yaml:"launchKitTenantLabelKey,omitempty"`
	LaunchKitTenantLabelValue *string            `json:"launchKitTenantLabelValue,omitempty" yaml:"launchKitTenantLabelValue,omitempty"`
	TenantIdentityConflict    bool               `json:"tenantIdentityConflict,omitempty" yaml:"tenantIdentityConflict,omitempty"`
}

type DPUDevice struct {
	ObjectSummary      `yaml:",inline"`
	SpecSerialNumber   *string `json:"specSerialNumber,omitempty" yaml:"specSerialNumber,omitempty"`
	StatusSerialNumber *string `json:"statusSerialNumber,omitempty" yaml:"statusSerialNumber,omitempty"`
	PSID               *string `json:"psid,omitempty" yaml:"psid,omitempty"`
}

type DPU struct {
	ObjectSummary  `yaml:",inline"`
	SerialNumber   *string `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	DeviceName     *string `json:"deviceName,omitempty" yaml:"deviceName,omitempty"`
	NodeName       *string `json:"nodeName,omitempty" yaml:"nodeName,omitempty"`
	Phase          *string `json:"phase,omitempty" yaml:"phase,omitempty"`
	DeploymentMode *string `json:"deploymentMode,omitempty" yaml:"deploymentMode,omitempty"`
	DPUType        *string `json:"dpuType,omitempty" yaml:"dpuType,omitempty"`
}

type StorageClass struct {
	Name                 string  `json:"name" yaml:"name"`
	Provisioner          string  `json:"provisioner" yaml:"provisioner"`
	VolumeBindingMode    *string `json:"volumeBindingMode,omitempty" yaml:"volumeBindingMode,omitempty"`
	ReclaimPolicy        *string `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`
	AllowVolumeExpansion *bool   `json:"allowVolumeExpansion,omitempty" yaml:"allowVolumeExpansion,omitempty"`
}

type CRDVersion struct {
	Name    string `json:"name" yaml:"name"`
	Served  bool   `json:"served" yaml:"served"`
	Storage bool   `json:"storage" yaml:"storage"`
}

type SchemaField struct {
	Path      string   `json:"path" yaml:"path"`
	PresentIn []string `json:"presentIn" yaml:"presentIn"`
}

type CRD struct {
	Name               string        `json:"name" yaml:"name"`
	Group              string        `json:"group" yaml:"group"`
	Plural             string        `json:"plural" yaml:"plural"`
	Kind               string        `json:"kind" yaml:"kind"`
	Scope              string        `json:"scope" yaml:"scope"`
	UID                string        `json:"uid,omitempty" yaml:"uid,omitempty"`
	Generation         int64         `json:"generation,omitempty" yaml:"generation,omitempty"`
	ObservedGeneration int64         `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	StatusFreshness    string        `json:"statusFreshness" yaml:"statusFreshness"`
	State              string        `json:"state" yaml:"state"`
	Versions           []CRDVersion  `json:"versions" yaml:"versions"`
	Fields             []SchemaField `json:"fields,omitempty" yaml:"fields,omitempty"`
	Conditions         []Condition   `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Collector uses injected clients so observation behavior is testable without a
// cluster. Kubernetes is used for standard resources, Dynamic for DPF objects,
// and Extensions for CRD schema discovery.
type Collector struct {
	Kubernetes kubernetes.Interface
	Dynamic    dynamic.Interface
	Extensions apiextensionsclient.Interface
}

type crdSpec struct {
	name, group, plural, kind string
	fields                    []string
}

var crds = []crdSpec{
	{name: "bluefieldsoftwares.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "bluefieldsoftwares", kind: "BlueFieldSoftware", fields: []string{"spec.osIso", "spec.pldmFwBundle", "status.versions"}},
	{name: "dpudeployments.svc.dpu.nvidia.com", group: "svc.dpu.nvidia.com", plural: "dpudeployments", kind: "DPUDeployment", fields: []string{"spec.dpus.bfb", "spec.dpus.blueFieldSoftware", "spec.dpus.dpuSets[].nameSuffix", "spec.dpus.dpuSetStrategy", "spec.dpus.nodeEffect"}},
	{name: "dpuflavors.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpuflavors", kind: "DPUFlavor", fields: []string{"spec.configFiles"}},
	{name: "dpfoperatorconfigs.operator.dpu.nvidia.com", group: "operator.dpu.nvidia.com", plural: "dpfoperatorconfigs", kind: "DPFOperatorConfig", fields: []string{"spec.deploymentMode", "status.conditions"}},
	{name: "dpuclusters.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpuclusters", kind: "DPUCluster", fields: []string{"spec.type", "spec.clusterEndpoint.keepalived.vip", "spec.clusterEndpoint.keepalived.interface", "spec.clusterEndpoint.keepalived.virtualRouterID", "spec.clusterEndpoint.keepalived.nodeSelector", "status.phase", "status.nodesCount"}},
	{name: "dpudevices.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpudevices", kind: "DPUDevice", fields: []string{"spec.serialNumber", "status.serialNumber", "status.psid"}},
	{name: "dpus.provisioning.dpu.nvidia.com", group: "provisioning.dpu.nvidia.com", plural: "dpus", kind: "DPU", fields: []string{"spec.serialNumber", "spec.dpuDeviceName", "spec.dpuNodeName", "status.phase"}},
}

type crdRead struct {
	spec crdSpec
	crd  CRD
}

// Collect reads existing management-cluster state in an explicit namespace.
// It reports observations and source issues only; it does not decide whether
// the cluster is ready for generation or deployment.
func (c Collector) Collect(ctx context.Context, namespace string) Snapshot {
	result := Snapshot{
		Namespace:       namespace,
		ObservedAt:      time.Now().UTC(),
		Deployments:     ResourceSet[Deployment]{State: CollectionUnavailable, Items: []Deployment{}},
		OperatorConfigs: ResourceSet[OperatorConfig]{State: CollectionUnavailable, Items: []OperatorConfig{}},
		DPUClusters:     ResourceSet[DPUCluster]{State: CollectionUnavailable, Items: []DPUCluster{}},
		DPUDevices:      ResourceSet[DPUDevice]{State: CollectionUnavailable, Items: []DPUDevice{}},
		DPUs:            ResourceSet[DPU]{State: CollectionUnavailable, Items: []DPU{}},
		StorageClasses:  ResourceSet[StorageClass]{State: CollectionUnavailable, Items: []StorageClass{}},
		CRDs:            []CRD{},
		Issues:          []Issue{},
	}
	if ctx == nil {
		result.Issues = append(result.Issues, Issue{Source: "collector", Code: "INVALID_CONTEXT", Message: "A management observation context is required."})
		return result
	}
	if namespace == "" || len(validation.IsDNS1123Label(namespace)) > 0 {
		result.Issues = append(result.Issues, Issue{Source: "collector", Field: "namespace", Code: "INVALID_NAMESPACE", Message: "A valid explicit namespace is required."})
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Issues = append(result.Issues, issueForError("collector", "", "", err))
		return result
	}

	crdReads := c.readCRDs(ctx, &result)
	if c.Kubernetes == nil {
		result.Issues = append(result.Issues, Issue{Source: "kubernetes", Code: "CLIENT_UNAVAILABLE", Message: "The Kubernetes client is not configured."})
	} else {
		result.Deployments = c.listDeployments(ctx, namespace, &result.Issues)
		result.StorageClasses = c.listStorageClasses(ctx, &result.Issues)
	}
	if c.Dynamic == nil {
		result.Issues = append(result.Issues, Issue{Source: "dynamic", Code: "CLIENT_UNAVAILABLE", Message: "The DPF resource client is not configured."})
	} else {
		result.OperatorConfigs = listCustom[OperatorConfig](ctx, c.Dynamic, namespace, findCRD(crdReads, "dpfoperatorconfigs.operator.dpu.nvidia.com"), &result.Issues)
		result.DPUClusters = listCustom[DPUCluster](ctx, c.Dynamic, metav1.NamespaceAll, findCRD(crdReads, "dpuclusters.provisioning.dpu.nvidia.com"), &result.Issues)
		deviceSpecs := findCRD(crdReads, "dpudevices.provisioning.dpu.nvidia.com")
		result.DPUDevices = listCustom[DPUDevice](ctx, c.Dynamic, metav1.NamespaceAll, deviceSpecs, &result.Issues)
		dpuSpecs := findCRD(crdReads, "dpus.provisioning.dpu.nvidia.com")
		result.DPUs = listCustom[DPU](ctx, c.Dynamic, metav1.NamespaceAll, dpuSpecs, &result.Issues)
	}
	storage := c.CollectTenantStorage(ctx, namespace)
	result.TenantStorage = &storage
	return result
}

func findCRD(reads []crdRead, name string) crdRead {
	for _, read := range reads {
		if read.spec.name == name {
			return read
		}
	}
	return crdRead{spec: crdSpec{name: name}, crd: CRD{Name: name, State: "unavailable"}}
}

func (c Collector) readCRDs(ctx context.Context, result *Snapshot) []crdRead {
	reads := make([]crdRead, 0, len(crds))
	if c.Extensions == nil {
		for _, spec := range crds {
			read := crdRead{spec: spec, crd: CRD{Name: spec.name, Group: spec.group, Plural: spec.plural, Kind: spec.kind, State: "unavailable", Versions: []CRDVersion{}}}
			reads = append(reads, read)
			result.CRDs = append(result.CRDs, read.crd)
		}
		result.Issues = append(result.Issues, Issue{Source: "crds", Code: "CLIENT_UNAVAILABLE", Message: "The CRD client is not configured."})
		return reads
	}
	for _, spec := range crds {
		obj, err := c.Extensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, spec.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			read := crdRead{spec: spec, crd: CRD{Name: spec.name, Group: spec.group, Plural: spec.plural, Kind: spec.kind, State: "absent", Versions: []CRDVersion{}, Fields: schemaFields(nil, spec.fields)}}
			reads = append(reads, read)
			result.CRDs = append(result.CRDs, read.crd)
			continue
		}
		if err != nil {
			read := crdRead{spec: spec, crd: CRD{Name: spec.name, Group: spec.group, Plural: spec.plural, Kind: spec.kind, State: "unavailable", Versions: []CRDVersion{}, Fields: []SchemaField{}}}
			reads = append(reads, read)
			result.CRDs = append(result.CRDs, read.crd)
			result.Issues = append(result.Issues, issueForError("crds", spec.name, "", err))
			continue
		}
		read := crdRead{spec: spec, crd: observeCRD(obj, spec, &result.Issues)}
		reads = append(reads, read)
		result.CRDs = append(result.CRDs, read.crd)
	}
	return reads
}

func observeCRD(obj *v1.CustomResourceDefinition, spec crdSpec, issues *[]Issue) CRD {
	result := CRD{
		Name: obj.Name, Group: obj.Spec.Group, Plural: obj.Spec.Names.Plural, Kind: obj.Spec.Names.Kind,
		Scope: string(obj.Spec.Scope), UID: string(obj.UID), Generation: obj.Generation,
		ObservedGeneration: obj.Status.ObservedGeneration, StatusFreshness: "unknown", State: "observed", Versions: []CRDVersion{},
		Fields: []SchemaField{}, Conditions: []Condition{},
	}
	if result.Group != spec.group || result.Plural != spec.plural || result.Kind != spec.kind {
		result.State = "invalid"
		*issues = append(*issues, Issue{Source: "crds", Resource: spec.name, Field: "spec.names", Code: "API_SCHEMA_IDENTITY_MISMATCH", Message: "The served CRD identity does not match the expected DPF API."})
	}
	for _, version := range obj.Spec.Versions {
		result.Versions = append(result.Versions, CRDVersion{Name: version.Name, Served: version.Served, Storage: version.Storage})
	}
	sort.Slice(result.Versions, func(i, j int) bool { return result.Versions[i].Name < result.Versions[j].Name })
	result.Fields = schemaFields(obj.Spec.Versions, spec.fields)
	for _, condition := range obj.Status.Conditions {
		result.Conditions = append(result.Conditions, Condition{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason})
	}
	if obj.Status.ObservedGeneration > 0 && obj.Status.ObservedGeneration < obj.Generation {
		result.StatusFreshness = "stale"
		*issues = append(*issues, Issue{Source: "crds", Resource: spec.name, Field: "status.observedGeneration", Code: "STATUS_STALE", Message: "CRD controller status does not reflect the current CRD generation."})
	} else if obj.Status.ObservedGeneration > 0 && obj.Status.ObservedGeneration >= obj.Generation && crdStatusCurrent(obj) {
		result.StatusFreshness = "current"
	}
	return result
}

func crdStatusCurrent(obj *v1.CustomResourceDefinition) bool {
	var established, namesAccepted bool
	for _, condition := range obj.Status.Conditions {
		switch condition.Type {
		case v1.Established:
			established = condition.Status == v1.ConditionTrue
		case v1.NamesAccepted:
			namesAccepted = condition.Status == v1.ConditionTrue
		}
	}
	served := false
	for _, version := range obj.Spec.Versions {
		served = served || version.Served
	}
	return established && namesAccepted && served
}

func schemaFields(versions []v1.CustomResourceDefinitionVersion, paths []string) []SchemaField {
	result := make([]SchemaField, 0, len(paths))
	for _, path := range paths {
		field := SchemaField{Path: path, PresentIn: []string{}}
		for _, version := range versions {
			if !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			if schemaPathPresent(version.Schema.OpenAPIV3Schema, strings.Split(path, ".")) {
				field.PresentIn = append(field.PresentIn, version.Name)
			}
		}
		sort.Strings(field.PresentIn)
		result = append(result, field)
	}
	return result
}

func schemaPathPresent(root *v1.JSONSchemaProps, path []string) bool {
	current := root
	for _, pathPart := range path {
		arrayItem := strings.HasSuffix(pathPart, "[]")
		part := strings.TrimSuffix(pathPart, "[]")
		next, ok := current.Properties[part]
		if !ok {
			return false
		}
		if arrayItem {
			if next.Type != "array" || next.Items == nil || next.Items.Schema == nil {
				return false
			}
			next = *next.Items.Schema
		}
		current = &next
	}
	return true
}

func listCustom[T any](ctx context.Context, client dynamic.Interface, namespace string, read crdRead, issues *[]Issue) ResourceSet[T] {
	if read.crd.State == "absent" {
		return ResourceSet[T]{State: CollectionAbsent, Items: []T{}}
	}
	if read.crd.State != "observed" {
		return ResourceSet[T]{State: CollectionUnavailable, Items: []T{}}
	}
	version := servedVersion(read.crd.Versions)
	if version == "" {
		*issues = append(*issues, Issue{Source: read.spec.name, Code: "API_VERSION_UNSERVED", Message: "The DPF resource has no served API version."})
		return ResourceSet[T]{State: CollectionUnavailable, Items: []T{}}
	}
	gvr := schema.GroupVersionResource{Group: read.spec.group, Version: version, Resource: read.spec.plural}
	objects, state := listUnstructured(ctx, client.Resource(gvr).Namespace(namespace), read.spec.name, issues)
	set := ResourceSet[T]{State: state, Items: make([]T, 0, len(objects))}
	for _, obj := range objects {
		issueStart := len(*issues)
		item, ok := convertCustom[T](obj, read.spec, issues)
		for index := issueStart; index < len(*issues); index++ {
			if (*issues)[index].Namespace == "" {
				(*issues)[index].Namespace = obj.GetNamespace()
			}
		}
		if ok {
			set.Items = append(set.Items, item)
		}
	}
	return set
}

func servedVersion(versions []CRDVersion) string {
	for _, version := range versions {
		if version.Name == "v1alpha1" && version.Served {
			return version.Name
		}
	}
	for _, version := range versions {
		if version.Served {
			return version.Name
		}
	}
	return ""
}

func listUnstructured(ctx context.Context, resource dynamic.ResourceInterface, source string, issues *[]Issue) ([]unstructured.Unstructured, CollectionState) {
	result := make([]unstructured.Unstructured, 0)
	continuation := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		items, err := resource.List(ctx, metav1.ListOptions{Limit: pageSize, Continue: continuation})
		if err != nil {
			*issues = append(*issues, issueForError(source, "", "", err))
			state := CollectionUnavailable
			if len(result) > 0 {
				state = CollectionIncomplete
			}
			return result, state
		}
		remaining := maxItems - len(result)
		if remaining <= 0 {
			*issues = append(*issues, coverageIssue(source, "The resource collection reached its bounded item limit."))
			return result, CollectionIncomplete
		}
		take := len(items.Items)
		if take > remaining {
			take = remaining
		}
		result = append(result, items.Items[:take]...)
		if take < len(items.Items) {
			*issues = append(*issues, coverageIssue(source, "The API returned more items than the bounded collection can retain."))
			return result, CollectionIncomplete
		}
		next := items.GetContinue()
		if next == "" {
			return result, CollectionComplete
		}
		if seen[next] {
			*issues = append(*issues, Issue{Source: source, Code: "API_PAGINATION_INVALID", Message: "The API repeated a pagination token; collection is incomplete."})
			return result, CollectionIncomplete
		}
		seen[next] = true
		continuation = next
	}
	*issues = append(*issues, coverageIssue(source, "The resource collection exceeded its bounded page limit."))
	return result, CollectionIncomplete
}

func (c Collector) listDeployments(ctx context.Context, namespace string, issues *[]Issue) ResourceSet[Deployment] {
	result := ResourceSet[Deployment]{State: CollectionUnavailable, Items: []Deployment{}}
	items := make([]appsv1.Deployment, 0)
	continuation := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		list, err := c.Kubernetes.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: pageSize, Continue: continuation})
		if err != nil {
			*issues = append(*issues, issueForError("deployments", "", "", err))
			if len(items) > 0 {
				result.State = CollectionIncomplete
			}
			result.Items = convertDeployments(items, issues)
			return result
		}
		remaining := maxItems - len(items)
		take := len(list.Items)
		if take > remaining {
			take = remaining
		}
		items = append(items, list.Items[:take]...)
		if take < len(list.Items) {
			*issues = append(*issues, coverageIssue("deployments", "The API returned more items than the bounded collection can retain."))
			result.State = CollectionIncomplete
			result.Items = convertDeployments(items, issues)
			return result
		}
		next := list.Continue
		if next == "" {
			result.State = CollectionComplete
			result.Items = convertDeployments(items, issues)
			return result
		}
		if seen[next] {
			*issues = append(*issues, Issue{Source: "deployments", Code: "API_PAGINATION_INVALID", Message: "The API repeated a pagination token; collection is incomplete."})
			result.State = CollectionIncomplete
			result.Items = convertDeployments(items, issues)
			return result
		}
		seen[next] = true
		continuation = next
	}
	*issues = append(*issues, coverageIssue("deployments", "The resource collection exceeded its bounded page limit."))
	result.State = CollectionIncomplete
	result.Items = convertDeployments(items, issues)
	return result
}

func (c Collector) listStorageClasses(ctx context.Context, issues *[]Issue) ResourceSet[StorageClass] {
	result := ResourceSet[StorageClass]{State: CollectionUnavailable, Items: []StorageClass{}}
	items := make([]storagev1.StorageClass, 0)
	continuation := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		list, err := c.Kubernetes.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{Limit: pageSize, Continue: continuation})
		if err != nil {
			*issues = append(*issues, issueForError("storageClasses", "", "", err))
			if len(items) > 0 {
				result.State = CollectionIncomplete
			}
			result.Items = convertStorageClasses(items)
			return result
		}
		remaining := maxItems - len(items)
		take := len(list.Items)
		if take > remaining {
			take = remaining
		}
		items = append(items, list.Items[:take]...)
		if take < len(list.Items) {
			*issues = append(*issues, coverageIssue("storageClasses", "The API returned more items than the bounded collection can retain."))
			result.State = CollectionIncomplete
			result.Items = convertStorageClasses(items)
			return result
		}
		next := list.Continue
		if next == "" {
			result.State = CollectionComplete
			result.Items = convertStorageClasses(items)
			return result
		}
		if seen[next] {
			*issues = append(*issues, Issue{Source: "storageClasses", Code: "API_PAGINATION_INVALID", Message: "The API repeated a pagination token; collection is incomplete."})
			result.State = CollectionIncomplete
			result.Items = convertStorageClasses(items)
			return result
		}
		seen[next] = true
		continuation = next
	}
	*issues = append(*issues, coverageIssue("storageClasses", "The resource collection exceeded its bounded page limit."))
	result.State = CollectionIncomplete
	result.Items = convertStorageClasses(items)
	return result
}

func convertStorageClasses(items []storagev1.StorageClass) []StorageClass {
	result := make([]StorageClass, 0, len(items))
	for _, item := range items {
		var bindingMode, reclaimPolicy *string
		if item.VolumeBindingMode != nil {
			value := string(*item.VolumeBindingMode)
			bindingMode = &value
		}
		if item.ReclaimPolicy != nil {
			value := string(*item.ReclaimPolicy)
			reclaimPolicy = &value
		}
		result = append(result, StorageClass{Name: item.Name, Provisioner: item.Provisioner, VolumeBindingMode: bindingMode, ReclaimPolicy: reclaimPolicy, AllowVolumeExpansion: item.AllowVolumeExpansion})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func convertCustom[T any](obj unstructured.Unstructured, spec crdSpec, issues *[]Issue) (T, bool) {
	var zero T
	owners := ownerRefs(obj.GetOwnerReferences())
	summary := ObjectSummary{Name: obj.GetName(), Namespace: obj.GetNamespace(), UID: string(obj.GetUID()), Generation: obj.GetGeneration(), Owners: owners, Conditions: readConditions(obj, spec, issues)}
	if observed, found, err := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); err == nil && found {
		summary.ObservedGeneration = observed
		summary.StatusFreshness = generationState(summary.Generation, observed, true)
	} else {
		conditionObserved := false
		stale := false
		for _, condition := range summary.Conditions {
			if condition.ObservedGeneration != nil {
				conditionObserved = true
				if *condition.ObservedGeneration < summary.Generation {
					stale = true
				}
			}
		}
		switch {
		case stale:
			summary.StatusFreshness = "stale"
		case conditionObserved:
			summary.StatusFreshness = "current"
		default:
			summary.StatusFreshness = "unknown"
		}
	}
	switch spec.kind {
	case "DPFOperatorConfig":
		mode, found, err := unstructured.NestedString(obj.Object, "spec", "deploymentMode")
		if err != nil {
			*issues = append(*issues, malformedIssue(spec, obj.GetName(), "spec.deploymentMode"))
			return zero, false
		}
		item := OperatorConfig{ObjectSummary: summary}
		if found {
			item.DeploymentMode = &mode
		}
		return any(item).(T), true
	case "DPUCluster":
		item := DPUCluster{ObjectSummary: summary, ResourceVersion: obj.GetResourceVersion()}
		setStringField(obj, spec, "spec.type", &item.Type, issues)
		setStringField(obj, spec, "spec.clusterEndpoint.keepalived.vip", &item.VIP, issues)
		setStringField(obj, spec, "spec.clusterEndpoint.keepalived.interface", &item.Interface, issues)
		if _, found, err := unstructured.NestedMap(obj.Object, "spec", "clusterEndpoint"); err != nil {
			*issues = append(*issues, malformedObjectIssue(spec, obj, "spec.clusterEndpoint"))
		} else if !found {
			present := false
			item.KeepalivedPresent = &present
		} else if _, found, err := unstructured.NestedMap(obj.Object, "spec", "clusterEndpoint", "keepalived"); err != nil {
			*issues = append(*issues, malformedObjectIssue(spec, obj, "spec.clusterEndpoint.keepalived"))
		} else {
			present := found
			item.KeepalivedPresent = &present
		}
		if value, found, err := unstructured.NestedInt64(obj.Object, "spec", "clusterEndpoint", "keepalived", "virtualRouterID"); err == nil && found {
			item.VirtualRouterID = &value
		} else if err != nil {
			*issues = append(*issues, malformedObjectIssue(spec, obj, "spec.clusterEndpoint.keepalived.virtualRouterID"))
		}
		if selector, found, err := unstructured.NestedStringMap(obj.Object, "spec", "clusterEndpoint", "keepalived", "nodeSelector"); err != nil {
			*issues = append(*issues, malformedObjectIssue(spec, obj, "spec.clusterEndpoint.keepalived.nodeSelector"))
		} else if found {
			if !validNodeSelector(selector) {
				*issues = append(*issues, malformedObjectIssue(spec, obj, "spec.clusterEndpoint.keepalived.nodeSelector"))
			} else {
				copy := maps.Clone(selector)
				item.NodeSelector = &copy
			}
		}
		for _, key := range []string{"nvidia.kubernetes-launch-kit.dpf-tenant", "launch-kit.nvidia.com/dpf-tenant"} {
			value, found, err := unstructured.NestedString(obj.Object, "metadata", "labels", key)
			if err != nil {
				*issues = append(*issues, malformedIssue(spec, obj.GetName(), "metadata.labels."+key))
				continue
			}
			if found {
				if item.LaunchKitTenantLabelKey != nil && *item.LaunchKitTenantLabelValue != value {
					item.TenantIdentityConflict = true
					*issues = append(*issues, Issue{Source: spec.name, Resource: obj.GetName(), Field: "metadata.labels", Code: "TENANT_LABEL_CONFLICT", Message: "DPUCluster has conflicting Launch Kit tenant identity labels."})
					continue
				}
				item.LaunchKitTenantLabelKey, item.LaunchKitTenantLabelValue = &key, &value
			}
		}
		setStringField(obj, spec, "status.phase", &item.Phase, issues)
		if value, found, err := unstructured.NestedInt64(obj.Object, "status", "nodesCount"); err == nil && found {
			item.NodesCount = &value
		} else if err != nil {
			*issues = append(*issues, malformedIssue(spec, obj.GetName(), "status.nodesCount"))
		}
		setStringField(obj, spec, "status.version", &item.Version, issues)
		return any(item).(T), true
	case "DPUDevice":
		item := DPUDevice{ObjectSummary: summary}
		setStringField(obj, spec, "spec.serialNumber", &item.SpecSerialNumber, issues)
		setStringField(obj, spec, "status.serialNumber", &item.StatusSerialNumber, issues)
		setStringField(obj, spec, "status.psid", &item.PSID, issues)
		if item.SpecSerialNumber != nil && item.StatusSerialNumber != nil && *item.SpecSerialNumber != *item.StatusSerialNumber {
			*issues = append(*issues, Issue{Source: spec.name, Resource: obj.GetName(), Field: "status.serialNumber", Code: "IDENTITY_CONFLICT", Message: "DPUDevice spec and status serial numbers disagree."})
		}
		return any(item).(T), true
	case "DPU":
		item := DPU{ObjectSummary: summary}
		setStringField(obj, spec, "spec.serialNumber", &item.SerialNumber, issues)
		setStringField(obj, spec, "spec.dpuDeviceName", &item.DeviceName, issues)
		setStringField(obj, spec, "spec.dpuNodeName", &item.NodeName, issues)
		setStringField(obj, spec, "status.phase", &item.Phase, issues)
		setStringField(obj, spec, "status.deploymentMode", &item.DeploymentMode, issues)
		setStringField(obj, spec, "status.dpuType", &item.DPUType, issues)
		return any(item).(T), true
	default:
		*issues = append(*issues, Issue{Source: spec.name, Resource: obj.GetName(), Code: "API_KIND_UNSUPPORTED", Message: "The DPF resource kind has no typed observation model."})
		return zero, false
	}
}

func readConditions(obj unstructured.Unstructured, spec crdSpec, issues *[]Issue) []Condition {
	items, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		*issues = append(*issues, malformedIssue(spec, obj.GetName(), "status.conditions"))
		return []Condition{}
	}
	if !found {
		return []Condition{}
	}
	result := make([]Condition, 0, len(items))
	for i, item := range items {
		raw, ok := item.(map[string]any)
		if !ok {
			*issues = append(*issues, malformedIssue(spec, obj.GetName(), fmt.Sprintf("status.conditions[%d]", i)))
			continue
		}
		condition := Condition{}
		if condition.Type, ok = raw["type"].(string); !ok {
			*issues = append(*issues, malformedIssue(spec, obj.GetName(), fmt.Sprintf("status.conditions[%d].type", i)))
			continue
		}
		if condition.Status, ok = raw["status"].(string); !ok {
			*issues = append(*issues, malformedIssue(spec, obj.GetName(), fmt.Sprintf("status.conditions[%d].status", i)))
			continue
		}
		condition.Reason, _ = raw["reason"].(string)
		if value, found := raw["observedGeneration"]; found {
			switch n := value.(type) {
			case int64:
				condition.ObservedGeneration = &n
			case int32:
				converted := int64(n)
				condition.ObservedGeneration = &converted
			case float64:
				converted := int64(n)
				if float64(converted) != n {
					*issues = append(*issues, malformedIssue(spec, obj.GetName(), fmt.Sprintf("status.conditions[%d].observedGeneration", i)))
				} else {
					condition.ObservedGeneration = &converted
				}
			default:
				*issues = append(*issues, malformedIssue(spec, obj.GetName(), fmt.Sprintf("status.conditions[%d].observedGeneration", i)))
			}
		}
		result = append(result, condition)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}

func setStringField(obj unstructured.Unstructured, spec crdSpec, path string, dest **string, issues *[]Issue) {
	parts := strings.Split(path, ".")
	value, found, err := unstructured.NestedString(obj.Object, parts...)
	if err != nil {
		*issues = append(*issues, malformedIssue(spec, obj.GetName(), path))
		return
	}
	if found {
		*dest = &value
	}
}

func ownerRefs(references []metav1.OwnerReference) []OwnerReference {
	result := make([]OwnerReference, 0, len(references))
	for _, owner := range references {
		result = append(result, OwnerReference{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: string(owner.UID), Controller: owner.Controller != nil && *owner.Controller})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].UID < result[j].UID
	})
	return result
}

func generationState(generation, observed int64, present bool) string {
	if !present {
		return "unknown"
	}
	if observed < generation {
		return "stale"
	}
	return "current"
}

func convertDeployments(items []appsv1.Deployment, issues *[]Issue) []Deployment {
	result := make([]Deployment, 0, len(items))
	for _, item := range items {
		images := make([]string, 0)
		for _, container := range item.Spec.Template.Spec.InitContainers {
			if container.Image != "" {
				images = append(images, container.Image)
			}
		}
		for _, container := range item.Spec.Template.Spec.Containers {
			if container.Image != "" {
				images = append(images, container.Image)
			}
		}
		sort.Strings(images)
		images = unique(images)
		conditions := make([]Condition, 0, len(item.Status.Conditions))
		for _, condition := range item.Status.Conditions {
			conditions = append(conditions, Condition{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason})
		}
		freshness := generationState(item.Generation, item.Status.ObservedGeneration, item.Status.ObservedGeneration > 0 || item.Generation == 0)
		result = append(result, Deployment{
			Name: item.Name, UID: string(item.UID), Generation: item.Generation,
			ObservedGeneration: item.Status.ObservedGeneration, StatusFreshness: freshness,
			Replicas: item.Spec.Replicas, ReadyReplicas: item.Status.ReadyReplicas,
			AvailableReplicas: item.Status.AvailableReplicas, Images: images, Conditions: conditions,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func issueForError(source, resource, field string, err error) Issue {
	code := "API_INSPECTION_FAILED"
	message := "The Kubernetes API request failed; no complete observation was obtained."
	switch {
	case apierrors.IsForbidden(err):
		code, message = "API_ACCESS_DENIED", "The configured Kubernetes identity is forbidden from reading this resource."
	case apierrors.IsUnauthorized(err):
		code, message = "API_AUTHENTICATION_FAILED", "The Kubernetes API did not accept the configured credentials."
	case apierrors.IsNotFound(err):
		code, message = "API_RESOURCE_NOT_FOUND", "The requested API resource was not found."
	case errors.Is(err, context.Canceled):
		code, message = "API_CANCELLED", "The Kubernetes API inspection was cancelled."
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err):
		code, message = "API_TIMEOUT", "The Kubernetes API inspection timed out."
	}
	return Issue{Source: source, Resource: resource, Field: field, Code: code, Message: message}
}

func malformedIssue(spec crdSpec, resource, field string) Issue {
	return Issue{Source: spec.name, Resource: resource, Field: field, Code: "API_RESPONSE_INVALID", Message: "The DPF API object does not match the expected typed observation fields."}
}

func malformedObjectIssue(spec crdSpec, object unstructured.Unstructured, field string) Issue {
	issue := malformedIssue(spec, object.GetName(), field)
	issue.Namespace = object.GetNamespace()
	return issue
}

func validNodeSelector(selector map[string]string) bool {
	for key, value := range selector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return false
		}
	}
	return true
}

func coverageIssue(source, message string) Issue {
	return Issue{Source: source, Code: "API_COVERAGE_INCOMPLETE", Message: message}
}

func unique(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
