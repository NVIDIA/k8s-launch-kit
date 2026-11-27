// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ocpOperator struct {
	packageName, namespace, api string
}

func requiredOCPOperators(cfg *config.LaunchKitConfig, deployment bool) []ocpOperator {
	ns := "nvidia-network-operator"
	if cfg.NetworkOperator != nil && cfg.NetworkOperator.Namespace != "" {
		ns = cfg.NetworkOperator.Namespace
	}
	operators := []ocpOperator{{"nvidia-network-operator", ns, "nicclusterpolicies.mellanox.com"}}
	if deployment {
		sriovNamespace := config.DefaultSriovOperatorNamespace
		if cfg.Sriov != nil && cfg.Sriov.OperatorNamespace != "" {
			sriovNamespace = cfg.Sriov.OperatorNamespace
		}
		nfdNamespace := config.DefaultNFDOperatorNamespace
		if cfg.NFD != nil && cfg.NFD.OperatorNamespace != "" {
			nfdNamespace = cfg.NFD.OperatorNamespace
		}
		maintenanceNamespace := config.DefaultMaintenanceOperatorNamespace
		if cfg.Maintenance != nil && cfg.Maintenance.OperatorNamespace != "" {
			maintenanceNamespace = cfg.Maintenance.OperatorNamespace
		}
		if cfg.Profile != nil && cfg.Profile.Deployment == "sriov" {
			operators = append(operators, ocpOperator{"sriov-network-operator", sriovNamespace, "sriovoperatorconfigs.sriovnetwork.openshift.io"})
		}
		operators = append(operators,
			ocpOperator{"nfd", nfdNamespace, "nodefeaturediscoveries.nfd.openshift.io"},
			ocpOperator{"nvidia-maintenance-operator", maintenanceNamespace, "maintenanceoperatorconfigs.maintenance.nvidia.com"})
	}
	return operators
}

// CheckOCPOperators checks only the expected OLM installation and the API
// needed by each generated operator configuration. The caller must select
// OpenShift before invoking this platform-specific check.
func CheckOCPOperators(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, deployment bool) error {
	if ctx == nil || c == nil || cfg == nil {
		return fmt.Errorf("OpenShift operator checks require context, client and cluster config")
	}
	for _, op := range requiredOCPOperators(cfg, deployment) {
		observed, err := inspectOCPOperator(ctx, c, op)
		if err != nil {
			return err
		}
		if observed.Issue != "" {
			return errors.New(observed.Issue)
		}
		if issue := ocpOperatorVersionIssue(cfg, observed); issue != "" {
			return errors.New(issue)
		}
	}
	return nil
}

func inspectOCPOperator(ctx context.Context, c client.Client, op ocpOperator) (config.DiscoveredOCPOperator, error) {
	observed := config.DiscoveredOCPOperator{Package: op.packageName, Namespace: op.namespace, CSV: "", Version: "", Issue: ""}
	csv, issue, err := inspectInstalledCSV(ctx, c, op)
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			observed.Issue = err.Error()
			return observed, err
		}
		return observed, err
	}
	if issue != "" {
		observed.Issue = issue
		return observed, nil
	}
	observed.CSV = csv.GetName()
	observed.Version, _, _ = unstructured.NestedString(csv.Object, "spec", "version")
	phase, _, _ := unstructured.NestedString(csv.Object, "status", "phase")
	if phase != "Succeeded" {
		observed.Issue = fmt.Sprintf("OpenShift %s CSV %s in %s is %q, expected Succeeded", op.packageName, csv.GetName(), op.namespace, phase)
		return observed, nil
	}
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	if err := c.Get(ctx, types.NamespacedName{Name: op.api}, crd); err != nil {
		wrapped := fmt.Errorf("OpenShift %s required API %s: %w", op.packageName, op.api, err)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			observed.Issue = wrapped.Error()
			return observed, wrapped
		}
		return observed, wrapped
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	served := false
	for _, version := range versions {
		fields, ok := version.(map[string]interface{})
		if ok && fields["served"] == true {
			served = true
			break
		}
	}
	conditions, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	established := false
	for _, condition := range conditions {
		fields, ok := condition.(map[string]interface{})
		if ok && fields["type"] == "Established" && fields["status"] == "True" {
			established = true
			break
		}
	}
	if !served || !established {
		observed.Issue = fmt.Sprintf("OpenShift %s required API %s is not served and established", op.packageName, op.api)
	}
	return observed, nil
}

