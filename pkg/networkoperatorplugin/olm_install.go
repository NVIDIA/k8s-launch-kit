// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Offline fallbacks, used when discover cannot read a value from a PackageManifest.
const (
	olmCatalogCertified    = "certified-operators"
	olmCatalogRedHat       = "redhat-operators"
	olmCatalogSourceNS     = "openshift-marketplace"
	olmDefaultChannel      = "stable"
	olmNetworkOperatorPkg  = "nvidia-network-operator"
	olmMaintenanceOperator = "nvidia-maintenance-operator"
)

// fallbackOLMSource maps a package to its catalog: NVIDIA operators are
// certified, everything else is Red Hat.
func fallbackOLMSource(packageName string) string {
	switch packageName {
	case olmNetworkOperatorPkg, olmMaintenanceOperator:
		return olmCatalogCertified
	default:
		return olmCatalogRedHat
	}
}

// effectiveOperatorSubscriptions derives the operator set and namespaces from cfg
// (authoritative) and carries catalog coordinates and install-plan approval over
// from any saved descriptor of the same package, leaving unset fields empty.
// Saved identities apply only within the same or an unspecified namespace.
func effectiveOperatorSubscriptions(cfg *config.LaunchKitConfig) []config.OCPOperatorSubscription {
	saved := map[string]config.OCPOperatorSubscription{}
	if cfg.OCPOperatorInstall != nil {
		for _, op := range cfg.OCPOperatorInstall.Operators {
			saved[op.Package] = op
		}
	}
	required := requiredOCPOperators(cfg, true)
	subs := make([]config.OCPOperatorSubscription, 0, len(required))
	for _, op := range required {
		sub := config.OCPOperatorSubscription{Package: op.packageName, Namespace: op.namespace}
		if prev, ok := saved[op.packageName]; ok {
			if prev.Namespace == "" || prev.Namespace == op.namespace {
				sub.Name = prev.Name
				sub.RetainOnClean = prev.RetainOnClean
			}
			sub.Channel = prev.Channel
			sub.Source = prev.Source
			sub.SourceNamespace = prev.SourceNamespace
			sub.InstallPlanApproval = prev.InstallPlanApproval
		}
		subs = append(subs, sub)
	}
	return subs
}

// fillOperatorDefaults fills any unset catalog coordinate or approval with its
// offline default.
func fillOperatorDefaults(subs []config.OCPOperatorSubscription) {
	for i := range subs {
		op := &subs[i]
		if op.Name == "" {
			op.Name = op.Package
		}
		if op.Channel == "" {
			op.Channel = olmDefaultChannel
		}
		if op.Source == "" {
			op.Source = fallbackOLMSource(op.Package)
		}
		if op.SourceNamespace == "" {
			op.SourceNamespace = olmCatalogSourceNS
		}
		if op.InstallPlanApproval == "" {
			op.InstallPlanApproval = config.DefaultOCPInstallPlanApproval
		}
	}
}

// DefaultOCPOperatorSubscriptions returns fully-defaulted install descriptors for
// the operators l8k configures, carrying over any saved catalog/approval choices.
// discover overrides channel/source/sourceNamespace from each PackageManifest.
func DefaultOCPOperatorSubscriptions(cfg *config.LaunchKitConfig) []config.OCPOperatorSubscription {
	subs := effectiveOperatorSubscriptions(cfg)
	fillOperatorDefaults(subs)
	reconcileNetworkOperatorChannel(cfg, subs)
	return subs
}

// isReleaseChannel reports whether an OLM channel name pins the given release line
// (e.g. "v26.7" or "26.7" for release "26.7").
func isReleaseChannel(channel, release string) bool {
	return channel == "v"+release || channel == release
}

// reconcileNetworkOperatorChannel repoints the network-operator channel at the
// selected release line offline, so a saved or default channel left over from a
// different release does not render a Subscription on the wrong line. discover
// instead verifies the channel against the live PackageManifest.
func reconcileNetworkOperatorChannel(cfg *config.LaunchKitConfig, subs []config.OCPOperatorSubscription) {
	if cfg.NetworkOperator == nil || cfg.NetworkOperator.SelectedRelease == "" {
		return
	}
	release := cfg.NetworkOperator.SelectedRelease
	for i := range subs {
		if subs[i].Package == olmNetworkOperatorPkg && !isReleaseChannel(subs[i].Channel, release) {
			subs[i].Channel = "v" + release
		}
	}
}

