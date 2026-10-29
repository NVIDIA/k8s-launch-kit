// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type ManagementClient struct {
	Client    kubernetes.Interface
	REST      *rest.Config
	Namespace string
}

func ManagementAccess(r config.Request) (ManagementClient, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: r.Kubeconfig}
	content, err := config.ReadReference(r.Kubeconfig, 16<<20)
	if err != nil {
		return ManagementClient{}, fmt.Errorf("invalid bounded management kubeconfig")
	}
	raw, err := clientcmd.Load(content)
	if err != nil {
		return ManagementClient{}, fmt.Errorf("cannot load explicit management kubeconfig")
	}
	for _, a := range raw.AuthInfos {
		a.LocationOfOrigin = r.Kubeconfig
	}
	for _, c := range raw.Clusters {
		c.LocationOfOrigin = r.Kubeconfig
	}
	if err := clientcmd.ResolveLocalPaths(raw); err != nil {
		return ManagementClient{}, fmt.Errorf("cannot resolve kubeconfig file references")
	}
	selected := r.Context
	if selected == "" {
		selected = raw.CurrentContext
		if raw.Contexts[selected] == nil && len(raw.Contexts) == 1 {
			for k := range raw.Contexts {
				selected = k
			}
		}
	}
	contextConfig := raw.Contexts[selected]
	if contextConfig == nil {
		return ManagementClient{}, fmt.Errorf("management context selection is required")
	}
	auth := raw.AuthInfos[contextConfig.AuthInfo]
	cluster := raw.Clusters[contextConfig.Cluster]
	if auth == nil || cluster == nil {
		return ManagementClient{}, fmt.Errorf("management context has missing auth/cluster")
	}
	if auth.Exec != nil || auth.AuthProvider != nil || auth.Username != "" || auth.Password != "" {
		return ManagementClient{}, fmt.Errorf("management kubeconfig requires certificate/key or bearer token; exec/auth-provider/basic authentication unsupported")
	}
	if auth.Token != "" && auth.TokenFile != "" || len(auth.ClientCertificateData) > 0 && auth.ClientCertificate != "" || len(auth.ClientKeyData) > 0 && auth.ClientKey != "" || len(cluster.CertificateAuthorityData) > 0 && cluster.CertificateAuthority != "" {
		return ManagementClient{}, fmt.Errorf("management auth/trust references must use exactly one file or embedded value")
	}
	hasToken := auth.Token != "" || auth.TokenFile != ""
	hasCertificate := (auth.ClientCertificate != "" || len(auth.ClientCertificateData) > 0) && (auth.ClientKey != "" || len(auth.ClientKeyData) > 0)
	if !hasToken && !hasCertificate {
		return ManagementClient{}, fmt.Errorf("management certificate/key or bearer token is required")
	}
	u, err := url.Parse(cluster.Server)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cluster.InsecureSkipTLSVerify {
		return ManagementClient{}, fmt.Errorf("management server requires verified HTTPS without userinfo/query/fragment")
	}
	for _, reference := range []struct {
		file *string
		data *[]byte
	}{{&auth.ClientCertificate, &auth.ClientCertificateData}, {&auth.ClientKey, &auth.ClientKeyData}, {&cluster.CertificateAuthority, &cluster.CertificateAuthorityData}} {
		if *reference.file != "" && len(*reference.data) == 0 {
			b, e := config.ReadReference(*reference.file, 1<<20)
			if e != nil {
				return ManagementClient{}, fmt.Errorf("invalid bounded management auth/trust reference")
			}
			*reference.data = b
			*reference.file = ""
		}
	}
	if auth.TokenFile != "" && auth.Token == "" {
		b, e := config.ReadReference(auth.TokenFile, 64<<10)
		if e != nil {
			return ManagementClient{}, fmt.Errorf("invalid bounded token reference")
		}
		auth.Token = strings.TrimSpace(string(b))
		if auth.Token == "" {
			return ManagementClient{}, fmt.Errorf("empty management token reference")
		}
		auth.TokenFile = ""
	}
	loader := clientcmd.NewNonInteractiveClientConfig(*raw, selected, &clientcmd.ConfigOverrides{}, rules)
	rc, err := loader.ClientConfig()
	if err != nil {
		return ManagementClient{}, fmt.Errorf("cannot resolve management certificate/token configuration")
	}
	rc.Timeout = r.RequestTimeout
	rc.CAFile = ""
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return ManagementClient{}, fmt.Errorf("cannot construct management client")
	}
	ns := r.Namespace
	if ns == "" {
		ns = contextConfig.Namespace
	}
	if ns == "" {
		ns = "default"
	}
	return ManagementClient{Client: client, REST: rc, Namespace: ns}, nil
}
func (m ManagementClient) Observe(ctx context.Context) (*inventory.Management, error) {
	ns, err := m.Client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("management cluster identity read failed")
	}
	if ns.UID == "" {
		return nil, fmt.Errorf("management identity UID is empty")
	}
	// REST request honors caller cancellation, unlike ServerVersion's context-free API.
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	data, err := m.Client.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil || json.Unmarshal(data, &version) != nil || version.GitVersion == "" {
		return nil, fmt.Errorf("management version read failed")
	}
	result := &inventory.Management{APIEndpoint: m.REST.Host, SystemNamespaceUID: string(ns.UID), KubernetesVersion: version.GitVersion, Namespace: m.Namespace, TrustDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(m.REST.CAData)), Nodes: []inventory.Node{}, EvidenceRefs: []string{"management-identity", "management-nodes"}}
	if len(m.REST.CAData) == 0 {
		result.TrustDigest = "system-roots"
	}
	if m.REST.ServerName != "" {
		result.TrustDigest = inventory.Digest("management-trust-v1", []string{result.TrustDigest, m.REST.ServerName})
	}
	cursor := ""
	for {
		nodes, err := m.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 64, Continue: cursor})
		if err != nil {
			return nil, fmt.Errorf("management node inspection failed")
		}
		for _, n := range nodes.Items {
			node := inventory.Node{Name: n.Name, UID: string(n.UID), Architecture: n.Status.NodeInfo.Architecture, OS: n.Status.NodeInfo.OperatingSystem, Unschedulable: n.Spec.Unschedulable, Addresses: []string{}}
			_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
			_, master := n.Labels["node-role.kubernetes.io/master"]
			node.ControlPlane = cp || master
			for _, condition := range n.Status.Conditions {
				if condition.Type == corev1.NodeReady {
					node.Ready = condition.Status == corev1.ConditionTrue
				}
			}
			for _, address := range n.Status.Addresses {
				node.Addresses = append(node.Addresses, address.Address)
			}
			slices.Sort(node.Addresses)
			result.Nodes = append(result.Nodes, node)
		}
		if len(result.Nodes) > 64 {
			return nil, fmt.Errorf("management node scope exceeds 64 nodes")
		}
		cursor = nodes.Continue
		if cursor == "" {
			break
		}
	}
	result.Observations = map[string]inventory.Observation{}
	storage, storageErr := m.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{Limit: 256})
	storageFact := inventory.Observation{State: "unknown", Values: []string{}, ReasonCode: "INSPECTION_UNAVAILABLE"}
	if storageErr == nil && storage.Continue == "" {
		storageFact.State = "known"
		storageFact.ReasonCode = ""
		for _, s := range storage.Items {
			storageFact.Values = append(storageFact.Values, s.Name+"/"+s.Provisioner)
		}
		slices.Sort(storageFact.Values)
	}
	result.Observations["storageClasses"] = storageFact
	groupsFact := inventory.Observation{State: "unknown", Values: []string{}, ReasonCode: "INSPECTION_UNAVAILABLE"}
	data, groupErr := m.Client.Discovery().RESTClient().Get().AbsPath("/apis").Do(ctx).Raw()
	var groups metav1.APIGroupList
	if groupErr == nil && json.Unmarshal(data, &groups) == nil {
		groupsFact.State = "known"
		groupsFact.ReasonCode = ""
		for _, g := range groups.Groups {
			if strings.HasSuffix(g.Name, "dpu.nvidia.com") {
				groupsFact.Values = append(groupsFact.Values, g.Name)
			}
		}
		slices.Sort(groupsFact.Values)
	}
	result.Observations["advertisedDPFAPIGroups"] = groupsFact
	slices.SortFunc(result.Nodes, func(a, b inventory.Node) int { return strings.Compare(a.UID, b.UID) })
	return result, nil
}

// NewEvidence binds API reads to the current collection rather than old success flags.
func NewEvidence(at time.Time) map[string]inventory.Evidence {
	return map[string]inventory.Evidence{"management-identity": {Kind: "kubernetes-api", InputRef: "/management", ResourceURI: "/api/v1/namespaces/kube-system", Property: "metadata.uid", ObservedAt: at, Vantage: "executor", Authenticated: true}, "management-nodes": {Kind: "kubernetes-api", InputRef: "/management", ResourceURI: "/api/v1/nodes", ObservedAt: at, Vantage: "executor", Authenticated: true}}
}
