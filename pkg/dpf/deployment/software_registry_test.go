// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const softwareTestNamespace = "dpf-operator-system"

// Exact nginx.conf data from DPF source fc4974ed3bc026ae27634673c2fdf818a2972235.
const softwarePinnedNginxConfig = "error_log  /dev/stderr notice;\n\nworker_processes  auto;\n\nevents {\n    worker_connections  1024;\n}\n\npid /nginx/nginx.pid;\n\nhttp {\n    include       /usr/local/nginx/conf/mime.types;\n    default_type  application/octet-stream;\n\n    # Set all temp paths to use /nginx directory\n    client_body_temp_path /nginx/client_body;\n    proxy_temp_path /nginx/proxy;\n    fastcgi_temp_path /nginx/fastcgi;\n    uwsgi_temp_path /nginx/uwsgi;\n    scgi_temp_path /nginx/scgi;\n\n    log_format  main  '$remote_addr - $remote_user [$time_local] \"$request\" $status';\n    access_log  /dev/stdout  main;\n\n    sendfile        on;\n    keepalive_timeout  65;\n\n    server {\n        listen       ${NGINX_HTTPS_PORT} ssl;\n        server_name  _;\n\n        ssl_certificate     /etc/bfb-registry/tls/tls.crt;\n        ssl_certificate_key /etc/bfb-registry/tls/tls.key;\n        ssl_protocols       TLSv1.2 TLSv1.3;\n        # TLS 1.2 fallback ciphers: high-strength only, no anonymous (unauthenticated) suites, no MD5. TLS 1.3 suites are negotiated separately and safe by default.\n        ssl_ciphers         HIGH:!aNULL:!MD5;\n\n        location / {\n            root   html;\n            index  index.html index.htm;\n        }\n\n        location /bfb {\n            root /;\n            concat on;\n            concat_max_files 20;\n            concat_types application/octet-stream;\n            concat_types *;\n            concat_unique off;\n        }\n\n        # APT package repository (.deb)\n        location /deb {\n            root /;\n            autoindex on;\n            types {\n                text/plain                          Packages;\n                application/gzip                    gz;\n                application/vnd.debian.binary-package deb;\n            }\n        }\n\n        # YUM/DNF package repository (.rpm)\n        location /rpm {\n            root /;\n            autoindex on;\n            types {\n                application/x-rpm                   rpm;\n                application/xml                     xml;\n                application/gzip                    gz;\n            }\n        }\n    }\n}"

const softwarePinnedNginxCommand = "envsubst '${NGINX_HTTPS_PORT}' < /nginx/nginx.conf.template > /nginx/nginx.conf && /usr/local/nginx/sbin/nginx -c /nginx/nginx.conf -g \"daemon off;\""

type softwareRegistryAPI struct {
	mu             sync.Mutex
	registry       *corev1.Pod
	manager        *corev1.Pod
	lease          *coordinationv1.Lease
	config         *corev1.ConfigMap
	registryConfig *corev1.ConfigMap
	claim          *corev1.PersistentVolumeClaim
}

