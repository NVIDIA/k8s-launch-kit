// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"sort"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const defaultCleanPollInterval = 2 * time.Second

// Cluster-scoped custom resources cannot be selected by namespace. Keep this
// list deliberately narrow: these are the cluster-scoped kinds rendered and
// owned by the Network Operator flow. All other cluster-scoped CRs are outside
// l8k clean's deletion boundary.
var clusterScopedCleanKinds = []schema.GroupVersionKind{
	{Group: "mellanox.com", Version: "v1alpha1", Kind: "HostDeviceNetwork"},
	{Group: "mellanox.com", Version: "v1alpha1", Kind: "IPoIBNetwork"},
	{Group: "mellanox.com", Version: "v1alpha1", Kind: "MacvlanNetwork"},
	{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicNodePolicy"},
	// NicClusterPolicy is last so its controller remains available while the
	// dependent custom resources above process deletion and finalizers.
	{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicy"},
}

// CleanOptions controls Network Operator teardown.
type CleanOptions struct {
	Namespace     string
	KeepHelmChart bool
	RestConfig    *rest.Config
	PollInterval  time.Duration
	HelmTimeout   time.Duration
}

// CleanResult summarizes mutations performed by Clean.
type CleanResult struct {
	Namespace              string
	CustomResourcesDeleted int
	HelmReleaseRemoved     bool
}

type cleanObjectRef struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

type helmUninstallFunc func(context.Context, *rest.Config, string, time.Duration) (bool, error)

// Clean deletes Network Operator custom resources and then uninstalls the
// Helm release unless KeepHelmChart is set. Helm is always last so controllers
// remain available to process custom-resource finalizers.
func Clean(
	ctx context.Context,
	kubeClient client.Client,
	opts CleanOptions,
) (CleanResult, error) {
	return cleanWithUninstaller(ctx, kubeClient, opts, Uninstall)
}

func cleanWithUninstaller(
	ctx context.Context,
	kubeClient client.Client,
	opts CleanOptions,
	uninstall helmUninstallFunc,
) (CleanResult, error) {
	result := CleanResult{Namespace: opts.Namespace}
	if ctx == nil {
		ctx = context.Background()
	}
	if kubeClient == nil {
		return result, fmt.Errorf("clean network operator: nil Kubernetes client")
	}
	if opts.Namespace == "" {
		return result, fmt.Errorf("clean network operator: namespace is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultCleanPollInterval
	}

	uiOutput := ui.FromContext(ctx)
	uiOutput.Section("Delete Network Operator custom resources")

	deleted, err := deleteNetworkOperatorCustomResources(
		ctx, kubeClient, opts.Namespace, opts.PollInterval, uiOutput)
	if err != nil {
		return result, err
	}
	result.CustomResourcesDeleted = deleted

	if opts.KeepHelmChart {
		uiOutput.Success("Kept Helm release %s in namespace %s",
			helmclient.DefaultReleaseName, opts.Namespace)
		return result, nil
	}
	if opts.RestConfig == nil {
		return result, fmt.Errorf("uninstall network-operator Helm release: nil REST config")
	}

	uiOutput.Section("Uninstall Network Operator Helm chart")
	removed, err := uninstall(ctx, opts.RestConfig, opts.Namespace, opts.HelmTimeout)
	if err != nil {
		return result, err
	}
	result.HelmReleaseRemoved = removed
	if removed {
		uiOutput.Success("Uninstalled Helm release %s from namespace %s",
			helmclient.DefaultReleaseName, opts.Namespace)
	} else {
		uiOutput.Info("Helm release %s was not installed in namespace %s",
			helmclient.DefaultReleaseName, opts.Namespace)
	}
	return result, nil
}

func deleteNetworkOperatorCustomResources(
	ctx context.Context,
	kubeClient client.Client,
	namespace string,
	pollInterval time.Duration,
	uiOutput ui.Output,
) (int, error) {
	// Resolve the full initial deletion set before mutating anything. A list
	// permission failure therefore cannot silently produce a partial cleanup.
	namespaced, err := listNamespacedCustomResources(ctx, kubeClient, namespace)
	if err != nil {
		return 0, err
	}
	clusterScoped, err := listClusterScopedCleanResources(ctx, kubeClient)
	if err != nil {
		return 0, err
	}

	deleted := 0
	for _, refs := range [][]cleanObjectRef{namespaced, clusterScoped} {
		count, err := deleteAndWait(ctx, kubeClient, refs, pollInterval, uiOutput)
		deleted += count
		if err != nil {
			return deleted, err
		}
	}

	// Controllers can remove or briefly recreate service CRs while the root
	// policies disappear. Sweep both scopes until they are empty after deleting
	// NicClusterPolicy so no operator-generated CR is left behind.
	for {
		remainingNamespaced, err := listNamespacedCustomResources(ctx, kubeClient, namespace)
		if err != nil {
			return deleted, err
		}
		remainingClusterScoped, err := listClusterScopedCleanResources(ctx, kubeClient)
		if err != nil {
			return deleted, err
		}
		if len(remainingNamespaced) == 0 && len(remainingClusterScoped) == 0 {
			return deleted, nil
		}
		for _, refs := range [][]cleanObjectRef{remainingNamespaced, remainingClusterScoped} {
			count, err := deleteAndWait(ctx, kubeClient, refs, pollInterval, uiOutput)
			deleted += count
			if err != nil {
				return deleted, err
			}
		}
	}
}

func listNamespacedCustomResources(
	ctx context.Context,
	kubeClient client.Client,
	namespace string,
) ([]cleanObjectRef, error) {
	crds := &apiextv1.CustomResourceDefinitionList{}
	if err := kubeClient.List(ctx, crds); err != nil {
		return nil, fmt.Errorf("list CustomResourceDefinitions: %w", err)
	}
	sort.Slice(crds.Items, func(i, j int) bool {
		return crds.Items[i].Name < crds.Items[j].Name
	})

	var refs []cleanObjectRef
	for i := range crds.Items {
		crd := &crds.Items[i]
		if crd.Spec.Scope != apiextv1.NamespaceScoped {
			continue
		}
		version := servedStorageVersion(crd)
		if version == "" {
			continue
		}
		gvk := schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: version,
			Kind:    crd.Spec.Names.Kind,
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				log.Log.V(1).Info("clean: custom resource kind is no longer served",
					"gvk", gvk.String(), "error", err.Error())
				continue
			}
			return nil, fmt.Errorf("list %s custom resources in namespace %s: %w",
				gvk.String(), namespace, err)
		}
		for j := range list.Items {
			refs = append(refs, cleanObjectRef{
				GVK:       gvk,
				Namespace: namespace,
				Name:      list.Items[j].GetName(),
			})
		}
	}
	sortCleanRefs(refs)
	return refs, nil
}

func listClusterScopedCleanResources(
	ctx context.Context,
	kubeClient client.Client,
) ([]cleanObjectRef, error) {
	var refs []cleanObjectRef
	for _, gvk := range clusterScopedCleanKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := kubeClient.List(ctx, list); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return nil, fmt.Errorf("list cluster-scoped %s custom resources: %w", gvk.String(), err)
		}
		sort.Slice(list.Items, func(i, j int) bool {
			return list.Items[i].GetName() < list.Items[j].GetName()
		})
		for i := range list.Items {
			refs = append(refs, cleanObjectRef{GVK: gvk, Name: list.Items[i].GetName()})
		}
	}
	return refs, nil
}

