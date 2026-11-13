// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (c *nativeClient) VerifyBMC(ctx context.Context, g config.Generation, binding config.DeviceBinding, device inventory.Device, psid string) error {
	enrolled, err := c.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUDevice", g.Namespace, binding.DPUDevice))
	if err != nil {
		return fmt.Errorf("cannot read bound BMC credential reference")
	}
	// Status records the credential currently in use; spec can already point to
	// its replacement while the operator is still performing rotation.
	secret := text(enrolled, "status", "bmcCredentialSecretName")
	if secret == "" {
		secret = text(enrolled, "spec", "bmcCredentialSecretName")
	}
	if secret == "" {
		secret = "bmc-shared-password"
	}
	password, err := c.secretData(ctx, g.Namespace, secret, "password")
	if err != nil {
		return err
	}
	certPEM, err := c.secretData(ctx, g.Namespace, "dpf-provisioning-redfish-client-secret-bf4", "tls.crt")
	if err != nil {
		return err
	}
	keyPEM, err := c.secretData(ctx, g.Namespace, "dpf-provisioning-redfish-client-secret-bf4", "tls.key")
	if err != nil {
		return err
	}
	trust, err := c.Get(ctx, ref(bundle.Management, "", "ConfigMap", g.Namespace, "dpf-ca-trust-bundle"))
	if err != nil {
		return fmt.Errorf("DPF CA trust bundle is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(text(trust, "data", "ca.crt"))) {
		return fmt.Errorf("DPF CA trust bundle is invalid")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("DPF BF4 client keypair is invalid")
	}
	endpoint, err := url.Parse(device.BMCEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Host == "" {
		return fmt.Errorf("BMC endpoint is not a bound HTTPS origin")
	}
	tlsConfig := verifiedBMCTLS(pool, certificate, endpoint.Hostname())
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 15 * time.Second, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(uri string) (map[string]any, error) {
		if !strings.HasPrefix(uri, "/redfish/v1/") || path.Clean(uri) != strings.TrimSuffix(uri, "/") || strings.ContainsAny(uri, "?%\\") {
			return nil, fmt.Errorf("unsupported BMC identity path")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.Scheme+"://"+endpoint.Host+uri, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot create bounded identity request")
		}
		request.SetBasicAuth("admin", string(password))
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("verified BMC GET failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("BMC identity GET returned HTTP %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		if err != nil || len(data) > 4<<20 {
			return nil, fmt.Errorf("BMC identity response is unavailable or oversized")
		}
		var object map[string]any
		if json.Unmarshal(data, &object) != nil {
			return nil, fmt.Errorf("BMC identity response is malformed")
		}
		return object, nil
	}
	if _, err = get("/redfish/v1/Managers"); err != nil {
		return err
	}
	chassis, err := get("/redfish/v1/Chassis/BlueField_0")
	if err != nil {
		return err
	}
	if chassis["SerialNumber"] != device.SerialNumber {
		return fmt.Errorf("live BF4 serial differs from selected identity")
	}
	system, err := get("/redfish/v1/Systems/BlueField_0")
	if err != nil {
		return err
	}
	livePSID, _ := system["AssetTag"].(string)
	if livePSID == "" || livePSID == "N/A" {
		livePSID, _ = chassis["AssetTag"].(string)
	}
	if !strings.EqualFold(livePSID, psid) {
		return fmt.Errorf("live BF4 PSID differs from selected firmware scope")
	}
	return nil
}

func verifiedBMCTLS(roots *x509.CertPool, certificate tls.Certificate, host string) *tls.Config {
	// BF4 firmware can issue CN-only IP certificates. The custom verification
	// below verifies the complete CA chain and identity; it is never a TLS bypass.
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || roots == nil || host == "" {
			return fmt.Errorf("BMC peer identity is missing")
		}
		leaf := state.PeerCertificates[0]
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("BMC certificate does not verify against DPF CA")
		}
		if leaf.VerifyHostname(host) == nil {
			return nil
		}
		if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 && leaf.Subject.CommonName == host {
			return nil
		}
		return fmt.Errorf("BMC certificate identity differs from selected endpoint")
	}}
}

