// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const ocpRollbackAnnotation = "nvidia.kubernetes-launch-kit/ocp-configuration-rollback"
const ocpLegacyCohort = "legacy-unscoped-deployment"

// Journals and field changes are persisted in the same optimistic transaction.
// Cohorts share the first known baseline, each holding a durable claim.
type ocpConfigurationJournal struct {
	RestoreEnv       map[string]ocpConfigurationValue `json:"restoreEnv,omitempty"`
	EnvWasPresent    *bool                            `json:"envWasPresent,omitempty"`
	ConfigWasPresent bool                             `json:"configWasPresent,omitempty"`
	SpecWasPresent   bool                             `json:"specWasPresent,omitempty"`
	Version          int                              `json:"version"`
	UID              string                           `json:"uid,omitempty"`
	Cohorts          []string                         `json:"cohorts"`
	Fields           []ocpConfigurationField          `json:"fields,omitempty"`
	Created          map[string]interface{}           `json:"created,omitempty"`
	NFDOriginal      *ocpConfigurationValue           `json:"nfdOriginal,omitempty"`
	NFDApplied       string                           `json:"nfdApplied,omitempty"`
}
type ocpConfigurationValue struct {
	Present bool        `json:"present"`
	Value   interface{} `json:"value,omitempty"`
}
type ocpConfigurationField struct {
	Path           []string              `json:"path"`
	Before         ocpConfigurationValue `json:"before"`
	After          ocpConfigurationValue `json:"after"`
	MissingParents [][]string            `json:"missingParents,omitempty"`
	Added          []interface{}         `json:"added,omitempty"`
}

