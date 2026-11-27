// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ocpCleanSubscriptionGVK  = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "Subscription"}
	ocpCleanCSVGVK           = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "ClusterServiceVersion"}
	ocpCleanOperatorGroupGVK = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1", Kind: "OperatorGroup"}
)

type ocpCleanOperator struct {
	desired      *unstructured.Unstructured
	subscription *unstructured.Unstructured
	csvs         []*unstructured.Unstructured
}

// OLM targets come only from retained install manifests and an observed managed
// Subscription. Package labels alone cannot attribute an orphan CSV to a bundle.
// Namespaces remain: applying their generated metadata does not grant ownership
// of arbitrary resources that other administrators may have put inside them.
type ocpCleanOperators struct {
	operators []ocpCleanOperator
	groups    []*unstructured.Unstructured
	retained  []string
}

func prepareOCPCleanOperators(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, artifacts *bundle.Bundle, owner string) (*ocpCleanOperators, error) {
	plan := &ocpCleanOperators{}
	if cfg == nil || artifacts == nil || c == nil || owner == "" {
		return nil, fmt.Errorf("OpenShift operator cleanup requires config, bundle and client")
	}
	// Older bundles can omit management metadata; recognized install manifests
	// are evidence that generation opted in. Explicit false still retains OLM.
	if cfg.OCPOperatorInstall != nil && !cfg.OCPOperatorInstall.Manage {
		return plan, nil
	}
	for _, doc := range artifacts.Documents() {
		if doc.Role() != bundle.Deployment || !isOLMInstallTemplate(filepath.Base(doc.Source().File)) {
			continue
		}
		desired := doc.ObjectCopy()
		switch desired.GroupVersionKind() {
		case ocpCleanSubscriptionGVK:
			if err := validateOCPOperatorInstallIdentity(cfg, desired); err != nil {
				return nil, err
			}
			setOCPCleanOwner(desired, owner)
			if desired.GetAnnotations()[ocpCleanRetainAnnotation] != "" {
				plan.retained = append(plan.retained, "Retain Subscription "+client.ObjectKeyFromObject(desired).String()+": retained install intent")
				continue
			}
			op, reason, err := prepareOCPCleanOperator(ctx, c, desired)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				plan.retained = append(plan.retained, reason)
			} else {
				plan.operators = append(plan.operators, op)
			}
		case ocpCleanOperatorGroupGVK:
			if err := validateOCPOperatorInstallIdentity(cfg, desired); err != nil {
				return nil, err
			}
			if desired.GetAnnotations()[ocpCleanRetainAnnotation] != "" {
				plan.retained = append(plan.retained, "Retain OperatorGroup "+client.ObjectKeyFromObject(desired).String()+": retained install intent")
				continue
			}
			live := desired.DeepCopy()
			if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("inspect generated OperatorGroup: %w", err)
			}
			if reason := ocpInstallOwnership(desired, live); reason != "" {
				plan.retained = append(plan.retained, fmt.Sprintf("Retain OperatorGroup %s: %s", client.ObjectKeyFromObject(live), reason))
				continue
			}
			if !ocpOperatorGroupMatches(desired, live) {
				plan.retained = append(plan.retained, fmt.Sprintf("Retain OperatorGroup %s: live spec differs from retained intent", client.ObjectKeyFromObject(live)))
				continue
			}
			plan.groups = append(plan.groups, live)
		case schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}:
			plan.retained = append(plan.retained, fmt.Sprintf("Retain Namespace %s: generated metadata does not establish exclusive ownership of its contents", desired.GetName()))
		default:
			return nil, fmt.Errorf("unsupported operator-install resource %s %s", desired.GetKind(), client.ObjectKeyFromObject(desired))
		}
	}
	slices.Reverse(plan.operators)
	slices.Reverse(plan.groups)
	return plan, nil
}