// DiscoverOCPOperators records the effective profile's required installations.
// Missing/unready prerequisites are observations; failed inspections are errors.
func DiscoverOCPOperators(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig) ([]config.DiscoveredOCPOperator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("OpenShift operator discovery requires a cluster config")
	}
	if cfg.Flavor != config.FlavorOCP {
		return nil, nil
	}
	if ctx == nil || c == nil {
		return nil, fmt.Errorf("OpenShift operator discovery requires context and client")
	}
	observations := make([]config.DiscoveredOCPOperator, 0)
	for _, op := range requiredOCPOperators(cfg, true) {
		observed, err := inspectOCPOperator(ctx, c, op)
		if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return nil, err
		}
		observations = append(observations, observed)
	}
	return observations, nil
}

// MissingOCPOperatorPrerequisites reports only facts about observed exact
// package/namespace identities. Offline and legacy inputs without such evidence
// retain their behavior; deployment always performs its own live checks.
func MissingOCPOperatorPrerequisites(cfg *config.LaunchKitConfig) []string {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return nil
	}
	var issues []string
	for _, op := range requiredOCPOperators(cfg, true) {
		for _, observed := range cfg.DiscoveredOCPOperators {
			if observed.Package != op.packageName || observed.Namespace != op.namespace {
				continue
			}
			issue := observed.Issue
			if issue == "" {
				issue = ocpOperatorVersionIssue(cfg, observed)
			}
			if issue != "" {
				issues = append(issues, issue)
			}
		}
	}
	return issues
}

func ocpOperatorVersionIssue(cfg *config.LaunchKitConfig, observed config.DiscoveredOCPOperator) string {
	if observed.Package == "nvidia-network-operator" && cfg.NetworkOperator != nil && cfg.NetworkOperator.SelectedRelease != "" {
		if !strings.HasPrefix(strings.TrimPrefix(observed.Version, "v"), cfg.NetworkOperator.SelectedRelease+".") {
			return fmt.Sprintf("OpenShift Network Operator CSV version %q does not match selected release %q", observed.Version, cfg.NetworkOperator.SelectedRelease)
		}
	}
	return ""
}

func installedCSV(ctx context.Context, c client.Client, op ocpOperator) (*unstructured.Unstructured, error) {
	csv, issue, err := inspectInstalledCSV(ctx, c, op)
	if issue != "" {
		return nil, errors.New(issue)
	}
	return csv, err
}

