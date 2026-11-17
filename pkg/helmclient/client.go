// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

// Package helmclient builds Helm SDK action configurations from explicit
// Kubernetes connection, namespace, and storage-driver inputs.
package helmclient

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memcacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// StorageDriver is Helm 3's default release-storage driver.
const StorageDriver = "secrets"

// NewActionConfig builds an action.Configuration using exactly the supplied
// namespace and Helm storage driver. Defaults belong to the caller's policy.
func NewActionConfig(restConfig *rest.Config, namespace, helmDriver string) (*action.Configuration, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("nil rest config")
	}
	if namespace == "" {
		return nil, fmt.Errorf("helm namespace is required")
	}
	if helmDriver == "" {
		return nil, fmt.Errorf("helm storage driver is required")
	}
	getter := &restClientGetter{restConfig: restConfig, namespace: namespace}
	cfg := new(action.Configuration)
	if err := cfg.Init(getter, namespace, helmDriver, debugNoop); err != nil {
		return nil, err
	}
	return cfg, nil
}

func debugNoop(format string, v ...interface{}) {
	_ = format
	_ = v
}

type restClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

func (r *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(r.restConfig), nil
}

func (r *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := r.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cfg.Burst = 100
	d, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memcacheddiscovery.NewMemCacheClient(d), nil
}

func (r *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(dc)
	return restmapper.NewShortcutExpander(mapper, dc, func(string) {}), nil
}

func (r *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: r.namespace}}
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), overrides)
}