func ocpOperatorGroupMatches(desired, live *unstructured.Unstructured) bool {
	want, _, wantErr := unstructured.NestedMap(desired.Object, "spec")
	have, _, haveErr := unstructured.NestedMap(live.Object, "spec")
	if wantErr != nil || haveErr != nil {
		return false
	}
	// OperatorGroup admission defaults this one optional field. Normalize only
	// that documented default on copies; retain administrator-specific fields.
	for _, spec := range []map[string]interface{}{want, have} {
		if spec["upgradeStrategy"] == "Default" {
			delete(spec, "upgradeStrategy")
		}
	}
	return reflect.DeepEqual(want, have)
}

// Config validates package placement without synthesizing missing install targets.
func validateOCPOperatorInstallIdentity(cfg *config.LaunchKitConfig, desired *unstructured.Unstructured) error {
	pkg, _, err := unstructured.NestedString(desired.Object, "spec", "name")
	if err != nil {
		return err
	}
	for _, expected := range requiredOCPOperators(cfg, true) {
		if desired.GetNamespace() == expected.namespace && (desired.GetKind() == "OperatorGroup" || pkg == expected.packageName) {
			return nil
		}
	}
	return fmt.Errorf("retained operator install %s %s does not match configured package namespaces", desired.GetKind(), client.ObjectKeyFromObject(desired))
}

func prepareOCPCleanOperator(ctx context.Context, c client.Client, desired *unstructured.Unstructured) (ocpCleanOperator, string, error) {
	op := ocpCleanOperator{desired: desired}
	pkg, _, err := unstructured.NestedString(desired.Object, "spec", "name")
	if err != nil || pkg == "" || desired.GetNamespace() == "" || desired.GetName() == "" {
		return op, "", fmt.Errorf("invalid retained Subscription identity or package")
	}
	switch pkg {
	case olmNetworkOperatorPkg, "sriov-network-operator", "nfd", olmMaintenanceOperator:
	default:
		return op, "", fmt.Errorf("unsupported retained operator package %q", pkg)
	}
	sub := desired.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(desired), sub); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return op, "", fmt.Errorf("inspect managed Subscription %s: %w", client.ObjectKeyFromObject(desired), err)
	} else if err == nil {
		if reason := ocpInstallOwnership(desired, sub); reason != "" {
			return op, "Retain Subscription " + client.ObjectKeyFromObject(desired).String() + ": " + reason, nil
		}
		want, _, _ := unstructured.NestedMap(desired.Object, "spec")
		have, _, _ := unstructured.NestedMap(sub.Object, "spec")
		// OLM can add fields such as startingCSV; only deliberately generated
		// install coordinates must still match. Deploy also merges config.env.
		if !ocpSpecContains(have, want) {
			return op, "", fmt.Errorf("managed Subscription %s live install spec differs from retained intent", client.ObjectKeyFromObject(desired))
		}
		op.subscription = sub
	}
	subscriptions, err := listCleanNetworkObjects(ctx, c, ocpCleanSubscriptionGVK, desired.GetNamespace())
	if err != nil {
		return op, "", err
	}
	for i := range subscriptions {
		peer := &subscriptions[i]
		peerPkg, _, _ := unstructured.NestedString(peer.Object, "spec", "name")
		if peerPkg == pkg && client.ObjectKeyFromObject(peer) != client.ObjectKeyFromObject(desired) {
			return op, "Retain Subscription " + client.ObjectKeyFromObject(desired).String() + ": another Subscription installs the same package", nil
		}
	}
	label := ocpOperatorPackageLabel(pkg, desired.GetNamespace())
	csvs, err := listCleanNetworkObjects(ctx, c, ocpCleanCSVGVK, desired.GetNamespace())
	if err != nil {
		return op, "", err
	}
	names := map[string]bool{}
	if op.subscription != nil {
		for _, field := range []string{"installedCSV", "currentCSV"} {
			name, _, err := unstructured.NestedString(sub.Object, "status", field)
			if err != nil {
				return op, "", fmt.Errorf("inspect Subscription %s: %w", field, err)
			}
			if name != "" {
				names[name] = true
			}
		}
	}
	for i := range csvs {
		csv := &csvs[i]
		_, labeled := csv.GetLabels()[label]
		if !names[csv.GetName()] && !labeled {
			continue
		}
		if csv.GetLabels()["olm.copiedFrom"] != "" || csv.GetAnnotations()["olm.copiedFrom"] != "" {
			continue
		}
		if op.subscription == nil && labeled {
			return op, fmt.Sprintf("Retain CSV %s: its Subscription is absent; OLM package identity alone cannot establish that this bundle installed it", client.ObjectKeyFromObject(csv)), nil
		}
		if reason := ocpOperatorOwnership(desired, csv); reason != "" {
			return op, fmt.Sprintf("Retain Subscription %s and CSV %s: %s", client.ObjectKeyFromObject(desired), client.ObjectKeyFromObject(csv), reason), nil
		}
		for key := range csv.GetLabels() {
			if strings.HasPrefix(key, "operators.coreos.com/") && key != label {
				return op, "Retain Subscription " + client.ObjectKeyFromObject(desired).String() + ": its CSV is shared with another OLM package", nil
			}
		}
		for j := range subscriptions {
			peer := &subscriptions[j]
			if client.ObjectKeyFromObject(peer) == client.ObjectKeyFromObject(desired) {
				continue
			}
			for _, field := range []string{"installedCSV", "currentCSV"} {
				name, _, _ := unstructured.NestedString(peer.Object, "status", field)
				if name == csv.GetName() {
					return op, "Retain Subscription " + client.ObjectKeyFromObject(desired).String() + ": another Subscription references its CSV", nil
				}
			}
		}
		op.csvs = append(op.csvs, csv.DeepCopy())
	}
	return op, "", nil
}