// effectiveOperatorGroups derives one OperatorGroup per unique operator namespace
// (authoritative from cfg) and carries over a saved group's name and target
// namespaces, leaving unset fields empty.
func effectiveOperatorGroups(cfg *config.LaunchKitConfig) []config.OCPOperatorGroup {
	saved := map[string]config.OCPOperatorGroup{}
	if cfg.OCPOperatorInstall != nil {
		for _, g := range cfg.OCPOperatorInstall.OperatorGroups {
			saved[g.Namespace] = g
		}
	}
	seen := map[string]bool{}
	var groups []config.OCPOperatorGroup
	for _, op := range requiredOCPOperators(cfg, true) {
		if op.namespace == "" || seen[op.namespace] {
			continue
		}
		seen[op.namespace] = true
		group := config.OCPOperatorGroup{Namespace: op.namespace}
		if prev, ok := saved[op.namespace]; ok {
			group.Name = prev.Name
			group.TargetNamespaces = prev.TargetNamespaces
			group.RetainOnClean = prev.RetainOnClean
		}
		groups = append(groups, group)
	}
	return groups
}

// fillOperatorGroupDefaults names each group after its namespace and scopes it
// OwnNamespace when discovery did not reuse an existing group.
func fillOperatorGroupDefaults(groups []config.OCPOperatorGroup) {
	for i := range groups {
		g := &groups[i]
		if g.Name == "" {
			g.Name = g.Namespace
		}
		if g.TargetNamespaces == nil {
			g.TargetNamespaces = []string{g.Namespace}
		}
	}
}

var operatorGroupListGVK = schema.GroupVersionKind{
	Group:   "operators.coreos.com",
	Version: "v1",
	Kind:    "OperatorGroupList",
}

// existingOperatorGroup returns the single OperatorGroup in namespace, or nil when
// there is none. It errors on more than one (OLM's TooManyOperatorGroups) and on a
// selector-based group, which this descriptor cannot faithfully reproduce.
func existingOperatorGroup(ctx context.Context, c client.Client, namespace string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(operatorGroupListGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list OperatorGroups in %s: %w", namespace, err)
	}
	switch len(list.Items) {
	case 0:
		return nil, nil
	case 1:
		og := &list.Items[0]
		if selector, _, _ := unstructured.NestedMap(og.Object, "spec", "selector"); len(selector) > 0 {
			return nil, fmt.Errorf("OperatorGroup %s/%s is selector-based; manage its membership manually", namespace, og.GetName())
		}
		return og, nil
	default:
		return nil, fmt.Errorf("namespace %s already has %d OperatorGroups (TooManyOperatorGroups); resolve to one before managing operators", namespace, len(list.Items))
	}
}

var packageManifestListGVK = schema.GroupVersionKind{
	Group:   "packages.operators.coreos.com",
	Version: "v1",
	Kind:    "PackageManifestList",
}

// DiscoverOCPOperatorInstall resolves each configured operator's
// channel/source/sourceNamespace from its OLM PackageManifest and stores the
// result in cfg.OCPOperatorInstall. A package with no manifest keeps the offline
// fallback and warns.
func DiscoverOCPOperatorInstall(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, out ui.Output) error {
	return discoverOCPOperatorInstall(ctx, c, cfg, out, true)
}

// DiscoverOCPOperatorInstallIdentities saves existing OLM identities without
// opting into management or reading PackageManifests. A later managed generation
// can then reuse external installations without claiming them for cleanup.
// Only existing or configured descriptors are saved; generation supplies defaults.
// Callers retain inspection errors: an unverified identity must not be replaced
// by an offline default when generating installation artifacts.
func DiscoverOCPOperatorInstallIdentities(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig) error {
	return discoverOCPOperatorInstall(ctx, c, cfg, nil, false)
}