func newSoftwareRegistryAPI(t *testing.T) (*softwareRegistryAPI, *APIClient) {
	t.Helper()
	caPEM := softwareTestCAPEM(t)
	state := &softwareRegistryAPI{
		registry:       softwareRegistryPodFixture(),
		manager:        softwareManagerPodFixture(),
		lease:          softwareLeaseFixture(),
		config:         &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: softwareCABundleName, Namespace: softwareTestNamespace, UID: "ca-uid", ResourceVersion: "1"}, Data: map[string]string{softwareCABundleKey: string(caPEM)}},
		registryConfig: &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: softwareRegistryConfigName, Namespace: softwareTestNamespace, UID: "nginx-config-uid", ResourceVersion: "1"}, Data: map[string]string{softwareRegistryConfigKey: softwarePinnedNginxConfig}},
		claim:          &corev1.PersistentVolumeClaim{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: metav1.ObjectMeta{Name: "bfb-pvc", Namespace: softwareTestNamespace, UID: "claim-uid", ResourceVersion: "1"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-bfb"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	}
	api := newAPIFixture(t, state.serve)
	client, err := NewAPIClient(context.Background(), api.request, api.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	return state, client
}

func (f *softwareRegistryAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var object any
	switch r.URL.Path {
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/pods/" + softwareRegistryPodName:
		object = f.registry
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/pods/manager-0":
		object = f.manager
	case "/prefix/apis/coordination.k8s.io/v1/namespaces/" + softwareTestNamespace + "/leases/" + softwareLeaderLeaseName:
		object = f.lease
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/configmaps/" + softwareCABundleName:
		object = f.config
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/configmaps/" + softwareRegistryConfigName:
		object = f.registryConfig
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/configmaps/custom-ca":
		object = f.config.DeepCopy()
		object.(*corev1.ConfigMap).Name = "custom-ca"
	case "/prefix/api/v1/namespaces/" + softwareTestNamespace + "/persistentvolumeclaims/bfb-pvc":
		object = f.claim
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
		return
	}
	if object == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
		return
	}
	_ = json.NewEncoder(w).Encode(object)
}

func softwareRegistryPodFixture() *corev1.Pod {
	controller := true
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: softwareRegistryPodName, Namespace: softwareTestNamespace, UID: "registry-uid", ResourceVersion: "1",
			Labels:          map[string]string{"app.kubernetes.io/part-of": "bfb-registry", "dpu.nvidia.com/component": "bfb-registry"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "manager-0", UID: "manager-uid", Controller: &controller}}},
		Spec: corev1.PodSpec{NodeName: "node-a", Volumes: []corev1.Volume{{Name: "bfb", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "bfb-pvc"}}}, {Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: softwareRegistryConfigName}}}}},
			Containers: []corev1.Container{{Name: "bfb-registry", Image: "registry.example/bfb@sha256:registry", Command: []string{"/bin/sh", "-c", softwarePinnedNginxCommand}, Env: []corev1.EnvVar{{Name: "NGINX_HTTPS_PORT", Value: "8443"}}, Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: softwareHTTPSPort, Protocol: corev1.ProtocolTCP}}, VolumeMounts: []corev1.VolumeMount{{Name: "bfb", MountPath: softwareBFBMountPath}, {Name: "config", MountPath: "/nginx/nginx.conf.template", SubPath: softwareRegistryConfigKey, ReadOnly: true}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "bfb-registry", Ready: true, Image: "registry.example/bfb@sha256:registry", ImageID: "containerd://sha256:registry"}}},
	}
}

func softwareManagerPodFixture() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "manager-0", Namespace: softwareTestNamespace, UID: "manager-uid", ResourceVersion: "1",
			Labels: map[string]string{"app.kubernetes.io/part-of": "dpf-provisioning-controller-manager", "dpu.nvidia.com/component": "dpf-provisioning-controller-manager"}},
		Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "manager", Image: "registry.example/provisioning@sha256:manager", Command: []string{"/provisioning"}, Args: []string{"--leader-elect", "--ca-trust-bundle-configmap-name=" + softwareCABundleName}, Env: []corev1.EnvVar{{Name: "BFB_REGISTRY_IMAGE", Value: "registry.example/bfb@sha256:registry"}}, VolumeMounts: []corev1.VolumeMount{{Name: "bfb-volume", MountPath: softwareBFBMountPath}}}}, Volumes: []corev1.Volume{{Name: "bfb-volume", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "bfb-pvc"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Ready: true, Image: "registry.example/provisioning@sha256:manager", ImageID: "containerd://sha256:manager"}}},
	}
}

func softwareLeaseFixture() *coordinationv1.Lease {
	duration, transitions := int32(15), int32(2)
	holder := "manager-0_12345678-1234-1234-1234-123456789abc"
	renew := metav1.NewMicroTime(time.Now())
	return &coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: softwareLeaderLeaseName, Namespace: softwareTestNamespace, UID: "lease-uid", ResourceVersion: "1"}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, RenewTime: &renew, LeaseTransitions: &transitions}}
}

func softwareTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "DPF test root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestObserveRegistrySnapshotBindsLeaderStorageAndPublicCA(t *testing.T) {
	state, client := newSoftwareRegistryAPI(t)
	state.registry.Spec.Containers[0].Env = append(state.registry.Spec.Containers[0].Env, corev1.EnvVar{Name: "UNRELATED_SETTING", Value: "allowed"})
	got, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if got.caName != softwareCABundleName || got.caUID != "ca-uid" || !bytes.Equal(got.caPEM, []byte(state.config.Data[softwareCABundleKey])) || got.registry.uid != "registry-uid" || got.manager.uid != "manager-uid" || got.lease.holder == "" || got.storage.kind != "pvc" || got.storage.claimUID != "claim-uid" || got.storage.volumeName != "pv-bfb" {
		t.Fatalf("snapshot omitted a required binding: %#v", got)
	}
	if !got.sameBinding(got) {
		t.Fatal("snapshot does not compare equal to itself")
	}
	// A routine renewal advances resourceVersion/time but keeps the same leader.
	state.mu.Lock()
	state.lease.ResourceVersion = "2"
	state.lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now()))
	state.mu.Unlock()
	renewed, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil || !got.sameBinding(renewed) {
		t.Fatalf("ordinary Lease renewal changed the binding: %v", err)
	}
}