func readOCPConfigurationJournal(obj *unstructured.Unstructured) (*ocpConfigurationJournal, error) {
	raw := obj.GetAnnotations()[ocpRollbackAnnotation]
	if raw == "" {
		return nil, nil
	}
	journal := &ocpConfigurationJournal{}
	if len(raw) > 128*1024 {
		return nil, fmt.Errorf("OpenShift rollback provenance exceeds 128 KiB")
	}
	if err := kjson.Unmarshal([]byte(raw), journal); err != nil {
		return nil, fmt.Errorf("decode OpenShift rollback provenance: %w", err)
	}
	if journal.UID != "" && journal.UID != string(obj.GetUID()) {
		return nil, fmt.Errorf("OpenShift rollback provenance belongs to a different object UID; inspect copied metadata with the configuration owner")
	}
	if journal.Version != 1 || len(journal.Cohorts) == 0 {
		return nil, fmt.Errorf("invalid OpenShift rollback provenance version or cohort claims")
	}
	for i := range journal.Fields {
		field := &journal.Fields[i]
		for _, parent := range field.MissingParents {
			if len(parent) == 0 || len(parent) >= len(field.Path) || !reflect.DeepEqual(parent, field.Path[:len(parent)]) {
				return nil, fmt.Errorf("invalid rollback parent path")
			}
		}
		if !validOCPRollbackPath(obj, field.Path) {
			return nil, fmt.Errorf("rollback provenance has unsupported field %q on %s", strings.Join(field.Path, "."), obj.GetKind())
		}
	}
	for name := range journal.RestoreEnv {
		if obj.GetKind() != "Subscription" || !validOCPRollbackPath(obj, []string{"env", name}) {
			return nil, fmt.Errorf("invalid Subscription restoration field")
		}
	}
	if journal.NFDOriginal != nil && obj.GetKind() != "NodeFeatureDiscovery" {
		return nil, fmt.Errorf("NFD rollback provenance on a different kind")
	}
	if journal.Created != nil {
		if obj.GetKind() == "Subscription" || isOCPSysctlAllowlistPatch(obj) {
			return nil, fmt.Errorf("rollback provenance cannot own platform %s", obj.GetKind())
		}
	}
	return journal, nil
}
func writeOCPConfigurationJournal(obj *unstructured.Unstructured, journal *ocpConfigurationJournal) error {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if journal == nil {
		delete(annotations, ocpRollbackAnnotation)
	} else {
		encoded, err := json.Marshal(journal)
		if err != nil {
			return err
		}
		if len(encoded) > 128*1024 {
			return fmt.Errorf("OpenShift rollback provenance exceeds 128 KiB; reduce the operator configuration")
		}
		annotations[ocpRollbackAnnotation] = string(encoded)
	}
	obj.SetAnnotations(annotations)
	return nil
}
func validOCPRollbackPath(obj *unstructured.Unstructured, path []string) bool {
	p := strings.Join(path, "/")
	switch obj.GetKind() {
	case "SriovOperatorConfig", "MaintenanceOperatorConfig":
		return len(path) >= 2 && path[0] == "spec"
	case "NodeFeatureDiscovery":
		return len(path) >= 4 && path[0] == "nfd" && path[1] == "sources" && path[2] == "pci"
	case "Subscription":
		return p == "env/MAINTENANCE_OPERATOR_ENABLED" || p == "env/MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE"
	case "ConfigMap":
		return isOCPSysctlAllowlistPatch(obj) && p == "data/allowlist.conf"
	}
	return false
}
func additiveOCPRollbackPath(path []string) bool {
	p := strings.Join(path, "/")
	return p == "spec/disablePlugins" || p == "nfd/sources/pci/deviceClassWhitelist"
}
func ocpConfigurationView(obj *unstructured.Unstructured) (map[string]interface{}, error) {
	if obj.GetKind() == "NodeFeatureDiscovery" {
		raw, _, err := unstructured.NestedString(obj.Object, "spec", "workerConfig", "configData")
		if err != nil {
			return nil, err
		}
		fields := map[string]interface{}{}
		if raw != "" {
			if err := yaml.Unmarshal([]byte(raw), &fields); err != nil {
				return nil, fmt.Errorf("decode NFD worker config: %w", err)
			}
		}
		return map[string]interface{}{"nfd": fields}, nil
	}
	if obj.GetKind() == "Subscription" {
		env, _, err := unstructured.NestedSlice(obj.Object, "spec", "config", "env")
		if err != nil {
			return nil, err
		}
		fields := map[string]interface{}{}
		for _, entry := range env {
			item, ok := entry.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("invalid Subscription environment entry")
			}
			name, _ := item["name"].(string)
			if name != "MAINTENANCE_OPERATOR_ENABLED" && name != "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE" {
				continue
			}
			if _, exists := fields[name]; exists {
				return nil, fmt.Errorf("duplicate Subscription environment entry %s", name)
			}
			fields[name] = item
		}
		return map[string]interface{}{"env": fields}, nil
	}
	return obj.Object, nil
}
func ocpConfigurationValueAt(view map[string]interface{}, path []string) (ocpConfigurationValue, error) {
	value, present, err := unstructured.NestedFieldCopy(view, path...)
	return ocpConfigurationValue{Present: present, Value: value}, err
}
func ocpConfigurationDiff(obj *unstructured.Unstructured, before, after map[string]interface{}) ([]ocpConfigurationField, error) {
	fields := []ocpConfigurationField{}
	var walk func(map[string]interface{}, map[string]interface{}, []string) error
	walk = func(a, b map[string]interface{}, prefix []string) error {
		keys := map[string]bool{}
		for key := range a {
			keys[key] = true
		}
		for key := range b {
			keys[key] = true
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		for _, key := range ordered {
			path := append(append([]string{}, prefix...), key)
			av, ap := a[key]
			bv, bp := b[key]
			if reflect.DeepEqual(av, bv) && ap == bp {
				continue
			}
			am, aok := av.(map[string]interface{})
			bm, bok := bv.(map[string]interface{})
			if (aok || bok) && (len(path) != 2 || path[0] != "env") {
				if ap && !aok || bp && !bok {
					return fmt.Errorf("configuration field %s changes shape", strings.Join(path, "."))
				}
				if err := walk(am, bm, path); err != nil {
					return err
				}
				continue
			}
			if !validOCPRollbackPath(obj, path) {
				return fmt.Errorf("unsupported reversible OpenShift configuration field %s", strings.Join(path, "."))
			}
			field := ocpConfigurationField{Path: path, Before: ocpConfigurationValue{Present: ap, Value: av}, After: ocpConfigurationValue{Present: bp, Value: bv}}
			for i := 1; i < len(path); i++ {
				_, present, err := unstructured.NestedFieldNoCopy(before, path[:i]...)
				if err != nil {
					return err
				}
				if !present {
					field.MissingParents = append(field.MissingParents, append([]string{}, path[:i]...))
				}
			}
			fields = append(fields, field)
		}
		return nil
	}
	if err := walk(before, after, nil); err != nil {
		return nil, err
	}
	return fields, nil
}
func ocpCreatedConfigurationIntent(obj *unstructured.Unstructured) map[string]interface{} {
	annotations := obj.GetAnnotations()
	delete(annotations, ocpRollbackAnnotation)
	annotationValues := map[string]interface{}{}
	for k, v := range annotations {
		annotationValues[k] = v
	}
	labels := map[string]interface{}{}
	for k, v := range obj.GetLabels() {
		labels[k] = v
	}
	spec, _, _ := unstructured.NestedFieldCopy(obj.Object, "spec")
	owners, _, _ := unstructured.NestedFieldCopy(obj.Object, "metadata", "ownerReferences")
	return map[string]interface{}{"spec": spec, "labels": labels, "annotations": annotationValues, "ownerReferences": owners}
}
func recordOCPConfigurationChange(before, after *unstructured.Unstructured, cohort string, created bool) error {
	if cohort == "" {
		return fmt.Errorf("OpenShift configuration cohort is required")
	}
	journal, err := readOCPConfigurationJournal(before)
	if err != nil {
		return err
	}
	if journal != nil && len(journal.RestoreEnv) > 0 {
		return fmt.Errorf("OpenShift configuration restoration is pending; finish clean before deploying again")
	}
	if after.GetDeletionTimestamp() != nil {
		return fmt.Errorf("OpenShift configuration %s is terminating", after.GetName())
	}
	if journal == nil {
		journal = &ocpConfigurationJournal{Version: 1, Cohorts: []string{}, UID: string(before.GetUID())}
	}
	if before.GetKind() == "Subscription" && journal.EnvWasPresent == nil {
		_, present, err := unstructured.NestedFieldNoCopy(before.Object, "spec", "config", "env")
		if err != nil {
			return err
		}
		journal.EnvWasPresent = &present
		_, journal.ConfigWasPresent, err = unstructured.NestedFieldNoCopy(before.Object, "spec", "config")
		if err != nil {
			return err
		}
		_, journal.SpecWasPresent, err = unstructured.NestedFieldNoCopy(before.Object, "spec")
		if err != nil {
			return err
		}
	}
	if before.GetKind() == "NodeFeatureDiscovery" && !created && journal.Created == nil {
		original, err := ocpConfigurationValueAt(before.Object, []string{"spec", "workerConfig", "configData"})
		if err != nil {
			return err
		}
		if journal.NFDOriginal == nil {
			journal.NFDOriginal = &original
		}
		applied, _, err := unstructured.NestedString(after.Object, "spec", "workerConfig", "configData")
		if err != nil {
			return err
		}
		journal.NFDApplied = applied
	}
	if created {
		journal.UID = "" // Bound to the server-assigned UID immediately after Create.
		journal.Created = ocpCreatedConfigurationIntent(after)
	} else if journal.Created != nil {
		// Do not adopt subsequent external changes as permission to delete.
		if reflect.DeepEqual(journal.Created, ocpCreatedConfigurationIntent(before)) {
			journal.Created = ocpCreatedConfigurationIntent(after)
		}
	} else {
		a, err := ocpConfigurationView(before)
		if err != nil {
			return err
		}
		b, err := ocpConfigurationView(after)
		if err != nil {
			return err
		}
		changes, err := ocpConfigurationDiff(after, a, b)
		if err != nil {
			return err
		}
		for _, change := range changes {
			change.Added = ocpConfigurationAdditions(change)
			found := false
			for i, field := range journal.Fields {
				if !reflect.DeepEqual(field.Path, change.Path) {
					continue
				}
				found = true
				if !additiveOCPRollbackPath(change.Path) && strings.Join(change.Path, "/") != "data/allowlist.conf" && !reflect.DeepEqual(field.After, change.Before) {
					journal.Fields[i].Before = change.Before
				}
				journal.Fields[i].After = change.After
				for _, addition := range change.Added {
					if !ocpListContains(journal.Fields[i].Added, addition) {
						journal.Fields[i].Added = append(journal.Fields[i].Added, addition)
					}
				}
			}
			if !found {
				journal.Fields = append(journal.Fields, change)
			}
		}
	}
	if !containsOCPString(journal.Cohorts, cohort) {
		journal.Cohorts = append(journal.Cohorts, cohort)
		sort.Strings(journal.Cohorts)
	}
	return writeOCPConfigurationJournal(after, journal)
}
func containsOCPString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Preflight is read-only and checks every journal before consuming CR teardown.
func preflightOCPOperatorConfigurationRestore(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, docs []*unstructured.Unstructured, cohort string) ([]*unstructured.Unstructured, error) {
	targets, err := ocpConfigurationRestoreTargets(ctx, c, cfg, docs, cohort)
	if err != nil {
		return nil, err
	}
	var live []*unstructured.Unstructured
	for _, target := range targets {
		current := target.Object.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(target.Object), current); apierrors.IsNotFound(err) || target.Optional && ocpOptionalConfigurationAbsent(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if target.Optional && !ocpConfigurationHasCohort(current, cohort) {
			continue
		}
		journal, err := readOCPConfigurationJournal(current)
		if err != nil {
			return nil, fmt.Errorf("inspect %s %s: %w", current.GetKind(), client.ObjectKeyFromObject(current), err)
		}
		if journal != nil && containsOCPString(journal.Cohorts, cohort) {
			if _, err := ocpConfigurationConsumer(ctx, c, current, journal); err != nil {
				return nil, err
			}
		}
		live = append(live, current)
	}
	return live, nil
}
func ocpConfigurationRestoreTargets(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, docs []*unstructured.Unstructured, cohort string) ([]ocpConfigurationRestoreTarget, error) {
	if c == nil || cfg == nil || cfg.NetworkOperator == nil || cohort == "" {
		return nil, fmt.Errorf("OpenShift restoration requires client, networkOperator configuration and bundle cohort")
	}
	targets := []ocpConfigurationRestoreTarget{}
	seen := map[string]bool{}
	for _, doc := range docs {
		if doc == nil || !isOCPOperatorConfig(doc) {
			return nil, fmt.Errorf("unexpected OpenShift restore configuration identity")
		}
		key := doc.GroupVersionKind().String() + "/" + client.ObjectKeyFromObject(doc).String()
		if !seen[key] {
			targets = append(targets, ocpConfigurationRestoreTarget{Object: doc.DeepCopy()})
			seen[key] = true
		}
	}
	for _, obj := range ocpConfigurationSingletons(cfg) {
		key := obj.GroupVersionKind().String() + "/" + client.ObjectKeyFromObject(obj).String()
		if !seen[key] {
			targets = append(targets, ocpConfigurationRestoreTarget{Object: obj, Optional: true})
			seen[key] = true
		}
	}
	if err := checkOCPConfigurationMapping(ctx, c, cfg, cohort); err != nil {
		return nil, err
	}
	namespace := cfg.NetworkOperator.Namespace
	if namespace == "" {
		namespace = "nvidia-network-operator"
	}
	sub, err := findOCPOperatorSubscription(ctx, c, ocpOperator{packageName: "nvidia-network-operator", namespace: namespace})
	if err != nil {
		return nil, err
	}
	if sub != nil {
		targets = append(targets, ocpConfigurationRestoreTarget{Object: sub})
	}
	return targets, nil
}

// Release only this bundle's claims, after all its consuming CRs are gone.
// Missing provenance, other claims and changed fields produce retained messages.
func restoreOCPOperatorConfiguration(ctx context.Context, c client.Client, targets []*unstructured.Unstructured, cohort string, skipWait bool) ([]string, error) {
	var messages []string
	for _, target := range targets {
		retained, err := restoreOCPConfigurationObject(ctx, c, target, cohort, skipWait)
		messages = append(messages, retained...)
		if err != nil {
			return messages, err
		}
	}
	return messages, nil
}
func restoreOCPConfigurationObject(ctx context.Context, c client.Client, target *unstructured.Unstructured, cohort string, skipWait bool) ([]string, error) {
	messages := []string{}
	var deleted *unstructured.Unstructured
	var subscription *unstructured.Unstructured
	restoredAPI := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		messages = nil
		deleted = nil
		subscription = nil
		restoredAPI = false
		current := target.DeepCopy()
		key := client.ObjectKeyFromObject(target)
		if err := c.Get(ctx, key, current); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		if current.GetUID() != target.GetUID() || current.GetAnnotations()[ocpRollbackAnnotation] != target.GetAnnotations()[ocpRollbackAnnotation] {
			return fmt.Errorf("%s %s ownership changed after planning; inspect cleanup again", current.GetKind(), key)
		}
		journal, err := readOCPConfigurationJournal(current)
		if err != nil {
			return err
		}
		retained := func(reason string) {
			messages = append(messages, fmt.Sprintf("Retained OpenShift %s %s: %s", current.GetKind(), key, reason))
		}
		if journal == nil {
			retained("no recorded prior values (legacy or externally configured); inspect shared configuration with its owner")
			return nil
		}
		if journal.UID == "" {
			retained("provenance was not bound to a server-assigned object UID; inspect interrupted deployment with the configuration owner")
			return nil
		}
		if !containsOCPString(journal.Cohorts, cohort) {
			retained("this bundle has no recorded claim; preserve configuration owned by another deployment")
			return nil
		}
		remaining := []string{}
		for _, owner := range journal.Cohorts {
			if owner != cohort {
				remaining = append(remaining, owner)
			}
		}
		if len(remaining) > 0 {
			journal.Cohorts = remaining
			retained(fmt.Sprintf("still required by %d other deployment cohort(s); clean those bundles before restoring shared settings", len(remaining)))
			if err := writeOCPConfigurationJournal(current, journal); err != nil {
				return err
			}
			return c.Update(ctx, current)
		}
		if consumer, err := ocpConfigurationConsumer(ctx, c, current, journal); err != nil {
			return err
		} else if consumer != "" {
			retained(consumer + "; preserved this bundle's claim and provenance; remove or migrate that consumer with its owner before retrying")
			return nil
		}
		if journal.Created != nil {
			if !reflect.DeepEqual(journal.Created, ocpCreatedConfigurationIntent(current)) {
				retained("l8k-created configuration has subsequent changes; inspect it with its owner before deleting")
				return nil
			}
			deleted = current.DeepCopy()
			uid, rv := current.GetUID(), current.GetResourceVersion()
			err := c.Delete(ctx, current, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		before := current.DeepCopy()
		view, err := ocpConfigurationView(current)
		if err != nil {
			return err
		}
		pending := []ocpConfigurationField{}
		for _, field := range journal.Fields {
			value, err := ocpConfigurationValueAt(view, field.Path)
			if err != nil {
				return err
			}
			restored, canRestore := restoreOCPConfigurationValue(field, value)
			if !canRestore {
				retained("field " + strings.Join(field.Path, ".") + " changed after deployment; inspect its current value before restoring")
				pending = append(pending, field)
				continue
			}
			if current.GetKind() == "Subscription" {
				if journal.RestoreEnv == nil {
					journal.RestoreEnv = map[string]ocpConfigurationValue{}
				}
				journal.RestoreEnv[field.Path[1]] = restored
			}
			if restored.Present {
				if err := unstructured.SetNestedField(view, restored.Value, field.Path...); err != nil {
					return err
				}
			} else {
				unstructured.RemoveNestedField(view, field.Path...)
				for i := len(field.MissingParents) - 1; i >= 0; i-- {
					path := field.MissingParents[i]
					value, present, err := unstructured.NestedMap(view, path...)
					if err != nil {
						return err
					}
					if present && len(value) == 0 {
						unstructured.RemoveNestedField(view, path...)
					}
				}
			}
		}
		if err := setOCPConfigurationView(current, view); err != nil {
			return err
		}
		if current.GetKind() == "Subscription" && journal.EnvWasPresent != nil && !*journal.EnvWasPresent {
			env, _, err := unstructured.NestedSlice(current.Object, "spec", "config", "env")
			if err != nil {
				return err
			}
			if len(env) == 0 {
				unstructured.RemoveNestedField(current.Object, "spec", "config", "env")
				config, _, err := unstructured.NestedMap(current.Object, "spec", "config")
				if err != nil {
					return err
				}
				if len(config) == 0 && !journal.ConfigWasPresent {
					unstructured.RemoveNestedField(current.Object, "spec", "config")
				}
				spec, _, err := unstructured.NestedMap(current.Object, "spec")
				if err != nil {
					return err
				}
				if len(spec) == 0 && !journal.SpecWasPresent {
					unstructured.RemoveNestedField(current.Object, "spec")
				}
			}
		}
		if current.GetKind() == "NodeFeatureDiscovery" && journal.NFDOriginal != nil && len(pending) == 0 {
			live, _, err := unstructured.NestedString(before.Object, "spec", "workerConfig", "configData")
			if err != nil {
				return err
			}
			originalObj := before.DeepCopy()
			if journal.NFDOriginal.Present {
				if err := unstructured.SetNestedField(originalObj.Object, journal.NFDOriginal.Value, "spec", "workerConfig", "configData"); err != nil {
					return err
				}
			} else {
				unstructured.RemoveNestedField(originalObj.Object, "spec", "workerConfig", "configData")
			}
			originalView, err := ocpConfigurationView(originalObj)
			if err != nil {
				return err
			}
			if live == journal.NFDApplied && reflect.DeepEqual(originalView, view) {
				if journal.NFDOriginal.Present {
					if err := unstructured.SetNestedField(current.Object, journal.NFDOriginal.Value, "spec", "workerConfig", "configData"); err != nil {
						return err
					}
				} else {
					unstructured.RemoveNestedField(current.Object, "spec", "workerConfig", "configData")
				}
			}
		}
		if len(journal.Fields) == 0 && len(journal.RestoreEnv) == 0 {
			retained("no changed fields were recorded; pre-existing or legacy shared settings remain in place and require owner review")
		}
		if current.GetKind() == "NodeFeatureDiscovery" || isOCPSysctlAllowlistPatch(current) {
			path := []string{"data", "allowlist.conf"}
			if current.GetKind() == "NodeFeatureDiscovery" {
				path = []string{"spec", "workerConfig", "configData"}
			}
			old, err := ocpConfigurationValueAt(before.Object, path)
			if err != nil {
				return err
			}
			next, err := ocpConfigurationValueAt(current.Object, path)
			if err != nil {
				return err
			}
			restoredAPI = !reflect.DeepEqual(old, next)
		}
		journal.Fields = pending
		needsSubscriptionWait := len(journal.RestoreEnv) > 0
		if len(pending) == 0 && len(journal.RestoreEnv) == 0 {
			journal = nil
		}
		if err := writeOCPConfigurationJournal(current, journal); err != nil {
			return err
		}
		if needsSubscriptionWait {
			subscription = current.DeepCopy()
		}
		if reflect.DeepEqual(before.Object, current.Object) {
			return nil
		}
		return c.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return messages, fmt.Errorf("restore OpenShift %s %s: %w", target.GetKind(), client.ObjectKeyFromObject(target), err)
	}
	if deleted != nil {
		if err := waitForOCPCreatedConfigurationDeletion(ctx, c, deleted); err != nil {
			return messages, err
		}
	}
	if subscription != nil {
		if skipWait {
			messages = append(messages, fmt.Sprintf("OpenShift Subscription %s API configuration restored; rollout wait skipped; rerun clean without --skip-wait to verify adoption", client.ObjectKeyFromObject(subscription)))
		} else if err := completeOCPSubscriptionRestore(ctx, c, subscription, cohort); err != nil {
			return messages, err
		}
	}
	if (restoredAPI || deleted != nil) && (target.GetKind() == "NodeFeatureDiscovery" || isOCPSysctlAllowlistPatch(target)) {
		messages = append(messages, fmt.Sprintf("OpenShift %s %s API configuration restored; worker propagation is unverified; inspect the platform controllers and workload behavior", target.GetKind(), client.ObjectKeyFromObject(target)))
	}
	return messages, nil
}
func ocpConfigurationAdditions(field ocpConfigurationField) []interface{} {
	var before, after []interface{}
	if additiveOCPRollbackPath(field.Path) {
		before, _ = field.Before.Value.([]interface{})
		after, _ = field.After.Value.([]interface{})
	} else if strings.Join(field.Path, "/") == "data/allowlist.conf" {
		a, _ := field.Before.Value.(string)
		b, _ := field.After.Value.(string)
		for _, line := range strings.Split(a, "\n") {
			before = append(before, strings.TrimSpace(line))
		}
		for _, line := range strings.Split(b, "\n") {
			if entry := strings.TrimSpace(line); entry != "" {
				after = append(after, entry)
			}
		}
	}
	added := []interface{}{}
	for _, entry := range after {
		if !ocpListContains(before, entry) {
			added = append(added, entry)
		}
	}
	return added
}
func restoreOCPConfigurationValue(field ocpConfigurationField, current ocpConfigurationValue) (ocpConfigurationValue, bool) {
	if additiveOCPRollbackPath(field.Path) {
		if !current.Present {
			return current, true
		}
		have, ok := current.Value.([]interface{})
		if !ok {
			return current, false
		}
		kept := []interface{}{}
		for _, item := range have {
			if !ocpListContains(field.Added, item) {
				kept = append(kept, item)
			}
		}
		if len(kept) == 0 && !field.Before.Present {
			return field.Before, true
		}
		return ocpConfigurationValue{Present: current.Present, Value: kept}, true
	}
	if strings.Join(field.Path, "/") == "data/allowlist.conf" {
		if !current.Present {
			return current, true
		}
		have, ok := current.Value.(string)
		if !ok {
			return current, false
		}
		baseline, _ := field.Before.Value.(string)
		expected := baseline
		for _, item := range field.Added {
			entry, ok := item.(string)
			if !ok {
				return current, false
			}
			if expected != "" && !strings.HasSuffix(expected, "\n") {
				expected += "\n"
			}
			expected += entry + "\n"
		}
		if have == expected {
			return field.Before, true
		}
		kept := strings.Builder{}
		for _, line := range strings.SplitAfter(have, "\n") {
			if !ocpListContains(field.Added, strings.TrimSpace(line)) {
				kept.WriteString(line)
			}
		}
		return ocpConfigurationValue{Present: true, Value: kept.String()}, true
	}
	if reflect.DeepEqual(current, field.Before) {
		return current, true
	}
	if reflect.DeepEqual(current, field.After) {
		return field.Before, true
	}
	return current, false
}

// Create cannot know its server UID beforehand. A crash before this second
// transaction leaves an unbound journal, which cleanup conservatively retains.
func bindOCPCreatedConfiguration(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	if dryRun || obj.GetUID() == "" {
		return nil
	}
	createdUID := obj.GetUID()
	createdIntent := ocpCreatedConfigurationIntent(obj)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := obj.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		if current.GetUID() != createdUID {
			return fmt.Errorf("created OpenShift configuration was replaced before UID binding")
		}
		journal, err := readOCPConfigurationJournal(current)
		if err != nil {
			return err
		}
		if journal == nil || journal.Created == nil {
			return fmt.Errorf("created OpenShift configuration lost rollback provenance before UID binding")
		}
		if journal.UID == string(createdUID) {
			obj.Object = current.Object
			return nil
		}
		before := current.DeepCopy()
		journal.UID = string(createdUID)
		// Only the actual Create response establishes the created intent. A
		// later read may include site changes and must never adopt those.
		journal.Created = createdIntent
		if err := writeOCPConfigurationJournal(current, journal); err != nil {
			return err
		}
		if err := c.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		obj.Object = current.Object
		return nil
	})
}
func ocpListContains(list []interface{}, item interface{}) bool {
	for _, value := range list {
		if reflect.DeepEqual(value, item) {
			return true
		}
	}
	return false
}
func setOCPConfigurationView(obj *unstructured.Unstructured, view map[string]interface{}) error {
	switch obj.GetKind() {
	case "NodeFeatureDiscovery":
		original, err := ocpConfigurationView(obj)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(original, view) {
			return nil
		}
		data, err := yaml.Marshal(view["nfd"])
		if err != nil {
			return err
		}
		return unstructured.SetNestedField(obj.Object, string(data), "spec", "workerConfig", "configData")
	case "Subscription":
		desired, _ := view["env"].(map[string]interface{})
		env, _, err := unstructured.NestedSlice(obj.Object, "spec", "config", "env")
		if err != nil {
			return err
		}
		out := []interface{}{}
		for _, entry := range env {
			item, ok := entry.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid Subscription environment entry")
			}
			name, _ := item["name"].(string)
			if name != "MAINTENANCE_OPERATOR_ENABLED" && name != "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE" {
				out = append(out, entry)
				continue
			}
			if value, exists := desired[name]; exists {
				out = append(out, value)
			}
		}
		return unstructured.SetNestedSlice(obj.Object, out, "spec", "config", "env")
	default:
		return nil
	}
}