func ocpInstallOwnership(desired, live *unstructured.Unstructured) string {
	version := desired.GetAnnotations()[launchKitVersionAnnotation]
	if version == "" || live.GetAnnotations()[launchKitVersionAnnotation] != version {
		return "matching Launch Kit generation provenance is absent"
	}
	return ocpOperatorOwnership(desired, live)
}

// CSVs are created by OLM and do not carry generated manifest provenance, but
// explicit retention and claims still apply to them as well as install objects.
func ocpOperatorOwnership(desired, live *unstructured.Unstructured) string {
	if live.GetUID() == "" || len(live.GetOwnerReferences()) > 0 {
		return "resource has no UID or belongs to another controller"
	}
	if live.GetAnnotations()[ocpCleanRetainAnnotation] != "" {
		return "resource is explicitly retained"
	}
	journal, err := readOCPConfigurationJournal(live)
	if err != nil {
		return "rollback ownership cannot be inspected: " + err.Error()
	}
	if journal != nil && len(journal.Cohorts) > 1 {
		return "resource is shared by multiple deployments"
	}
	if journal != nil && journal.Cohorts[0] != desired.GetAnnotations()[ocpCleanOwnerAnnotation] {
		return "resource has a claim from another deployment"
	}
	if owner := live.GetAnnotations()[ocpCleanOwnerAnnotation]; owner != "" && owner != desired.GetAnnotations()[ocpCleanOwnerAnnotation] {
		return "resource belongs to another deployment"
	}
	return ""
}

func ocpOperatorPackageLabel(pkg, namespace string) string {
	name := pkg + "." + namespace
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], ".-_")
	}
	return "operators.coreos.com/" + name
}

func (p *ocpCleanOperators) removesSubscription(obj *unstructured.Unstructured) bool {
	if p == nil || obj == nil || obj.GroupVersionKind() != ocpCleanSubscriptionGVK {
		return false
	}
	for _, op := range p.operators {
		if op.subscription != nil && client.ObjectKeyFromObject(op.desired) == client.ObjectKeyFromObject(obj) {
			return true
		}
	}
	return false
}

func (p *ocpCleanOperators) targets() []string {
	if p == nil {
		return nil
	}
	lines := slices.Clone(p.retained)
	for _, op := range p.operators {
		if op.subscription == nil {
			continue
		}
		lines = append(lines, "Uninstall managed Subscription "+client.ObjectKeyFromObject(op.desired).String()+" and its OLM CSVs after consumer teardown")
	}
	for _, group := range p.groups {
		lines = append(lines, "Remove OperatorGroup "+client.ObjectKeyFromObject(group).String()+" if no shared operators remain")
	}
	return lines
}