func TestObserveRegistrySnapshotRejectsUnsafeBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*softwareRegistryAPI)
	}{
		{"expired Lease", func(f *softwareRegistryAPI) {
			f.lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(time.Now().Add(-time.Minute)))
		}},
		{"different Lease holder", func(f *softwareRegistryAPI) {
			*f.lease.Spec.HolderIdentity = "manager-1_12345678-1234-1234-1234-123456789abc"
		}},
		{"unprefixed leader option", func(f *softwareRegistryAPI) { f.manager.Spec.Containers[0].Args = []string{"leader-elect"} }},
		{"custom hostname", func(f *softwareRegistryAPI) { f.manager.Spec.Hostname = "custom-name" }},
		{"leader election disabled", func(f *softwareRegistryAPI) {
			f.manager.Spec.Containers[0].Args = []string{"--ca-trust-bundle-configmap-name=" + softwareCABundleName}
		}},
		{"registry image mismatch", func(f *softwareRegistryAPI) {
			f.registry.Spec.Containers[0].Image = "registry.example/other@sha256:other"
		}},
		{"registry nginx configuration changed", func(f *softwareRegistryAPI) {
			f.registryConfig.Data[softwareRegistryConfigKey] += "\n# unexpected mapping"
		}},
		{"nginx configuration has no UID", func(f *softwareRegistryAPI) { f.registryConfig.UID = "" }},
		{"misidentified nginx configuration response", func(f *softwareRegistryAPI) { f.registryConfig.Namespace = "other" }},
		{"deleting nginx configuration", func(f *softwareRegistryAPI) { f.registryConfig.DeletionTimestamp = ptr.To(metav1.NewTime(time.Now())) }},
		{"binary data shadows nginx key", func(f *softwareRegistryAPI) {
			f.registryConfig.BinaryData = map[string][]byte{softwareRegistryConfigKey: []byte("other")}
		}},
		{"nginx command changed", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].Command[2] += " --config /tmp/other.conf" }},
		{"nginx command arguments override config", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].Args = []string{"-c", "/tmp/other.conf"} }},
		{"nginx port environment override", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].Env[0].Value = "8080" }},
		{"nginx config volume mismatch", func(f *softwareRegistryAPI) { f.registry.Spec.Volumes[1].ConfigMap.Name = "other" }},
		{"nginx config mount key mismatch", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].VolumeMounts[1].SubPath = "other.conf" }},
		{"nginx config mount writable", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].VolumeMounts[1].ReadOnly = false }},
		{"missing public CA", func(f *softwareRegistryAPI) { f.config.Data = map[string]string{} }},
		{"misidentified CA response", func(f *softwareRegistryAPI) { f.config.Name = "unexpected-ca" }},
		{"claim not bound", func(f *softwareRegistryAPI) { f.claim.Status.Phase = corev1.ClaimPending }},
		{"misidentified claim response", func(f *softwareRegistryAPI) { f.claim.Namespace = "other" }},
		{"different claim", func(f *softwareRegistryAPI) { f.manager.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other-pvc" }},
		{"subpath mount", func(f *softwareRegistryAPI) { f.registry.Spec.Containers[0].VolumeMounts[0].SubPath = "subdir" }},
		{"overlapping mount", func(f *softwareRegistryAPI) {
			f.manager.Spec.Containers[0].VolumeMounts = append(f.manager.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/bfb/components"})
		}},
		{"wrong host path", func(f *softwareRegistryAPI) {
			f.registry.Spec.Volumes[0] = corev1.Volume{Name: "bfb", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/bfb", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, client := newSoftwareRegistryAPI(t)
			state.mu.Lock()
			tc.mutate(state)
			state.mu.Unlock()
			if _, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace); err == nil {
				t.Fatal("unsafe registry binding was accepted")
			}
		})
	}
}

func TestRegistrySnapshotBindingDetectsLeaderAndStorageChanges(t *testing.T) {
	state, client := newSoftwareRegistryAPI(t)
	before, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	*state.lease.Spec.HolderIdentity = "manager-0_aaaaaaaa-1234-1234-1234-123456789abc"
	state.registry.ResourceVersion = "2"
	state.config.Data[softwareCABundleKey] = string(softwareTestCAPEM(t))
	state.config.ResourceVersion = "2"
	state.registryConfig.UID = "replacement-nginx-config"
	state.mu.Unlock()
	after, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if before.sameBinding(after) {
		t.Fatal("leader epoch or CA rotation was not detected")
	}
}

func TestObserveRegistrySnapshotSupportsPinnedHostPathAndCustomPublicCA(t *testing.T) {
	state, client := newSoftwareRegistryAPI(t)
	state.mu.Lock()
	state.registry.Spec.Volumes[0] = corev1.Volume{Name: "bfb", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: softwareBFBHostPath, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}
	state.manager.Spec.Volumes[0] = corev1.Volume{Name: "bfb-volume", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: softwareBFBHostPath, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}
	state.manager.Spec.Containers[0].Args[1] = "--ca-trust-bundle-configmap-name=custom-ca"
	state.mu.Unlock()
	got, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if got.storage.kind != "hostPath" || got.storage.hostPath != softwareBFBHostPath || got.caName != "custom-ca" {
		t.Fatalf("valid pinned hostPath/custom CA configuration was not resolved: %#v", got)
	}
	state.mu.Lock()
	state.manager.Spec.NodeName = "node-b"
	state.mu.Unlock()
	if _, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace); err == nil || !strings.Contains(err.Error(), "same node") {
		t.Fatalf("hostPath mismatch across nodes was accepted: %v", err)
	}
}

func TestRegistrySnapshotBindsContainerCommandImageAndStorageIdentity(t *testing.T) {
	state, client := newSoftwareRegistryAPI(t)
	first, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.registry.Status.ContainerStatuses[0].ImageID = "containerd://sha256:replacement"
	state.claim.UID = types.UID("replacement-claim")
	state.claim.ResourceVersion = "2"
	state.mu.Unlock()
	second, err := client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if first.sameBinding(second) {
		t.Fatal("in-place server or mounted claim identity change was missed")
	}
}

func TestRegistrySnapshotErrorsDoNotExposeAPIServerText(t *testing.T) {
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pods/bfb-registry") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"secret-registry-test"}`)
			return
		}
		http.NotFound(w, r)
	})
	client, err := NewAPIClient(context.Background(), api.request, api.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.observeRegistrySnapshot(context.Background(), softwareTestNamespace)
	if err == nil || strings.Contains(err.Error(), "secret-registry-test") {
		t.Fatalf("API failure was not safely surfaced: %v", err)
	}
}