// inspectInstalledCSV keeps expected installation gaps separate from failed reads.
func inspectInstalledCSV(ctx context.Context, c client.Client, op ocpOperator) (*unstructured.Unstructured, string, error) {
	sub, err := findOCPOperatorSubscription(ctx, c, op)
	if err != nil {
		return nil, "", err
	}
	csvName := ""
	if sub != nil {
		csvName, _, _ = unstructured.NestedString(sub.Object, "status", "installedCSV")
		if csvName == "" {
			return nil, fmt.Sprintf("OpenShift %s Subscription %s/%s has no installedCSV", op.packageName, op.namespace, sub.GetName()), nil
		}
	} else {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "ClusterServiceVersionList"})
		if err := c.List(ctx, list, client.InNamespace(op.namespace)); err != nil {
			return nil, "", fmt.Errorf("list CSVs in %s: %w", op.namespace, err)
		}
		for _, item := range list.Items {
			if item.GetLabels()["olm.copiedFrom"] != "" || item.GetAnnotations()["olm.copiedFrom"] != "" {
				continue
			}
			if strings.HasPrefix(item.GetName(), op.packageName+".") {
				if csvName != "" {
					return nil, "", fmt.Errorf("multiple %s CSVs in %s without a Subscription", op.packageName, op.namespace)
				}
				csvName = item.GetName()
			}
		}
		if csvName == "" {
			return nil, fmt.Sprintf("OpenShift %s is not installed in %s", op.packageName, op.namespace), nil
		}
	}
	csv := &unstructured.Unstructured{}
	csv.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "ClusterServiceVersion"})
	if err := c.Get(ctx, types.NamespacedName{Namespace: op.namespace, Name: csvName}, csv); err != nil {
		return nil, "", fmt.Errorf("read %s CSV %s: %w", op.packageName, csvName, err)
	}
	if csv.GetLabels()["olm.copiedFrom"] != "" || csv.GetAnnotations()["olm.copiedFrom"] != "" {
		return nil, fmt.Sprintf("OpenShift %s CSV %s in %s is copied from another namespace", op.packageName, csv.GetName(), op.namespace), nil
	}
	if !strings.HasPrefix(csv.GetName(), op.packageName+".") {
		return nil, "", fmt.Errorf("unexpected %s CSV %s", op.packageName, csv.GetName())
	}
	return csv, "", nil
}

// findOCPOperatorSubscription identifies an OLM package by spec.name while
// preserving the common exact-name Get path and its narrower RBAC needs.
func findOCPOperatorSubscription(ctx context.Context, c client.Client, op ocpOperator) (*unstructured.Unstructured, error) {
	sub := &unstructured.Unstructured{}
	sub.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "Subscription"})
	err := c.Get(ctx, types.NamespacedName{Namespace: op.namespace, Name: op.packageName}, sub)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read %s Subscription in %s: %w", op.packageName, op.namespace, err)
	}
	if err == nil {
		packageName, _, _ := unstructured.NestedString(sub.Object, "spec", "name")
		if packageName != op.packageName {
			return nil, fmt.Errorf("subscription %s/%s installs %q, expected %q", op.namespace, op.packageName, packageName, op.packageName)
		}
		return sub, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "SubscriptionList"})
	if err := c.List(ctx, list, client.InNamespace(op.namespace)); err != nil {
		return nil, fmt.Errorf("list Subscriptions in %s: %w", op.namespace, err)
	}
	var found *unstructured.Unstructured
	for i := range list.Items {
		item := &list.Items[i]
		packageName, _, _ := unstructured.NestedString(item.Object, "spec", "name")
		if packageName != op.packageName {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple Subscriptions install %s in %s", op.packageName, op.namespace)
		}
		found = item.DeepCopy()
	}
	return found, nil
}

// CheckOCPOperatorVersion reports the certified Network Operator CSV version.
// OLM bundle suffixes such as 26.7.0-1 are revisions of the 26.7 release.
func CheckOCPOperatorVersion(ctx context.Context, c client.Client, namespace, selectedRelease string) (*VersionCheck, error) {
	if selectedRelease == "" {
		return &VersionCheck{Skipped: true, Reason: "no networkOperator.selectedRelease in user-config"}, nil
	}
	csv, err := installedCSV(ctx, c, ocpOperator{packageName: "nvidia-network-operator", namespace: namespace})
	if err != nil {
		return nil, err
	}
	version, _, _ := unstructured.NestedString(csv.Object, "spec", "version")
	phase, _, _ := unstructured.NestedString(csv.Object, "status", "phase")
	return &VersionCheck{
		SelectedRelease: selectedRelease,
		ExpectedVersion: selectedRelease,
		DeployedRelease: &HelmReleaseInfo{Name: csv.GetName(), AppVersion: version, Status: phase},
		Match:           phase == "Succeeded" && strings.HasPrefix(strings.TrimPrefix(version, "v"), selectedRelease+"."),
	}, nil
}