func discoverOCPOperatorInstall(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, out ui.Output, withCatalog bool) error {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return nil
	}
	if ctx == nil || c == nil {
		return fmt.Errorf("OpenShift operator install discovery requires a context and Kubernetes client")
	}
	operators := effectiveOperatorSubscriptions(cfg)

	var byPackage map[string][]*unstructured.Unstructured
	if withCatalog {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(packageManifestListGVK)
		if err := c.List(ctx, list); err != nil {
			return fmt.Errorf("list OLM PackageManifests: %w", err)
		}
		byPackage = make(map[string][]*unstructured.Unstructured, len(list.Items))
		for i := range list.Items {
			item := &list.Items[i]
			name, _, _ := unstructured.NestedString(item.Object, "status", "packageName")
			if name == "" {
				name = item.GetName()
			}
			byPackage[name] = append(byPackage[name], item)
		}
	}

	var observedOperators []config.OCPOperatorSubscription
	for i := range operators {
		op := &operators[i]
		// Always read the live Subscription name so a saved name does not go stale
		// if the installation is recreated under a different name. Propagate lookup
		// errors (forbidden, ambiguous, wrong package) so a failed inspection never
		// falls back to a package-named Subscription that would duplicate on apply.
		sub, err := findOCPOperatorSubscription(ctx, c, ocpOperator{packageName: op.Package, namespace: op.Namespace})
		if err != nil {
			return err
		}
		if sub != nil {
			op.Name = sub.GetName()
			op.RetainOnClean = op.RetainOnClean || retainDiscoveredOCPInstall(sub)
			if !withCatalog {
				// Without catalog enrichment, preserve the existing installation's
				// coordinates rather than redirect it to offline fallback sources.
				if op.Channel == "" {
					op.Channel, _, _ = unstructured.NestedString(sub.Object, "spec", "channel")
				}
				if op.Source == "" {
					op.Source, _, _ = unstructured.NestedString(sub.Object, "spec", "source")
				}
				if op.SourceNamespace == "" {
					op.SourceNamespace, _, _ = unstructured.NestedString(sub.Object, "spec", "sourceNamespace")
				}
				if op.InstallPlanApproval == "" {
					op.InstallPlanApproval, _, _ = unstructured.NestedString(sub.Object, "spec", "installPlanApproval")
				}
			}
		} else {
			// Clear stale retention after removal, but preserve an observed
			// CSV-only install that OLM could adopt through a new Subscription.
			op.RetainOnClean = false
			for _, observed := range cfg.DiscoveredOCPOperators {
				if observed.Package == op.Package && observed.Namespace == op.Namespace && observed.CSV != "" {
					op.RetainOnClean = true
					break
				}
			}
		}
		if !withCatalog {
			if op.Name != "" || op.Channel != "" || op.Source != "" || op.SourceNamespace != "" || op.InstallPlanApproval != "" || op.RetainOnClean {
				observedOperators = append(observedOperators, *op)
			}
			continue
		}
		manifests, ok := byPackage[op.Package]
		if !ok {
			if out != nil {
				out.Warning(
					"No OLM PackageManifest found for %q; keeping the configured or default channel/source. Verify the catalog is available before deploy.",
					op.Package)
			}
			continue
		}
		wantSource := op.Source
		if wantSource == "" {
			wantSource = fallbackOLMSource(op.Package)
		}
		manifest, err := selectPackageManifest(manifests, op.Package, wantSource)
		if err != nil {
			return err
		}
		if op.Package == olmNetworkOperatorPkg && cfg.NetworkOperator != nil && cfg.NetworkOperator.SelectedRelease != "" {
			channel, err := networkOperatorReleaseChannel(manifest, cfg.NetworkOperator.SelectedRelease)
			if err != nil {
				return err
			}
			op.Channel = channel
		} else if op.Channel == "" {
			if channel, _, _ := unstructured.NestedString(manifest.Object, "status", "defaultChannel"); channel != "" {
				op.Channel = channel
			}
		}
		if source, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSource"); source != "" {
			op.Source = source
		}
		if op.SourceNamespace == "" {
			if sourceNS, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSourceNamespace"); sourceNS != "" {
				op.SourceNamespace = sourceNS
			}
		}
	}

	if withCatalog {
		fillOperatorDefaults(operators)
	} else {
		operators = observedOperators
	}

	groups := effectiveOperatorGroups(cfg)
	var observedGroups []config.OCPOperatorGroup
	for i := range groups {
		g := &groups[i]
		// Always read the live namespace: a saved name goes stale if the group is
		// recreated under a different name, which would duplicate it on apply.
		existing, err := existingOperatorGroup(ctx, c, g.Namespace)
		if err != nil {
			return err
		}
		if existing != nil {
			g.Name = existing.GetName()
			g.RetainOnClean = g.RetainOnClean || retainDiscoveredOCPInstall(existing)
			targets, _, _ := unstructured.NestedStringSlice(existing.Object, "spec", "targetNamespaces")
			if targets == nil {
				targets = []string{}
			}
			g.TargetNamespaces = targets
		} else {
			g.RetainOnClean = false
		}
		if !withCatalog && (g.Name != "" || g.TargetNamespaces != nil) {
			observedGroups = append(observedGroups, *g)
		}
	}
	if withCatalog {
		fillOperatorGroupDefaults(groups)
	} else {
		groups = observedGroups
	}

	manage := withCatalog || cfg.OCPOperatorInstall != nil && cfg.OCPOperatorInstall.Manage
	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage:         manage,
		Operators:      operators,
		OperatorGroups: groups,
	}
	return nil
}

