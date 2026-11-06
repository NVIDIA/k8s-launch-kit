// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type gatewayTarget struct {
	Gateway  string
	Evidence string
}

// configuredGateway follows exact attachment and pool identities. NV-IPAM CNI
// does not install a route via its gateway unless requested by the CNI config;
// the IPPool remains authoritative even when the pod has only a connected route.
func configuredGateway(ctx context.Context, c client.Client, test PingTest, poolNamespace string) (gatewayTarget, error) {
	target := gatewayTarget{}
	if err := ctx.Err(); err != nil {
		return target, err
	}
	if test.gatewayEndpointErr != nil {
		return target, test.gatewayEndpointErr
	}
	parts := strings.Split(test.SrcNetwork, "/")
	namespace, name := test.SrcNamespace, test.SrcNetwork
	if len(parts) == 2 {
		namespace, name = parts[0], parts[1]
	}
	if len(parts) > 2 || namespace == "" || name == "" {
		return target, fmt.Errorf("missing or malformed secondary NAD identity %q", test.SrcNetwork)
	}
	target.Evidence = "NAD " + namespace + "/" + name
	lookupCtx, cancel := context.WithTimeout(ctx, gatewayConfigTimeout)
	defer cancel()
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"})
	if err := c.Get(lookupCtx, client.ObjectKey{Namespace: namespace, Name: name}, nad); err != nil {
		return target, fmt.Errorf("read gateway configuration for NAD %s/%s: %w", namespace, name, err)
	}
	raw, found, err := unstructured.NestedString(nad.Object, "spec", "config")
	if err != nil || !found || strings.TrimSpace(raw) == "" {
		return target, fmt.Errorf("NAD %s/%s has missing or malformed spec.config", namespace, name)
	}
	poolName, poolType, err := nvIPAMPool(raw)
	if err != nil {
		return target, fmt.Errorf("NAD %s/%s gateway configuration: %w", namespace, name, err)
	}
	if poolName != "" {
		if err := validateGatewayNetworkSelection(test.sourceSelection, test.SrcNamespace, namespace, name, test.SrcIface); err != nil {
			return target, err
		}
	}
	if poolName == "" || poolType == "cidrpool" {
		target.Evidence += ": runtime interface routes (non-IPPool IPAM)"
		return target, nil
	}
	// Pool namespace is the IPAM daemon setting supplied by the Host adapter,
	// independently of the pod/NAD namespace. Never scan unrelated pools.
	if poolNamespace == "" {
		return target, fmt.Errorf("NV-IPAM IPPool gateway lookup requires an explicit IPAM namespace")
	}
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(schema.GroupVersionKind{Group: "nv-ipam.nvidia.com", Version: "v1alpha1", Kind: "IPPool"})
	if err := c.Get(lookupCtx, client.ObjectKey{Namespace: poolNamespace, Name: poolName}, pool); err != nil {
		return target, fmt.Errorf("read NV-IPAM IPPool %s/%s: %w", poolNamespace, poolName, err)
	}
	subnet, found, err := unstructured.NestedString(pool.Object, "spec", "subnet")
	if err != nil || !found {
		return target, fmt.Errorf("IPPool %s/%s has missing or malformed subnet", pool.GetNamespace(), poolName)
	}
	prefix, prefixErr := netip.ParsePrefix(subnet)
	source, sourceErr := netip.ParseAddr(test.SrcIP)
	if prefixErr != nil || sourceErr != nil || !prefix.Contains(source) {
		return target, fmt.Errorf("source %q does not belong to IPPool %s/%s subnet %q", test.SrcIP, pool.GetNamespace(), poolName, subnet)
	}
	gateway, _, err := unstructured.NestedString(pool.Object, "spec", "gateway")
	if err != nil {
		return target, fmt.Errorf("IPPool %s/%s has malformed gateway", pool.GetNamespace(), poolName)
	}
	target.Evidence += fmt.Sprintf("; IPPool %s/%s subnet=%s gateway=%s", pool.GetNamespace(), poolName, subnet, gateway)
	if gateway != "" {
		parsed, err := validateGatewayAddress(gateway, source)
		if err != nil {
			return target, err
		}
		if !parsed.IsLinkLocalUnicast() && !prefix.Contains(parsed) {
			return target, fmt.Errorf("gateway %s is outside IPPool subnet %s", parsed, prefix)
		}
		gateway = parsed.String()
	}
	target.Gateway = gateway
	return target, nil
}

func nvIPAMPool(raw string) (name, poolType string, err error) {
	type plugin struct {
		Type string `json:"type"`
		IPAM *struct {
			Type     string `json:"type"`
			PoolName string `json:"poolName"`
			PoolType string `json:"poolType"`
		} `json:"ipam"`
		Args json.RawMessage `json:"args"`
	}
	var config struct {
		plugin
		Plugins []plugin `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil || strings.TrimSpace(raw) == "null" {
		return "", "", fmt.Errorf("malformed CNI JSON")
	}
	if config.Type == "" && len(config.Plugins) == 0 {
		return "", "", fmt.Errorf("CNI JSON requires a plugin type or a plugins list")
	}
	plugins := config.Plugins
	if config.Type != "" {
		plugins = append([]plugin{config.plugin}, plugins...)
	}
	matches := 0
	for _, p := range plugins {
		if p.Type == "" {
			return "", "", fmt.Errorf("CNI plugin is missing its type")
		}
		if p.IPAM == nil || p.IPAM.Type != "nv-ipam" {
			continue
		}
		matches++
		name, poolType = p.IPAM.PoolName, p.IPAM.PoolType
		if strings.TrimSpace(name) == "" || strings.Contains(name, ",") || strings.TrimSpace(name) != name || p.Args != nil || config.Args != nil {
			return "", "", fmt.Errorf("NV-IPAM requires one explicit poolName without CNI args overrides for gateway lookup")
		}
		if poolType != "" && poolType != "ippool" && poolType != "cidrpool" {
			return "", "", fmt.Errorf("unsupported NV-IPAM poolType %q", poolType)
		}
	}
	if matches > 1 {
		return "", "", fmt.Errorf("multiple NV-IPAM configurations are ambiguous")
	}
	return name, poolType, nil
}
