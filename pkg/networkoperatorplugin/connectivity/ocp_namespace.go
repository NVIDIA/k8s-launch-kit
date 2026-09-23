// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const networksAnnotation = "k8s.v1.cni.cncf.io/networks"

// isolateOpenShiftValidation moves temporary workloads and their SCC grant
// into namespaces that workload-project pod creators cannot use. Only the
// network attachment's CNI config and resource annotation are copied.
func isolateOpenShiftValidation(ctx context.Context, c client.Client, daemonSets []*unstructured.Unstructured,
	refs []DaemonSetRef, support []*unstructured.Unstructured, created *[]client.Object) error {
	if len(daemonSets) != len(refs) {
		return fmt.Errorf("validation DaemonSet and reference counts differ")
	}
	sourceNamespaces := map[string]struct{}{}
	for _, ref := range refs {
		if ref.Namespace == "" {
			return fmt.Errorf("OpenShift validation DaemonSet %s has no namespace", ref.Name)
		}
		sourceNamespaces[ref.Namespace] = struct{}{}
	}
	sources := make([]string, 0, len(sourceNamespaces))
	for source := range sourceNamespaces {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	targets := map[string]string{}
	for _, source := range sources {
		random := make([]byte, 6)
		if _, err := rand.Read(random); err != nil {
			return fmt.Errorf("generate validation namespace: %w", err)
		}
		name := "l8k-validation-" + hex.EncodeToString(random)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := c.Create(ctx, ns); err != nil {
			return fmt.Errorf("create isolated validation namespace %s: %w", name, err)
		}
		*created = append(*created, ns)
		targets[source] = name
	}

	copiedNetworks := map[string]struct{}{}
	copiedSecrets := map[string]struct{}{}
	for i, obj := range daemonSets {
		source := refs[i].Namespace
		pullSecrets, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "imagePullSecrets")
		if err != nil {
			return fmt.Errorf("read validation DaemonSet %s pull secrets: %w", obj.GetName(), err)
		}
		if found {
			for _, entry := range pullSecrets {
				secretRef, ok := entry.(map[string]interface{})
				if !ok {
					return fmt.Errorf("validation DaemonSet %s has invalid pull secret reference", obj.GetName())
				}
				name, ok := secretRef["name"].(string)
				if !ok || name == "" {
					return fmt.Errorf("validation DaemonSet %s has unnamed pull secret", obj.GetName())
				}
				key := source + "/" + name
				if _, copied := copiedSecrets[key]; copied {
					continue
				}
				original := &corev1.Secret{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: source, Name: name}, original); err != nil {
					return fmt.Errorf("read validation image pull secret %s: %w", key, err)
				}
				copy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: targets[source]},
					Type: original.Type, Data: original.Data}
				if err := c.Create(ctx, copy); err != nil {
					return fmt.Errorf("copy validation image pull secret %s into %s: %w", key, targets[source], err)
				}
				*created = append(*created, copy)
				copiedSecrets[key] = struct{}{}
			}
		}
		annotation, found, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", networksAnnotation)
		if err != nil || !found || annotation == "" {
			return fmt.Errorf("OpenShift validation DaemonSet %s has no usable Multus network annotation: %v", obj.GetName(), err)
		}
		var localNames []string
		for _, selection := range strings.Split(annotation, ",") {
			selection = strings.TrimSpace(selection)
			if selection == "" || strings.ContainsAny(selection, "[]{} ") {
				return fmt.Errorf("OpenShift validation DaemonSet %s has unsupported network selection %q", obj.GetName(), selection)
			}
			parts := strings.Split(selection, "/")
			name := selection
			if len(parts) == 2 {
				if parts[0] != source {
					return fmt.Errorf("OpenShift validation DaemonSet %s refers to network outside workload namespace: %q", obj.GetName(), selection)
				}
				name = parts[1]
			} else if len(parts) != 1 {
				return fmt.Errorf("OpenShift validation DaemonSet %s has unsupported network selection %q", obj.GetName(), selection)
			}
			if name == "" {
				return fmt.Errorf("OpenShift validation DaemonSet %s has empty network name", obj.GetName())
			}
			key := source + "/" + name
			if _, copied := copiedNetworks[key]; !copied {
				original := &unstructured.Unstructured{}
				original.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"})
				if err := c.Get(ctx, client.ObjectKey{Namespace: source, Name: name}, original); err != nil {
					return fmt.Errorf("read validation network %s: %w", key, err)
				}
				config, found, err := unstructured.NestedString(original.Object, "spec", "config")
				if err != nil || !found {
					return fmt.Errorf("validation network %s has no config: %v", key, err)
				}
				annotations := map[string]interface{}{}
				if resourceName := original.GetAnnotations()["k8s.v1.cni.cncf.io/resourceName"]; resourceName != "" {
					annotations["k8s.v1.cni.cncf.io/resourceName"] = resourceName
				}
				copy := &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "k8s.cni.cncf.io/v1",
					"kind":       "NetworkAttachmentDefinition",
					"metadata": map[string]interface{}{
						"name":        name,
						"namespace":   targets[source],
						"annotations": annotations,
					},
					"spec": map[string]interface{}{"config": config},
				}}
				if err := c.Create(ctx, copy); err != nil {
					return fmt.Errorf("copy validation network %s into %s: %w", key, targets[source], err)
				}
				*created = append(*created, copy)
				copiedNetworks[key] = struct{}{}
			}
			localNames = append(localNames, name)
		}
		if err := unstructured.SetNestedField(obj.Object, strings.Join(localNames, ","),
			"spec", "template", "metadata", "annotations", networksAnnotation); err != nil {
			return err
		}
		obj.SetNamespace(targets[source])
		refs[i].Namespace = targets[source]
	}

	// The generated Role's SCC resourceName identifies which cluster-scoped
	// SCC belongs to each source namespace.
	sccNames := map[string]string{}
	for _, obj := range support {
		if obj.GetKind() != "Role" {
			continue
		}
		rules, ok := obj.Object["rules"].([]interface{})
		if !ok || len(rules) != 1 {
			return fmt.Errorf("OpenShift validation Role %s has unexpected SCC rules", obj.GetName())
		}
		rule, ok := rules[0].(map[string]interface{})
		if !ok {
			return fmt.Errorf("OpenShift validation Role %s has invalid SCC rule", obj.GetName())
		}
		resourceNames, ok := rule["resourceNames"].([]interface{})
		if !ok || len(resourceNames) != 1 {
			return fmt.Errorf("OpenShift validation Role %s has no unique SCC resource", obj.GetName())
		}
		oldName, ok := resourceNames[0].(string)
		if !ok || targets[obj.GetNamespace()] == "" {
			return fmt.Errorf("OpenShift validation Role %s has unknown namespace or SCC", obj.GetName())
		}
		sccNames[oldName] = targets[obj.GetNamespace()]
		rule["resourceNames"] = []interface{}{targets[obj.GetNamespace()]}
	}
	for _, obj := range support {
		if obj.GetKind() == "SecurityContextConstraints" {
			newName := sccNames[obj.GetName()]
			if newName == "" {
				return fmt.Errorf("OpenShift validation SCC %s has no matching Role", obj.GetName())
			}
			obj.SetName(newName)
			continue
		}
		target := targets[obj.GetNamespace()]
		if target == "" {
			return fmt.Errorf("OpenShift validation support %s/%s has unknown namespace %s", obj.GetKind(), obj.GetName(), obj.GetNamespace())
		}
		obj.SetNamespace(target)
		if obj.GetKind() == "RoleBinding" {
			subjects, ok := obj.Object["subjects"].([]interface{})
			if !ok || len(subjects) != 1 {
				return fmt.Errorf("OpenShift validation RoleBinding %s has unexpected subjects", obj.GetName())
			}
			subject, ok := subjects[0].(map[string]interface{})
			if !ok || subject["kind"] != "ServiceAccount" {
				return fmt.Errorf("OpenShift validation RoleBinding %s has invalid subject", obj.GetName())
			}
			subject["namespace"] = target
		}
	}
	return nil
}
