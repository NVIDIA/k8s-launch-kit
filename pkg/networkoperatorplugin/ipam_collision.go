// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"net/netip"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type namedCIDR struct {
	prefix netip.Prefix
	source string
}

func parseNamedCIDR(value, source string) (namedCIDR, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Addr().Is4In6() {
		return namedCIDR{}, fmt.Errorf("%s: invalid CIDR %q", source, value)
	}
	return namedCIDR{prefix.Masked(), source}, nil
}

func ipPoolCIDRs(artifacts *bundle.Bundle) ([]namedCIDR, error) {
	if artifacts == nil {
		return nil, fmt.Errorf("deployment bundle must not be nil")
	}
	var pools []namedCIDR
	for _, doc := range artifacts.Documents() {
		ref := doc.Ref()
		if doc.Role() != bundle.Deployment || ref.Group != "nv-ipam.nvidia.com" || ref.Kind != "IPPool" {
			continue
		}
		source := fmt.Sprintf("%s document %d IPPool %s/%s", doc.Source().File, doc.Source().Document, ref.Namespace, ref.Name)
		subnet, _, err := unstructured.NestedString(doc.ObjectCopy().Object, "spec", "subnet")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		pool, err := parseNamedCIDR(subnet, source)
		if err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

func configuredCIDRs(cfg *config.LaunchKitConfig) ([]namedCIDR, *config.ClusterNetworkConfig, error) {
	c := &config.ClusterNetworkConfig{}
	if cfg != nil && cfg.Cluster != nil {
		c = cfg.Cluster
	}
	if err := config.ValidateClusterNetworkConfig(c); err != nil {
		return nil, nil, err
	}
	var ranges []namedCIDR
	for _, field := range []struct {
		name   string
		values []string
	}{{"podCIDRs", c.PodCIDRs}, {"serviceCIDRs", c.ServiceCIDRs}} {
		for i, value := range field.values {
			r, err := parseNamedCIDR(value, fmt.Sprintf("cluster.%s[%d]", field.name, i))
			if err != nil {
				return nil, nil, err
			}
			ranges = append(ranges, r)
		}
	}
	return ranges, c, nil
}

func checkCIDROverlaps(pools, ranges []namedCIDR) error {
	for _, pool := range pools {
		for _, r := range ranges {
			if pool.prefix.Overlaps(r.prefix) {
				return fmt.Errorf("%s subnet %s overlaps %s (%s)", pool.source, pool.prefix, r.source, r.prefix)
			}
		}
	}
	return nil
}

// CheckConfiguredIPPoolCIDRs checks the actual rendered bundle against explicit
// authoritative ranges before generation replaces output. It makes no live reads.
func CheckConfiguredIPPoolCIDRs(artifacts *bundle.Bundle, cfg *config.LaunchKitConfig) error {
	pools, err := ipPoolCIDRs(artifacts)
	if err != nil {
		return err
	}
	ranges, _, err := configuredCIDRs(cfg)
	if err != nil {
		return err
	}
	return checkCIDROverlaps(pools, ranges)
}

// checkLiveIPPoolCIDRs uses fresh all-cluster observations before any deployment
// effects, including dry-run and externally managed Helm. Observations stay local:
// persisting them as overrides would incorrectly make partial/stale data authoritative.
func checkLiveIPPoolCIDRs(ctx context.Context, c client.Client, artifacts *bundle.Bundle, cfg *config.LaunchKitConfig) error {
	pools, err := ipPoolCIDRs(artifacts)
	if err != nil {
		return err
	}
	ranges, overrides, err := configuredCIDRs(cfg)
	if err != nil {
		return err
	}
	if len(pools) == 0 {
		return nil
	}
	if err := checkCIDROverlaps(pools, ranges); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("kubernetes client is required to inspect cluster addresses")
	}
	if overrides.ServiceCIDRs == nil {
		services, available, err := serviceCIDRs(ctx, c)
		if err != nil {
			return err
		}
		if !available {
			ui.FromContext(ctx).Warning("ServiceCIDR API unavailable (for example Kubernetes 1.32 without MultiCIDRServiceAllocator); service ranges unverified; set cluster.serviceCIDRs")
		}
		ranges = append(ranges, services...)
	}
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return fmt.Errorf("inspect Node pod CIDRs and InternalIPs: %w", err)
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("cannot inspect cluster addresses: Node list is empty")
	}
	// Node pod ranges are empty under OVN-Kubernetes; prefer OpenShift's
	// cluster-wide ranges when the API is served.
	nodePodRanges := overrides.PodCIDRs == nil
	if nodePodRanges {
		pods, err := openShiftPodCIDRs(ctx, c)
		if err != nil {
			return err
		}
		ranges = append(ranges, pods...)
		nodePodRanges = len(pods) == 0
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	for _, node := range nodes.Items {
		if nodePodRanges {
			cidrs := node.Spec.PodCIDRs
			if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
				cidrs = []string{node.Spec.PodCIDR}
			}
			if len(cidrs) == 0 {
				ui.FromContext(ctx).Warning("Node %s: pod range unknown; set cluster.podCIDRs for authoritative CNI ranges", node.Name)
			}
			for _, value := range cidrs {
				r, err := parseNamedCIDR(value, "Node "+node.Name+" pod CIDR")
				if err != nil {
					return err
				}
				ranges = append(ranges, r)
			}
		}
		internalIPs := 0
		for _, address := range node.Status.Addresses {
			if address.Type != corev1.NodeInternalIP {
				continue
			}
			ip, err := netip.ParseAddr(address.Address)
			if err != nil || ip.Zone() != "" {
				return fmt.Errorf("node %s: invalid InternalIP %q", node.Name, address.Address)
			}
			ip = ip.Unmap()
			ranges = append(ranges, namedCIDR{netip.PrefixFrom(ip, ip.BitLen()), "Node " + node.Name + " InternalIP"})
			internalIPs++
		}
		if internalIPs == 0 {
			ui.FromContext(ctx).Warning("Node %s: InternalIP unknown; node address collision check incomplete", node.Name)
		}
	}
	return checkCIDROverlaps(pools, ranges)
}