func servedStorageVersion(crd *apiextv1.CustomResourceDefinition) string {
	if crd == nil {
		return ""
	}
	for _, version := range crd.Spec.Versions {
		if version.Served && version.Storage {
			return version.Name
		}
	}
	for _, version := range crd.Spec.Versions {
		if version.Served {
			return version.Name
		}
	}
	return ""
}

func deleteAndWait(
	ctx context.Context,
	kubeClient client.Client,
	refs []cleanObjectRef,
	pollInterval time.Duration,
	uiOutput ui.Output,
) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	propagation := metav1.DeletePropagationBackground
	deleted := 0
	for _, ref := range refs {
		obj := objectForCleanRef(ref)
		if err := kubeClient.Delete(ctx, obj, client.PropagationPolicy(propagation)); err != nil {
			if client.IgnoreNotFound(err) == nil || meta.IsNoMatchError(err) {
				continue
			}
			return deleted, fmt.Errorf("delete %s: %w", cleanRefString(ref), err)
		}
		deleted++
		uiOutput.Info("Deleting %s", cleanRefString(ref))
	}

	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		for _, ref := range refs {
			obj := objectForCleanRef(ref)
			err := kubeClient.Get(ctx, types.NamespacedName{
				Namespace: ref.Namespace,
				Name:      ref.Name,
			}, obj)
			switch {
			case err == nil:
				return false, nil
			case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
				continue
			default:
				return false, fmt.Errorf("wait for deletion of %s: %w", cleanRefString(ref), err)
			}
		}
		return true, nil
	})
	if err != nil {
		return deleted, fmt.Errorf("wait for custom resource deletion: %w", err)
	}
	return deleted, nil
}

func objectForCleanRef(ref cleanObjectRef) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ref.GVK)
	obj.SetNamespace(ref.Namespace)
	obj.SetName(ref.Name)
	return obj
}

func sortCleanRefs(refs []cleanObjectRef) {
	sort.Slice(refs, func(i, j int) bool {
		left := refs[i].GVK.String() + "/" + refs[i].Namespace + "/" + refs[i].Name
		right := refs[j].GVK.String() + "/" + refs[j].Namespace + "/" + refs[j].Name
		return left < right
	})
}

func cleanRefString(ref cleanObjectRef) string {
	if ref.Namespace == "" {
		return fmt.Sprintf("%s %s", ref.GVK.Kind, ref.Name)
	}
	return fmt.Sprintf("%s %s/%s", ref.GVK.Kind, ref.Namespace, ref.Name)
}
