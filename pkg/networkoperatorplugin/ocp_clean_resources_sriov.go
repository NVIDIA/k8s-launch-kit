// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ocpSriovNodeClean struct {
	ref                 cleanObjectRef
	pfs                 map[string]bool
	requiresProof       bool
	provedGeneration    int64
	attemptedGeneration int64
	nextLogRead         time.Time
}
type ocpSriovClean struct {
	nodes    map[string]*ocpSriovNodeClean
	readLogs sriovDaemonLogReader
}

func prepareOCPSriovClean(ctx context.Context, c client.Client, refs []cleanObjectRef, readLogs sriovDaemonLogReader) (*ocpSriovClean, error) {
	reset := &ocpSriovClean{nodes: map[string]*ocpSriovNodeClean{}, readLogs: readLogs}
	var policies []cleanObjectRef
	for _, ref := range refs {
		if ref.GVK.Group == cleanSriovGroup && ref.GVK.Kind == "SriovNetworkNodePolicy" {
			policies = append(policies, ref)
		}
	}
	if len(policies) == 0 {
		return reset, nil
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("inspect selected SR-IOV nodes: %w", err)
	}
	for _, ref := range policies {
		policy, err := ocpDesired(ref)
		if err != nil {
			return nil, err
		}
		selector, _, err := unstructured.NestedStringMap(policy.Object, "spec", "nodeSelector")
		if err != nil {
			return nil, err
		}
		matchedNodes := 0
		for i := range nodes.Items {
			node := &nodes.Items[i]
			matches := true
			for key, value := range selector {
				if actual, present := node.Labels[key]; !present || actual != value {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			matchedNodes++
			stateRef := cleanObjectRef{GVK: schema.GroupVersionKind{Group: cleanSriovGroup, Version: ref.GVK.Version, Kind: "SriovNetworkNodeState"}, Namespace: ref.Namespace, Name: node.Name}
			state := objectForCleanRef(stateRef)
			if err := c.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
				return nil, fmt.Errorf("capture selected SriovNetworkNodeState %s/%s: %w", ref.Namespace, node.Name, err)
			}
			key := ref.Namespace + "/" + node.Name
			selected := reset.nodes[key]
			if selected == nil {
				stateRef.UID = state.GetUID()
				selected = &ocpSriovNodeClean{ref: stateRef, pfs: map[string]bool{}}
				reset.nodes[key] = selected
			}
			interfaces, _, err := unstructured.NestedSlice(state.Object, "status", "interfaces")
			if err != nil {
				return nil, err
			}
			spec, _, err := unstructured.NestedMap(state.Object, "spec")
			if err != nil {
				return nil, err
			}
			selected.requiresProof = selected.requiresProof || ref.UID != "" || state.GetGeneration() != 1 || !cleanSriovEmpty(spec)
			if selected.requiresProof && readLogs == nil {
				return nil, fmt.Errorf("selected SriovNetworkNodeState %s requires current-generation daemon log evidence; REST config/pods-log access is unavailable", key)
			}
			if selected.requiresProof && (state.GetUID() == "" || state.GetCreationTimestamp().Time.IsZero() || state.GetGeneration() <= 0) {
				return nil, fmt.Errorf("selected SriovNetworkNodeState %s has no stable UID/lifetime/generation for reset proof", key)
			}
			matchedPFs := 0
			for _, value := range interfaces {
				iface, ok := value.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("malformed SR-IOV status interface")
				}
				match, err := ocpSriovMatches(policy, iface)
				if err != nil {
					return nil, err
				}
				if !match {
					continue
				}
				address, _, err := unstructured.NestedString(iface, "pciAddress")
				if err != nil {
					return nil, err
				}
				if address == "" {
					return nil, fmt.Errorf("selected SR-IOV status interface has no PCI address")
				}
				external, _, err := unstructured.NestedBool(iface, "externallyManaged")
				if err != nil {
					return nil, err
				}
				if external {
					return nil, fmt.Errorf("selected PF %s on %s is externally managed; reset cannot be verified", address, node.Name)
				}
				selected.pfs[address] = true
				matchedPFs++
			}
			if matchedPFs == 0 {
				return nil, fmt.Errorf("no selected PF is observable on %s for %s; reset scope cannot be verified", node.Name, cleanRefString(ref))
			}
		}
		if matchedNodes == 0 {
			return nil, fmt.Errorf("no selected nodes are observable for %s; reset scope cannot be verified", cleanRefString(ref))
		}
	}
	if err := reset.checkPeerPolicies(ctx, c, refs, nodes); err != nil {
		return nil, err
	}
	return reset, nil
}