// openShiftPodCIDRs reads spec.clusterNetwork from the config.openshift.io
// Network "cluster". It returns no ranges when the API or object is absent;
// other read failures and malformed ranges are errors.
func openShiftPodCIDRs(ctx context.Context, c client.Client) ([]namedCIDR, error) {
	network := &unstructured.Unstructured{}
	network.SetAPIVersion("config.openshift.io/v1")
	network.SetKind("Network")
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, network); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read OpenShift Network cluster: %w", err)
	}
	entries, _, err := unstructured.NestedSlice(network.Object, "spec", "clusterNetwork")
	if err != nil {
		return nil, fmt.Errorf("OpenShift Network cluster: invalid spec.clusterNetwork: %w", err)
	}
	var ranges []namedCIDR
	for i, entry := range entries {
		m, _ := entry.(map[string]interface{})
		value, _ := m["cidr"].(string)
		r, err := parseNamedCIDR(value, fmt.Sprintf("OpenShift Network cluster spec.clusterNetwork[%d]", i))
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

// Prefer GA, falling back to beta only on verified API absence. Denied reads,
// transport failures, malformed objects and an empty served API are errors.
func serviceCIDRs(ctx context.Context, c client.Client) ([]namedCIDR, bool, error) {
	for _, version := range []string{"v1", "v1beta1"} {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("networking.k8s.io/" + version)
		list.SetKind("ServiceCIDRList")
		err := c.List(ctx, list)
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("list ServiceCIDRs: %w", err)
		}
		if len(list.Items) == 0 {
			return nil, true, fmt.Errorf("ServiceCIDR API returned no ranges; supply cluster.serviceCIDRs or repair cluster service allocation")
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
		var ranges []namedCIDR
		for _, item := range list.Items {
			values, _, err := unstructured.NestedStringSlice(item.Object, "spec", "cidrs")
			if err != nil || len(values) == 0 {
				return nil, true, fmt.Errorf("ServiceCIDR %s has invalid or empty spec.cidrs", item.GetName())
			}
			for _, value := range values {
				r, err := parseNamedCIDR(value, "ServiceCIDR "+item.GetName())
				if err != nil {
					return nil, true, err
				}
				ranges = append(ranges, r)
			}
		}
		return ranges, true, nil
	}
	return nil, false, nil
}