// CachedArtifacts hashes the actual DPF registry payload through an authenticated
// Kubernetes Pod proxy, then re-observes the same software identity/status.
// DPF lacks native digest enforcement: cache replacement/deletion during active
// provisioning remains an explicit external invariant.
type CachedArtifacts struct{}

func (CachedArtifacts) Cached(ctx context.Context, client Client, g config.Generation, software *unstructured.Unstructured) error {
	native, ok := client.(*nativeClient)
	if !ok {
		return fmt.Errorf("native cached-byte verification requires the bound native client")
	}
	ready, _, _ := Readiness(software)
	if !ready || software.GetUID() == "" {
		return fmt.Errorf("software snapshot is not Ready and identified")
	}
	components, _, _ := unstructured.NestedMap(software.Object, "status", "downloadedComponents")
	paths := map[string]string{}
	iso, _ := components["osIso"].(string)
	paths[iso] = g.Provisioning.Software.OSISO.SHA256
	pldm, _ := components["pldmFwBundle"].(map[string]any)
	for psid, artifact := range g.Provisioning.Software.PLDM {
		var filename string
		for candidate, value := range pldm {
			if strings.EqualFold(candidate, psid) {
				filename, _ = value.(string)
			}
		}
		paths[filename] = artifact.SHA256
	}
	if g.Provisioning.Software.NICFirmware != nil {
		filename, _ := components["nicFw"].(string)
		paths[filename] = g.Provisioning.Software.NICFirmware.SHA256
	}
	service, err := native.Get(ctx, ref(bundle.Management, "", "Service", g.Namespace, "bfb-registry"))
	if err != nil || service.GetUID() == "" {
		return fmt.Errorf("bound DPF bfb-registry Service is unavailable")
	}
	registry, leader, err := native.registryStorage(ctx, g.Namespace)
	if err != nil {
		return err
	}
	prefix := software.GetNamespace() + "-" + software.GetName() + "-"
	for filename, expected := range paths {
		if filename == "" || path.Clean(filename) != filename || !strings.HasPrefix(filename, "/bfb/") || strings.ContainsAny(filename, "?%\\") || !strings.HasPrefix(path.Base(filename), prefix) {
			return fmt.Errorf("software status contains an unsupported cache path")
		}
		if path.Dir(filename) != "/bfb" && path.Dir(filename) != "/bfb/components" {
			return fmt.Errorf("software cache path escapes qualified roots")
		}
		stream, err := native.core.RESTClient().Get().Namespace(g.Namespace).Resource("pods").Name("https:bfb-registry:8443").SubResource("proxy").Suffix(strings.TrimPrefix(filename, "/")).Stream(ctx)
		if err != nil {
			return fmt.Errorf("cannot stream bound cached payload through Kubernetes service proxy")
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, io.LimitReader(stream, (32<<30)+1))
		closeErr := stream.Close()
		if copyErr != nil || closeErr != nil || size == 0 || size > 32<<30 {
			return fmt.Errorf("cached artifact stream failed or exceeded bound")
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("cached artifact SHA256 differs from locked software")
		}
	}
	after, err := native.Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "BlueFieldSoftware", software.GetNamespace(), software.GetName()))
	if err != nil {
		return fmt.Errorf("cannot re-observe software after cache verification")
	}
	afterComponents, _, _ := unstructured.NestedMap(after.Object, "status", "downloadedComponents")
	ready, _, _ = Readiness(after)
	if !ready || after.GetUID() != software.GetUID() || after.GetGeneration() != software.GetGeneration() || !reflect.DeepEqual(components, afterComponents) {
		return fmt.Errorf("software identity/generation/cache paths changed while verifying consumed bytes")
	}
	afterService, err := native.Get(ctx, ref(bundle.Management, "", "Service", g.Namespace, "bfb-registry"))
	if err != nil || afterService.GetUID() != service.GetUID() || afterService.GetResourceVersion() != service.GetResourceVersion() {
		return fmt.Errorf("registry Service changed during cache verification")
	}
	afterRegistry, afterLeader, err := native.registryStorage(ctx, g.Namespace)
	if err != nil || afterRegistry.GetUID() != registry.GetUID() || afterRegistry.GetResourceVersion() != registry.GetResourceVersion() || afterLeader.GetUID() != leader.GetUID() {
		return fmt.Errorf("registry/leader storage binding changed while hashing cached files")
	}
	return nil
}