func (p *ocpCleanOperators) execute(ctx context.Context, c client.Client, opts CleanOptions) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	retained := slices.Clone(p.retained)
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultCleanPollInterval
	}
	for _, op := range p.operators {
		if op.subscription == nil {
			continue
		}
		pkg, _, _ := unstructured.NestedString(op.desired.Object, "spec", "name")
		reason, err := ocpRemainingOperatorConsumer(ctx, c, pkg, op.desired.GetNamespace())
		if err != nil {
			return retained, err
		}
		if reason != "" {
			retained = append(retained, fmt.Sprintf("Retain Subscription %s and its CSVs: %s", client.ObjectKeyFromObject(op.desired), reason))
			continue
		}
		// Re-list peers and CSVs near uninstall; new installs and upgrade targets must
		// be reviewed rather than silently added to the confirmed deletion set.
		current, reason, err := prepareOCPCleanOperator(ctx, c, op.desired)
		if err != nil {
			return retained, err
		}
		if reason != "" {
			return retained, fmt.Errorf("managed operator changed after planning: %s", reason)
		}
		if !sameOCPOperatorTargets(op, current) {
			return retained, fmt.Errorf("managed operator %s changed after planning; inspect cleanup again", client.ObjectKeyFromObject(op.desired))
		}
		if op.subscription != nil {
			if err := deleteOCPOperatorObject(ctx, c, op.subscription, opts); err != nil {
				return retained, err
			}
		}
		for _, csv := range op.csvs {
			if err := deleteOCPOperatorObject(ctx, c, csv, opts); err != nil {
				return retained, err
			}
		}
	}
	for _, desired := range p.groups {
		managedNamespace := false
		for _, op := range p.operators {
			managedNamespace = managedNamespace || op.desired.GetNamespace() == desired.GetNamespace()
		}
		if !managedNamespace {
			retained = append(retained, fmt.Sprintf("Retain OperatorGroup %s: no exclusively managed Subscription was selected in its namespace", client.ObjectKeyFromObject(desired)))
			continue
		}
		group := desired.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(group), group); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		} else if err != nil {
			return retained, err
		}
		if group.GetUID() != desired.GetUID() {
			return retained, fmt.Errorf("OperatorGroup %s was replaced after planning", client.ObjectKeyFromObject(group))
		}
		reason := ocpInstallOwnership(desired, group)
		targets, _, _ := unstructured.NestedStringSlice(group.Object, "spec", "targetNamespaces")
		if reason == "" && (!reflect.DeepEqual(desired.Object["spec"], group.Object["spec"]) || len(targets) != 1 || targets[0] != group.GetNamespace()) {
			reason = "OperatorGroup is shared, global or changed from generated intent"
		}
		for _, gvk := range []schema.GroupVersionKind{ocpCleanSubscriptionGVK, ocpCleanCSVGVK} {
			if reason != "" {
				break
			}
			peers, err := listCleanNetworkObjects(ctx, c, gvk, group.GetNamespace())
			if err != nil {
				return retained, err
			}
			if len(peers) > 0 {
				reason = "operators remain in its namespace"
			}
		}
		if reason != "" {
			retained = append(retained, fmt.Sprintf("Retain OperatorGroup %s: %s", client.ObjectKeyFromObject(group), reason))
			continue
		}
		if err := deleteOCPOperatorObject(ctx, c, group, opts); err != nil {
			return retained, err
		}
	}
	return retained, nil
}