// Refuse a known competing claim before deleting any selected intent. A policy
// on an unrelated PF remains valid and does not prevent selected-PF teardown.
func (reset *ocpSriovClean) checkPeerPolicies(ctx context.Context, c client.Client, refs []cleanObjectRef, nodes *corev1.NodeList) error {
	selected := map[string]bool{}
	namespaces := map[string]string{}
	for _, ref := range refs {
		if ref.GVK.Group == cleanSriovGroup && ref.GVK.Kind == "SriovNetworkNodePolicy" {
			selected[ref.Namespace+"/"+ref.Name] = true
			namespaces[ref.Namespace] = ref.GVK.Version
		}
	}
	nodeLabels := map[string]map[string]string{}
	for i := range nodes.Items {
		nodeLabels[nodes.Items[i].Name] = nodes.Items[i].Labels
	}
	for namespace, version := range namespaces {
		policies, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: cleanSriovGroup, Version: version, Kind: "SriovNetworkNodePolicy"}, namespace)
		if err != nil {
			return err
		}
		for i := range policies {
			policy := &policies[i]
			if selected[namespace+"/"+policy.GetName()] {
				continue
			}
			selector, _, err := unstructured.NestedStringMap(policy.Object, "spec", "nodeSelector")
			if err != nil {
				return err
			}
			nicSelector, _, err := unstructured.NestedMap(policy.Object, "spec", "nicSelector")
			if err != nil {
				return err
			}
			if cleanSriovEmpty(nicSelector) {
				continue
			}
			for _, node := range reset.nodes {
				if node.ref.Namespace != namespace {
					continue
				}
				matches := true
				for key, value := range selector {
					if actual, present := nodeLabels[node.ref.Name][key]; !present || actual != value {
						matches = false
						break
					}
				}
				if !matches {
					continue
				}
				state := objectForCleanRef(node.ref)
				if err := c.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
					return err
				}
				interfaces, _, err := unstructured.NestedSlice(state.Object, "status", "interfaces")
				if err != nil {
					return err
				}
				for _, value := range interfaces {
					iface, ok := value.(map[string]interface{})
					if !ok {
						return fmt.Errorf("malformed SR-IOV status interface")
					}
					address, _, err := unstructured.NestedString(iface, "pciAddress")
					if err != nil {
						return err
					}
					if !node.pfs[address] {
						continue
					}
					overlap, err := ocpSriovMatches(policy, iface)
					if err != nil {
						return fmt.Errorf("cannot rule out competing policy %s/%s on selected PF %s: %w", namespace, policy.GetName(), address, err)
					}
					if overlap {
						return fmt.Errorf("refuse selected SR-IOV reset: unselected policy %s/%s still selects PF %s on %s; remove that claim or retain the selected policy", namespace, policy.GetName(), address, node.ref.Name)
					}
				}
			}
		}
	}
	return nil
}

// Match the selectors emitted by OCP profiles. Refuse unsupported selectors
// rather than silently broadening the hardware verification boundary.
func ocpSriovMatches(policy *unstructured.Unstructured, iface map[string]interface{}) (bool, error) {
	selector, found, err := unstructured.NestedMap(policy.Object, "spec", "nicSelector")
	if err != nil {
		return false, err
	}
	if !found || len(selector) == 0 {
		return false, fmt.Errorf("selected SR-IOV policy has an empty NIC selector")
	}
	for key := range selector {
		if !slices.Contains([]string{"vendor", "deviceID", "rootDevices", "pfNames", "netFilter"}, key) {
			return false, fmt.Errorf("unsupported SR-IOV cleanup NIC selector %s", key)
		}
	}
	netFilter, _, err := unstructured.NestedString(selector, "netFilter")
	if err != nil {
		return false, err
	}
	if netFilter != "" {
		return false, fmt.Errorf("SR-IOV cleanup cannot verify netFilter selector %q", netFilter)
	}
	for _, key := range []string{"vendor", "deviceID"} {
		expected, _, err := unstructured.NestedString(selector, key)
		if err != nil {
			return false, err
		}
		actual, _, err := unstructured.NestedString(iface, key)
		if err != nil {
			return false, err
		}
		if expected != "" && expected != actual {
			return false, nil
		}
	}
	addresses, _, err := unstructured.NestedStringSlice(selector, "rootDevices")
	if err != nil {
		return false, err
	}
	address, _, err := unstructured.NestedString(iface, "pciAddress")
	if err != nil {
		return false, err
	}
	if len(addresses) > 0 && !slices.Contains(addresses, address) {
		return false, nil
	}
	names, _, err := unstructured.NestedStringSlice(selector, "pfNames")
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		return true, nil
	}
	name, _, err := unstructured.NestedString(iface, "name")
	if err != nil {
		return false, err
	}
	alts, _, err := unstructured.NestedStringSlice(iface, "altNames")
	if err != nil {
		return false, err
	}
	for _, selected := range names {
		base, _, _ := strings.Cut(selected, "#")
		if base == name || slices.Contains(alts, base) {
			return true, nil
		}
	}
	return false, nil
}