func (c *nativeClient) registryStorage(ctx context.Context, namespace string) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	registry, err := c.Get(ctx, ref(bundle.Management, "", "Pod", namespace, "bfb-registry"))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot inspect bound registry Pod")
	}
	ready, _, _ := Readiness(registry)
	if !ready {
		return nil, nil, fmt.Errorf("registry Pod is not Ready")
	}
	var ownerName string
	var ownerUID string
	for _, owner := range registry.GetOwnerReferences() {
		if owner.Kind == "Pod" {
			if ownerName != "" {
				return nil, nil, fmt.Errorf("registry has ambiguous provisioning leader ownership")
			}
			ownerName = owner.Name
			ownerUID = string(owner.UID)
		}
	}
	if ownerName == "" {
		return nil, nil, fmt.Errorf("registry lacks the qualified provisioning leader Pod owner")
	}
	leader, err := c.Get(ctx, ref(bundle.Management, "", "Pod", namespace, ownerName))
	if err != nil || string(leader.GetUID()) != ownerUID {
		return nil, nil, fmt.Errorf("registry provisioning leader identity is unresolved")
	}
	lease, err := c.Get(ctx, ref(bundle.Management, "coordination.k8s.io", "Lease", namespace, "provisioning.dpu.nvidia.com"))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read qualified provisioning leader Lease")
	}
	holder := text(lease, "spec", "holderIdentity")
	hostname := text(leader, "spec", "hostname")
	if hostname == "" {
		hostname = ownerName
	}
	if !strings.HasPrefix(holder, hostname+"_") {
		return nil, nil, fmt.Errorf("registry owner is not the current provisioning Lease holder")
	}
	renewed, err := time.Parse(time.RFC3339Nano, text(lease, "spec", "renewTime"))
	duration := number(lease, "spec", "leaseDurationSeconds")
	if err != nil || duration <= 0 || time.Now().After(renewed.Add(time.Duration(duration)*time.Second)) {
		return nil, nil, fmt.Errorf("provisioning leader Lease is expired or unobserved")
	}
	registryVolume, ok := bfbStorage(registry)
	if !ok {
		return nil, nil, fmt.Errorf("registry /bfb storage mount is unsupported")
	}
	leaderVolume, ok := bfbStorage(leader)
	if !ok {
		return nil, nil, fmt.Errorf("provisioning leader /bfb storage mount is unsupported")
	}
	if !reflect.DeepEqual(registryVolume, leaderVolume) {
		return nil, nil, fmt.Errorf("registry and provisioning leader do not consume the same cache storage")
	}
	if _, hostPath := registryVolume["hostPath"]; hostPath && (text(registry, "spec", "nodeName") == "" || text(registry, "spec", "nodeName") != text(leader, "spec", "nodeName")) {
		return nil, nil, fmt.Errorf("hostPath registry and provisioning cache are on different nodes")
	}
	return registry, leader, nil
}

func bfbStorage(pod *unstructured.Unstructured) (map[string]any, bool) {
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	volumeName := ""
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			continue
		}
		mounts, _ := container["volumeMounts"].([]any)
		for _, item := range mounts {
			mount, ok := item.(map[string]any)
			if ok && mount["mountPath"] == "/bfb" {
				if sub, _ := mount["subPath"].(string); sub != "" {
					return nil, false
				}
				name, _ := mount["name"].(string)
				if volumeName != "" && volumeName != name {
					return nil, false
				}
				volumeName = name
			}
		}
	}
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, item := range volumes {
		volume, ok := item.(map[string]any)
		if !ok || volume["name"] != volumeName {
			continue
		}
		if host, ok := volume["hostPath"].(map[string]any); ok && host["path"] == "/var/lib/nvidia/dpf/bfb" {
			return map[string]any{"hostPath": host["path"]}, true
		}
		if pvc, ok := volume["persistentVolumeClaim"].(map[string]any); ok {
			if claim, ok := pvc["claimName"].(string); ok && claim != "" {
				return map[string]any{"persistentVolumeClaim": claim}, true
			}
		}
	}
	return nil, false
}
