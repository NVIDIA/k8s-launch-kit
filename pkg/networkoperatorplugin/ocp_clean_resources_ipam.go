// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NV-IPAM's daemon watches the Network Operator's pool namespace. A network or
// NAD namespace identifies its users; it does not qualify poolName. Inspect
// surviving definitions before removing a pool, including definitions whose NAD
// has not been reconciled yet and externally managed standalone NADs.
func checkOCPPoolConsumers(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, refs []cleanObjectRef) error {
	namespace := ocpCleanNamespaces(cfg).operator
	pools := map[string]bool{}
	selected := map[string]bool{}
	var networks []cleanObjectRef
	for _, ref := range refs {
		selected[ocpCleanIdentity(ref.GVK, ref.Namespace, ref.Name)] = true
		if ref.GVK.Group == "nv-ipam.nvidia.com" && ref.GVK.Kind == "IPPool" && ref.Namespace == namespace {
			pools[ref.Name] = true
		}
		if isCleanNetworkDefinition(ref, ref.Namespace) {
			networks = append(networks, ref)
		}
	}
	if len(pools) == 0 {
		return nil
	}
	check := func(object *unstructured.Unstructured, configuration ocpPoolCNIConfig) error {
		references, err := ocpNVIPAMPoolReferences(configuration, object.GetName())
		if err != nil {
			return fmt.Errorf("inspect surviving %s %s IPAM before pool deletion: %w", object.GetKind(), client.ObjectKeyFromObject(object), err)
		}
		for _, name := range references {
			if pools[name] {
				return fmt.Errorf("refuse cleanup of IPPool %s/%s and its NV-IPAM controller: surviving %s %s still references that pool; retain the pool or remove that consumer explicitly", namespace, name, object.GetKind(), client.ObjectKeyFromObject(object))
			}
		}
		return nil
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: cleanSriovGroup, Version: "v1", Kind: "SriovNetwork"}, {Group: cleanSriovGroup, Version: "v1", Kind: "SriovIBNetwork"}, {Group: cleanSriovGroup, Version: "v1", Kind: "OVSNetwork"},
		{Group: "mellanox.com", Version: "v1alpha1", Kind: "HostDeviceNetwork"}, {Group: "mellanox.com", Version: "v1alpha1", Kind: "MacvlanNetwork"}, {Group: "mellanox.com", Version: "v1alpha1", Kind: "IPoIBNetwork"},
	} {
		objects, err := listCleanNetworkObjects(ctx, c, gvk, "")
		if err != nil {
			return fmt.Errorf("inspect surviving network pool consumers: %w", err)
		}
		for i := range objects {
			object := &objects[i]
			if selected[ocpCleanIdentity(gvk, object.GetNamespace(), object.GetName())] {
				continue
			}
			configuration := ocpPoolCNIConfig{Name: object.GetName()}
			raw, _, err := unstructured.NestedString(object.Object, "spec", "ipam")
			if err != nil {
				return fmt.Errorf("inspect surviving %s %s spec.ipam: %w", gvk.Kind, client.ObjectKeyFromObject(object), err)
			}
			if strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &configuration.IPAM); err != nil {
					return fmt.Errorf("inspect surviving %s %s spec.ipam JSON: %w", gvk.Kind, client.ObjectKeyFromObject(object), err)
				}
			}
			meta, _, err := unstructured.NestedString(object.Object, "spec", "metaPlugins")
			if err != nil {
				return fmt.Errorf("inspect surviving %s %s metaPlugins: %w", gvk.Kind, client.ObjectKeyFromObject(object), err)
			}
			if strings.TrimSpace(meta) != "" {
				// The operators splice comma-separated JSON plugin objects into the
				// generated conflist; the CR field itself is not a JSON array.
				if err := json.Unmarshal([]byte("["+meta+"]"), &configuration.Plugins); err != nil {
					return fmt.Errorf("inspect surviving %s %s metaPlugins JSON: %w", gvk.Kind, client.ObjectKeyFromObject(object), err)
				}
			}

			if err := check(object, configuration); err != nil {
				return err
			}
		}
	}
	proofs, err := ocpPoolNADRemovalProofs(ctx, c, networks)
	if err != nil {
		return err
	}
	nads, err := listCleanNetworkObjects(ctx, c, cleanNADGVK, "")
	if err != nil {
		return fmt.Errorf("inspect surviving NAD pool consumers: %w", err)
	}
	for i := range nads {
		nad := &nads[i]
		if ocpPoolNADWillBeRemoved(nad, proofs) {
			continue
		}
		raw, found, err := unstructured.NestedString(nad.Object, "spec", "config")
		if err != nil {
			return fmt.Errorf("inspect surviving NAD %s config: %w", client.ObjectKeyFromObject(nad), err)
		}
		// Empty-config NADs use node-local CNI files, outside the inline definition
		// contract inspected here; do not invent references from their identity.
		if !found || strings.TrimSpace(raw) == "" {
			continue
		}
		var configuration ocpPoolCNIConfig
		if err := json.Unmarshal([]byte(raw), &configuration); err != nil {
			return fmt.Errorf("inspect surviving NAD %s config JSON: %w", client.ObjectKeyFromObject(nad), err)
		}
		if err := check(nad, configuration); err != nil {
			return err
		}
	}
	return nil
}

