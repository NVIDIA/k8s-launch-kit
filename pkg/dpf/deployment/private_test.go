// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

var _ = Describe("native private boundaries", func() {
	It("uses the recorded current BMC credential before a different pending spec reference", func() {
		requests := 0
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			user, password, ok := r.BasicAuth()
			if r.Method != http.MethodGet || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || !ok || user != "admin" || password != "recorded-current" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/redfish/v1/Managers":
				_, _ = w.Write([]byte(`{"Members":[]}`))
			case "/redfish/v1/Chassis/BlueField_0":
				_, _ = w.Write([]byte(`{"SerialNumber":"SERIAL-A","AssetTag":"TEST-PSID"}`))
			case "/redfish/v1/Systems/BlueField_0":
				_, _ = w.Write([]byte(`{"AssetTag":"TEST-PSID"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAnyClientCert}
		server.StartTLS()
		defer server.Close()
		certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
		Expect(err).NotTo(HaveOccurred())
		privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
		secret := func(name string, data map[string][]byte) *unstructured.Unstructured {
			o := testObject("v1", "Secret", "dpf-system", name, nil)
			encoded := map[string]any{}
			for key, value := range data {
				encoded[key] = base64.StdEncoding.EncodeToString(value)
			}
			o.Object["data"] = encoded
			return o
		}
		device := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUDevice", "dpf-system", "device-a", map[string]any{"bmcCredentialSecretName": "future"})
		_ = unstructured.SetNestedField(device.Object, "current", "status", "bmcCredentialSecretName")
		trust := testObject("v1", "ConfigMap", "dpf-system", "dpf-ca-trust-bundle", nil)
		trust.Object["data"] = map[string]any{"ca.crt": string(certificate)}
		dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), device, trust, secret("current", map[string][]byte{"password": []byte("recorded-current")}), secret("future", map[string][]byte{"password": []byte("not-adopted-yet")}), secret("dpf-provisioning-redfish-client-secret-bf4", map[string][]byte{"tls.crt": certificate, "tls.key": privateKey}))
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1"}})
		for _, kind := range []string{"Secret", "ConfigMap"} {
			mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: kind}, meta.RESTScopeNamespace)
		}
		mapper.Add(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUDevice"}, meta.RESTScopeNamespace)
		client := &nativeClient{dynamic: dynamic, mapper: mapper}
		Expect(client.VerifyBMC(context.Background(), config.Generation{Namespace: "dpf-system"}, config.DeviceBinding{DPUDevice: "device-a"}, inventory.Device{SerialNumber: "SERIAL-A", BMCEndpoint: server.URL}, "TEST-PSID")).To(Succeed())
		Expect(requests).To(Equal(3))
	})
	It("verifies DPF CA chain and CN-only BMC identity rather than bypassing TLS", func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "192.0.2.10"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		Expect(err).NotTo(HaveOccurred())
		cert, err := x509.ParseCertificate(der)
		Expect(err).NotTo(HaveOccurred())
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		Expect(verifiedBMCTLS(pool, tls.Certificate{}, "192.0.2.10").VerifyConnection(state)).To(Succeed())
		Expect(verifiedBMCTLS(pool, tls.Certificate{}, "192.0.2.11").VerifyConnection(state)).NotTo(Succeed())
		Expect(verifiedBMCTLS(x509.NewCertPool(), tls.Certificate{}, "192.0.2.10").VerifyConnection(state)).NotTo(Succeed())
		Expect(verifiedBMCTLS(pool, tls.Certificate{}, "192.0.2.10").VerifyConnection(tls.ConnectionState{})).NotTo(Succeed())
	})

	It("rejects kubeconfigs with insecure TLS, external execution, or ambient file credentials", func() {
		base := "apiVersion: v1\nkind: Config\ncurrent-context: explicit\nclusters:\n- name: cluster\n  cluster:\n    server: https://192.0.2.1\n%scontexts:\n- name: explicit\n  context: {cluster: cluster, user: user}\nusers:\n- name: user\n  user:\n%s"
		for _, input := range []string{fmt.Sprintf(base, "    insecure-skip-tls-verify: true\n", "    token: unused\n"), fmt.Sprintf(base, "", "    exec: {apiVersion: client.authentication.k8s.io/v1, command: forbidden}\n"), fmt.Sprintf(base, "", "    tokenFile: /ambient/token\n")} {
			_, err := clientFromBytes([]byte(input), "explicit")
			Expect(err).To(HaveOccurred())
		}
	})

	DescribeTable("hashes actual registry-consumed bytes and refuses drift", func(tamper, drift, wrongLeader bool) {
		iso, fw := []byte("exact ISO fixture bytes"), []byte("exact PLDM fixture bytes")
		digest := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
		isoPath := "/bfb/dpf-system-software-os.iso"
		fwPath := "/bfb/components/dpf-system-software-TEST-PSID-fw.fwpkg"
		software := testObject("provisioning.dpu.nvidia.com/v1alpha1", "BlueFieldSoftware", "dpf-system", "software", map[string]any{"osIso": "https://example.invalid/os.iso"})
		setReady(software)
		_ = unstructured.SetNestedMap(software.Object, map[string]any{"osIso": isoPath, "pldmFwBundle": map[string]any{"TEST-PSID": fwPath}}, "status", "downloadedComponents")
		service := testObject("v1", "Service", "dpf-system", "bfb-registry", map[string]any{"ports": []any{map[string]any{"port": int64(8443)}}})
		cacheSpec := map[string]any{"nodeName": "management-a", "containers": []any{map[string]any{"name": "controller", "volumeMounts": []any{map[string]any{"name": "cache", "mountPath": "/bfb"}}}}, "volumes": []any{map[string]any{"name": "cache", "hostPath": map[string]any{"path": "/var/lib/nvidia/dpf/bfb"}}}}
		leader := testObject("v1", "Pod", "dpf-system", "provisioning-leader", cacheSpec)
		setReady(leader)
		_ = unstructured.SetNestedField(leader.Object, "Running", "status", "phase")
		registry := testObject("v1", "Pod", "dpf-system", "bfb-registry", cacheSpec)
		registry.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: leader.GetName(), UID: leader.GetUID()}})
		setReady(registry)
		_ = unstructured.SetNestedField(registry.Object, "Running", "status", "phase")
		lease := testObject("coordination.k8s.io/v1", "Lease", "dpf-system", "provisioning.dpu.nvidia.com", map[string]any{"holderIdentity": "provisioning-leader_unique-election-id", "renewTime": time.Now().UTC().Format(time.RFC3339Nano), "leaseDurationSeconds": int64(60)})
		if wrongLeader {
			_ = unstructured.SetNestedField(lease.Object, "another-leader_unique-election-id", "spec", "holderIdentity")
		}
		dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), software, service, leader, registry, lease)
		if drift {
			dynamic.PrependReactor("get", "bluefieldsoftwares", func(action clienttesting.Action) (bool, runtime.Object, error) {
				changed := software.DeepCopy()
				changed.SetGeneration(2)
				return true, changed, nil
			})
		}
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1"}, {Group: "coordination.k8s.io", Version: "v1"}})
		mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "BlueFieldSoftware"}, meta.RESTScopeNamespace)
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, isoPath):
				if tamper {
					_, _ = w.Write([]byte("tampered"))
				} else {
					_, _ = w.Write(iso)
				}
			case strings.HasSuffix(r.URL.Path, fwPath):
				_, _ = w.Write(fw)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()
		core, err := typedcore.NewForConfig(&rest.Config{Host: server.URL})
		Expect(err).NotTo(HaveOccurred())
		client := &nativeClient{dynamic: dynamic, mapper: mapper, core: core}
		g := config.Generation{Namespace: "dpf-system", Provisioning: config.Provisioning{Software: config.Software{OSISO: config.Artifact{SHA256: digest(iso)}, PLDM: map[string]config.Artifact{"TEST-PSID": {SHA256: digest(fw)}}}}}
		err = (CachedArtifacts{}).Cached(context.Background(), client, g, software)
		if wrongLeader {
			Expect(err).To(MatchError(ContainSubstring("Lease holder")))
			Expect(requests).To(BeZero())
		} else if tamper {
			Expect(err).To(MatchError(ContainSubstring("SHA256 differs")))
		} else if drift {
			Expect(err).To(MatchError(ContainSubstring("changed while verifying")))
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(requests).To(Equal(2))
		}
	}, Entry("matches exact cached ISO and firmware", false, false, false), Entry("rejects cached tampering", true, false, false), Entry("rejects software generation drift", false, true, false), Entry("rejects registry storage from an inactive provisioning leader", false, false, true))
})