func (reset *ocpSriovClean) wait(ctx context.Context, c client.Client, interval time.Duration) error {
	if len(reset.nodes) == 0 {
		return nil
	}
	return pollOCPClean(ctx, interval, "selected SR-IOV PF reset", func(ctx context.Context) (string, error) {
		var reasons []string
		for key, node := range reset.nodes {
			state := objectForCleanRef(node.ref)
			if err := c.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
				return "", fmt.Errorf("observe selected SriovNetworkNodeState %s: %w", key, err)
			}
			if state.GetUID() != node.ref.UID {
				return "", fmt.Errorf("selected SriovNetworkNodeState %s was replaced; reset remains unverified", key)
			}
			pending, err := ocpSriovResetPending(state, node.pfs)
			if err != nil {
				return "", fmt.Errorf("SriovNetworkNodeState %s: %w", key, err)
			}
			if pending != "" {
				reasons = append(reasons, key+": "+pending)
				continue
			}
			spec, _, err := unstructured.NestedMap(state.Object, "spec")
			if err != nil {
				return "", err
			}
			node.requiresProof = node.requiresProof || state.GetGeneration() != 1 || !cleanSriovEmpty(spec)
			if !node.requiresProof || node.provedGeneration == state.GetGeneration() {
				continue
			}
			now := time.Now()
			if node.attemptedGeneration != state.GetGeneration() || !now.Before(node.nextLogRead) {
				node.attemptedGeneration = state.GetGeneration()
				node.nextLogRead = now.Add(5 * time.Second)
				latest, err := verifySriovDaemonGenerationWithProof(ctx, c, state, reset.readLogs, ocpSriovLogProvesGeneration)
				if err != nil {
					return "", fmt.Errorf("selected SriovNetworkNodeState %s generation %d reset proof unavailable: %w", key, state.GetGeneration(), err)
				}
				if latest != nil {
					pending, err := ocpSriovResetPending(latest, node.pfs)
					if err != nil {
						return "", err
					}
					if pending != "" {
						reasons = append(reasons, key+": "+pending)
						continue
					}
					node.provedGeneration = latest.GetGeneration()
					continue
				}
			}
			reasons = append(reasons, fmt.Sprintf("%s: waiting for current generation %d successful RH daemon apply evidence (logs may be pending, rotated or truncated)", key, state.GetGeneration()))
		}
		return joinOCPCleanReasons(reasons), nil
	})
}

func ocpSriovResetPending(state *unstructured.Unstructured, pfs map[string]bool) (string, error) {
	desired, _, err := unstructured.NestedSlice(state.Object, "spec", "interfaces")
	if err != nil {
		return "", err
	}
	for _, value := range desired {
		iface, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("malformed desired interface")
		}
		address, _, err := unstructured.NestedString(iface, "pciAddress")
		if err != nil {
			return "", err
		}
		if pfs[address] {
			return "selected PF " + address + " retains desired configuration (another or concurrent policy may still select it)", nil
		}
	}
	status, _, err := unstructured.NestedString(state.Object, "status", "syncStatus")
	if err != nil {
		return "", err
	}
	failure, _, err := unstructured.NestedString(state.Object, "status", "lastSyncError")
	if err != nil {
		return "", err
	}
	if status == "Failed" {
		return "", fmt.Errorf("reset failed: %s", failure)
	}
	if status != "Succeeded" || failure != "" {
		return fmt.Sprintf("syncStatus=%q lastSyncError=%q", status, failure), nil
	}
	for _, key := range []string{"sriovnetwork.openshift.io/current-state", "sriovnetwork.openshift.io/desired-state"} {
		if value, present := state.GetAnnotations()[key]; present && value != "Idle" {
			return key + "=" + value, nil
		}
	}
	interfaces, _, err := unstructured.NestedSlice(state.Object, "status", "interfaces")
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, value := range interfaces {
		iface, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("malformed observed interface")
		}
		address, _, err := unstructured.NestedString(iface, "pciAddress")
		if err != nil {
			return "", err
		}
		if !pfs[address] {
			continue
		}
		seen[address] = true
		external, _, err := unstructured.NestedBool(iface, "externallyManaged")
		if err != nil {
			return "", err
		}
		if external {
			return "", fmt.Errorf("selected PF %s became externally managed; reset remains unverified", address)
		}
		count, _, err := unstructured.NestedInt64(iface, "numVfs")
		if err != nil {
			return "", err
		}
		vfs, _, err := unstructured.NestedSlice(iface, "Vfs")
		if err != nil {
			return "", err
		}
		if count != 0 || len(vfs) > 0 {
			return fmt.Sprintf("selected PF %s still reports VFs (numVfs=%d, Vfs=%d)", address, count, len(vfs)), nil
		}
	}
	for address := range pfs {
		if !seen[address] {
			return "selected PF " + address + " is missing from observed hardware", nil
		}
	}
	return "", nil
}