// Existing external installations stay external when their identities are reused.
// Already generated managed objects retain their cleanup behavior on rediscovery.
func retainDiscoveredOCPInstall(obj *unstructured.Unstructured) bool {
	annotations := obj.GetAnnotations()
	return annotations[ocpCleanRetainAnnotation] != "" || annotations[launchKitVersionAnnotation] == ""
}

// selectPackageManifest picks a package's PackageManifest. When several catalogs
// carry it, it prefers the one matching the configured source and errors when
// none does, rather than silently choosing by API list order.
func selectPackageManifest(manifests []*unstructured.Unstructured, pkg, wantSource string) (*unstructured.Unstructured, error) {
	if len(manifests) == 1 {
		return manifests[0], nil
	}
	sources := make([]string, 0, len(manifests))
	for _, m := range manifests {
		source, _, _ := unstructured.NestedString(m.Object, "status", "catalogSource")
		if source == wantSource {
			return m, nil
		}
		sources = append(sources, source)
	}
	return nil, fmt.Errorf("OLM package %q is in multiple catalogs %v but none is the expected source %q; set its source explicitly in ocpOperatorInstall",
		pkg, sources, wantSource)
}

// networkOperatorReleaseChannel returns the release-specific channel named for the
// selected release (e.g. "v26.7"), so Automatic upgrades stay on that line. A
// moving channel like "stable" can currently sit on the release yet later advance
// across lines, so matching by current CSV version is not enough: a missing
// release channel is a hard error.
func networkOperatorReleaseChannel(manifest *unstructured.Unstructured, release string) (string, error) {
	wanted := map[string]bool{"v" + release: true, release: true}
	channels, _, _ := unstructured.NestedSlice(manifest.Object, "status", "channels")
	available := make([]string, 0, len(channels))
	for _, entry := range channels {
		ch, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(ch, "name")
		version, _, _ := unstructured.NestedString(ch, "currentCSVDesc", "version")
		if version == "" {
			csv, _, _ := unstructured.NestedString(ch, "currentCSV")
			version = csvVersion(csv)
		}
		available = append(available, fmt.Sprintf("%s (%s)", name, version))
		if wanted[name] {
			return name, nil
		}
	}
	return "", fmt.Errorf("no release-specific OLM channel (e.g. %q) for %s matches selected release %q; available channels: %s; a moving channel such as \"stable\" cannot pin the release line",
		"v"+release, olmNetworkOperatorPkg, release, strings.Join(available, ", "))
}

// csvVersion extracts the version from an OLM CSV name ("<package>.v<version>").
func csvVersion(csvName string) string {
	if i := strings.LastIndex(csvName, ".v"); i >= 0 {
		return csvName[i+2:]
	}
	return ""
}

// EnsureOCPOperatorInstallDefaults reconciles the ocpOperatorInstall block with
// the effective config when management is on (e.g. generate without a prior
// discover): the operator set and namespaces track the config, while saved
// catalog/approval choices are preserved and any gaps are defaulted. No-op
// otherwise.
func EnsureOCPOperatorInstallDefaults(cfg *config.LaunchKitConfig) {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return
	}
	if cfg.OCPOperatorInstall == nil || !cfg.OCPOperatorInstall.Manage {
		return
	}
	operators := effectiveOperatorSubscriptions(cfg)
	fillOperatorDefaults(operators)
	reconcileNetworkOperatorChannel(cfg, operators)
	cfg.OCPOperatorInstall.Operators = operators

	groups := effectiveOperatorGroups(cfg)
	fillOperatorGroupDefaults(groups)
	cfg.OCPOperatorInstall.OperatorGroups = groups
}