type ocpPoolCNIConfig struct {
	Name    string             `json:"name"`
	IPAM    json.RawMessage    `json:"ipam"`
	Args    json.RawMessage    `json:"args"`
	Plugins []ocpPoolCNIConfig `json:"plugins"`
}

// Mirror the actual CNI naming contract: args.cni.poolNames overrides the
// comma-separated ipam.poolName; omitted names default to the CNI network name.
// poolType defaults to ippool. There is no ipam.namespace/poolNamespace field.
func ocpNVIPAMPoolReferences(configuration ocpPoolCNIConfig, fallbackName string) ([]string, error) {
	name := configuration.Name
	if name == "" {
		name = fallbackName
	}
	var result []string
	var header struct {
		Type string `json:"type"`
	}
	if len(configuration.IPAM) > 0 {
		if err := json.Unmarshal(configuration.IPAM, &header); err != nil {
			return nil, fmt.Errorf("decode IPAM type: %w", err)
		}
	}
	if header.Type == "nv-ipam" {
		var ipam struct {
			PoolName      string `json:"poolName"`
			PoolType      string `json:"poolType"`
			ForcePoolName bool   `json:"forcePoolName"`
		}
		if err := json.Unmarshal(configuration.IPAM, &ipam); err != nil {
			return nil, fmt.Errorf("decode NV-IPAM pool configuration: %w", err)
		}
		poolType := ipam.PoolType
		var args struct {
			CNI *struct {
				PoolNames []string `json:"poolNames"`
				PoolType  string   `json:"poolType"`
			} `json:"cni"`
		}
		if len(configuration.Args) > 0 {
			if err := json.Unmarshal(configuration.Args, &args); err != nil {
				return nil, fmt.Errorf("decode NV-IPAM CNI args: %w", err)
			}
		}
		var names []string
		if args.CNI != nil {
			names = args.CNI.PoolNames
			if args.CNI.PoolType != "" {
				poolType = args.CNI.PoolType
			}
		}
		if len(names) == 0 {
			value := ipam.PoolName
			if value == "" {
				if ipam.ForcePoolName {
					return nil, fmt.Errorf("NV-IPAM requires an explicit poolName when forcePoolName is true")
				}
				value = name
			}
			names = strings.Split(value, ",")
		}
		if len(names) > 2 {
			return nil, fmt.Errorf("NV-IPAM supports at most two pool names")
		}
		for _, name := range names {
			if name == "" {
				return nil, fmt.Errorf("NV-IPAM has an empty pool name")
			}
		}
		switch strings.ToLower(poolType) {
		case "", "ippool":
			result = append(result, names...)
		case "cidrpool": // An independent CIDRPool is not a selected IPPool.
		default:
			return nil, fmt.Errorf("unsupported NV-IPAM poolType %q", poolType)
		}
	}

	for _, plugin := range configuration.Plugins {
		// libcni supplies the conflist's name to each delegate, overriding a name
		// written inside the delegate itself.
		plugin.Name = name
		references, err := ocpNVIPAMPoolReferences(plugin, name)
		if err != nil {
			return nil, err
		}
		result = append(result, references...)
	}
	return result, nil
}

type ocpPoolNADRemovalProof struct {
	ref       cleanObjectRef
	namespace string
}

func ocpPoolNADRemovalProofs(ctx context.Context, c client.Client, refs []cleanObjectRef) ([]ocpPoolNADRemovalProof, error) {
	var proofs []ocpPoolNADRemovalProof
	for _, ref := range refs {
		// Annotation-only ancestry cannot bind a vanished parent to an incarnation.
		if ref.UID == "" {
			continue
		}
		live, err := inspectOCPDeletionTarget(ctx, c, ref)
		if err != nil {
			return nil, err
		}
		if live == nil {
			continue
		}
		namespace, _, err := unstructured.NestedString(live.Object, "spec", "networkNamespace")
		if err != nil {
			return nil, err
		}
		if namespace == "" {
			if ref.GVK.Group == cleanSriovGroup {
				namespace = ref.Namespace
			} else {
				namespace = "default"
			}
		}
		proofs = append(proofs, ocpPoolNADRemovalProof{ref: ref, namespace: namespace})
	}
	return proofs, nil
}

func ocpPoolNADWillBeRemoved(nad *unstructured.Unstructured, proofs []ocpPoolNADRemovalProof) bool {
	owners := nad.GetOwnerReferences()
	for _, proof := range proofs {
		ref := proof.ref
		// Exactly one matching GC owner is sufficient even if the child was renamed.
		// With additional owners, deleting this parent cannot guarantee child removal.
		if len(owners) == 1 {
			owner := owners[0]
			gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
			if gvk.Group == ref.GVK.Group && gvk.Kind == ref.GVK.Kind && owner.Name == ref.Name && owner.UID == ref.UID && (ref.Namespace == "" || ref.Namespace == nad.GetNamespace()) {
				return true
			}
		}
		// Cross-namespace SR-IOV finalizers act on the rendered NAD's exact identity.
		// A copied annotation on another name/namespace supplies no deletion proof.
		annotation := ref.GVK.Kind + "." + ref.GVK.Group + "/" + ref.Namespace + "/" + ref.Name
		if len(owners) == 0 && ref.GVK.Group == cleanSriovGroup && nad.GetName() == ref.Name && nad.GetNamespace() == proof.namespace && nad.GetAnnotations()["sriovnetwork.openshift.io/owner-ref"] == annotation {
			return true
		}
	}
	return false
}