func sameOCPOperatorTargets(before, after ocpCleanOperator) bool {
	if (before.subscription == nil) != (after.subscription == nil) {
		return false
	}
	if before.subscription != nil && (before.subscription.GetUID() != after.subscription.GetUID() || !reflect.DeepEqual(before.subscription.Object["spec"], after.subscription.Object["spec"])) {
		return false
	}
	if len(before.csvs) != len(after.csvs) {
		return false
	}
	for _, csv := range before.csvs {
		found := false
		for _, current := range after.csvs {
			if csv.GetName() == current.GetName() && csv.GetUID() == current.GetUID() && reflect.DeepEqual(csv.Object["spec"], current.Object["spec"]) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func deleteOCPOperatorObject(ctx context.Context, c client.Client, target *unstructured.Unstructured, opts CleanOptions) error {
	current := target.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	} else if err != nil {
		return err
	}
	if current.GetUID() != target.GetUID() || !reflect.DeepEqual(current.Object["spec"], target.Object["spec"]) ||
		!reflect.DeepEqual(current.GetOwnerReferences(), target.GetOwnerReferences()) || !maps.Equal(current.GetLabels(), target.GetLabels()) {
		return fmt.Errorf("%s %s changed before operator uninstall", current.GetKind(), client.ObjectKeyFromObject(current))
	}
	for _, key := range []string{launchKitVersionAnnotation, ocpCleanOwnerAnnotation, ocpCleanRetainAnnotation, ocpRollbackAnnotation, "olm.copiedFrom"} {
		if current.GetAnnotations()[key] != target.GetAnnotations()[key] {
			return fmt.Errorf("%s %s ownership changed before operator uninstall", current.GetKind(), client.ObjectKeyFromObject(current))
		}
	}
	uid, version := current.GetUID(), current.GetResourceVersion()
	if uid == "" || version == "" {
		return fmt.Errorf("operator uninstall target has no UID or resourceVersion")
	}
	if err := c.Delete(ctx, current, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("uninstall %s %s: %w", current.GetKind(), client.ObjectKeyFromObject(current), err)
	}
	return pollOCPClean(ctx, opts.PollInterval, "operator dependency deletion", func(ctx context.Context) (string, error) {
		live := target.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(live), live); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", nil
		} else if err != nil {
			return "", err
		}
		if live.GetUID() != uid {
			return "", fmt.Errorf("%s %s was replaced during operator uninstall", live.GetKind(), client.ObjectKeyFromObject(live))
		}
		return live.GetKind() + " " + client.ObjectKeyFromObject(live).String(), nil
	})
}

func ocpRemainingOperatorConsumer(ctx context.Context, c client.Client, pkg, namespace string) (string, error) {
	switch pkg {
	case olmNetworkOperatorPkg:
		if reason, err := ocpRemainingNetworkingConsumer(ctx, c, false); err != nil || reason != "" {
			return reason, err
		}
		for _, kind := range []string{"HostDeviceNetwork", "MacvlanNetwork", "IPoIBNetwork"} {
			objects, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind}, "")
			if err != nil {
				return "", err
			}
			if len(objects) > 0 {
				return fmt.Sprintf("%s %s still requires the Network Operator controller", kind, client.ObjectKeyFromObject(&objects[0])), nil
			}
		}
	case olmMaintenanceOperator:
		return ocpRemainingNetworkingConsumer(ctx, c, true)
	case "nfd":
		objects, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: "nfd.openshift.io", Version: "v1", Kind: "NodeFeatureDiscovery"}, namespace)
		if err != nil {
			return "", err
		}
		if len(objects) > 0 {
			return "NodeFeatureDiscovery configuration remains", nil
		}
		if reason, err := ocpRemainingNetworkingConsumer(ctx, c, false); err != nil || reason != "" {
			return reason, err
		}
		return ocpRemainingSriovConsumer(ctx, c, "")
	case "sriov-network-operator":
		if reason, err := ocpRemainingSriovConsumer(ctx, c, namespace); err != nil || reason != "" {
			return reason, err
		}
		for _, kind := range []string{"SriovNetwork", "SriovIBNetwork", "OVSNetwork", "SriovNetworkPoolConfig"} {
			objects, err := listCleanNetworkObjects(ctx, c, schema.GroupVersionKind{Group: cleanSriovGroup, Version: "v1", Kind: kind}, namespace)
			if err != nil {
				return "", err
			}
			for i := range objects {
				obj := &objects[i]
				return fmt.Sprintf("%s %s remains", kind, client.ObjectKeyFromObject(obj)), nil
			}
		}
	}
	return "", nil
}
