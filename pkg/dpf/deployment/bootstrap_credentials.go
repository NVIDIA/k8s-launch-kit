// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/cluster-bootstrap/token/jws"
)

var bootstrapTokenPattern = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)

// bootstrapCredentialReason is a finite public projection. It never includes
// the secret name/token ID, command, raw API error or agent status message.
func bootstrapCredentialReason(code string) string {
	switch code {
	case "BOOTSTRAP_CREDENTIALS_UNMEASURED":
		return "Bootstrap credential inspection was canceled or unavailable."
	case "BOOTSTRAP_TENANT_UNVERIFIED":
		return "The bound tenant API or its credential source changed."
	case "BOOTSTRAP_DPU_CHANGED":
		return "The selected DPU identity or tenant reference changed."
	case "BOOTSTRAP_REGISTRATION_UNMEASURED":
		return "The expected tenant registration could not be inspected."
	case "BOOTSTRAP_JOIN_SECRET_UNAVAILABLE":
		return "The selected DPU join Secret is absent or cannot be read."
	case "BOOTSTRAP_JOIN_SECRET_UNVERIFIED":
		return "The selected DPU join Secret has invalid ownership, command or tenant trust."
	case "BOOTSTRAP_TOKEN_UNAVAILABLE":
		return "The matching tenant bootstrap token is absent or cannot be read."
	case "BOOTSTRAP_TOKEN_UNVERIFIED":
		return "The matching tenant bootstrap token has invalid identity, usage or expiration."
	case "BOOTSTRAP_TOKEN_EXPIRED":
		return "The matching tenant bootstrap token has expired."
	case "BOOTSTRAP_JWS_UNVERIFIED":
		return "Current tenant cluster-info does not provide a verified matching bootstrap signature and trust."
	case "BOOTSTRAP_CREDENTIALS_CHANGED":
		return "The referenced bootstrap credentials changed during inspection."
	default:
		return ""
	}
}

func bootstrapCredentialFailure(journal *executionJournal, code string) error {
	return activationFailure(journal, code, bootstrapCredentialReason(code)+" Preserve the operation and reboot requests; resolve enrollment credentials through the supported recovery procedure before continuing.", nil)
}

func bootstrapCredentialDiagnostic(err error) string {
	var failure *ReconciliationError
	if errors.As(err, &failure) && failure != nil {
		if reason := bootstrapCredentialReason(failure.ReasonCode); reason != "" {
			return " Bootstrap inspection: " + failure.ReasonCode + "; " + reason
		}
	}
	return ""
}

// verifyAbsentRegistrationCredentials checks only whether missing initial
// registration still has usable bootstrap material at this observation. A
// present expected-name Node is not an ownership or readiness proof and grants
// no exemption from the independent activation, Node, lease or runtime fences.
func verifyAbsentRegistrationCredentials(ctx context.Context, management ResourceClient, tenant *tenantClient, expected *unstructured.Unstructured, journal *executionJournal) error {
	fail := func(code string) error { return bootstrapCredentialFailure(journal, code) }
	if ctx == nil || ctx.Err() != nil || management == nil || tenant == nil || tenant.api == nil || expected == nil || expected.GetUID() == "" || expected.GetGeneration() <= 0 {
		return fail("BOOTSTRAP_CREDENTIALS_UNMEASURED")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	dpu, err := management.Get(ctx, expected.DeepCopy())
	if err != nil || !bootstrapDPUMatches(dpu, expected, tenant) {
		return fail("BOOTSTRAP_DPU_CHANGED")
	}
	node, err := tenant.api.CoreV1().Nodes().Get(ctx, dpu.GetName(), metav1.GetOptions{})
	if err == nil {
		if node == nil || node.Name != dpu.GetName() || node.Namespace != "" || node.UID == "" || node.ResourceVersion == "" || node.DeletionTimestamp != nil {
			return fail("BOOTSTRAP_REGISTRATION_UNMEASURED")
		}
		again, readErr := tenant.api.CoreV1().Nodes().Get(ctx, dpu.GetName(), metav1.GetOptions{})
		if readErr != nil || again == nil || again.Name != node.Name || again.Namespace != "" || again.ResourceVersion == "" || again.UID != node.UID || again.DeletionTimestamp != nil {
			return fail("BOOTSTRAP_REGISTRATION_UNMEASURED")
		}
		return verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal)
	}
	if !apierrors.IsNotFound(err) {
		return fail("BOOTSTRAP_REGISTRATION_UNMEASURED")
	}
	joinIdentity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret"}}
	joinIdentity.SetNamespace(dpu.GetNamespace())
	joinIdentity.SetName(dpu.GetName() + "-kubeadm-join")
	joinObject, err := management.Get(ctx, joinIdentity)
	if err != nil {
		return fail("BOOTSTRAP_JOIN_SECRET_UNAVAILABLE")
	}
	join, ok := bootstrapJoinSecret(joinObject, dpu)
	if !ok {
		return fail("BOOTSTRAP_JOIN_SECRET_UNVERIFIED")
	}
	sources, err := tenant.source.observeTenantSources(ctx, tenant.expected)
	if err != nil {
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	digest, endpoint, adminData, err := validateTenantSources(tenant.expected, tenant.dpuClusterUID, sources)
	if err != nil || digest != tenant.sourceDigest {
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	admin, err := clientcmd.Load(adminData)
	if err != nil || admin.Contexts[admin.CurrentContext] == nil {
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	cluster := admin.Clusters[admin.Contexts[admin.CurrentContext].Cluster]
	if cluster == nil {
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	id, secret, ok := parseBootstrapJoin(string(join.Data["join"]), endpoint, cluster.CertificateAuthorityData)
	if !ok {
		return fail("BOOTSTRAP_JOIN_SECRET_UNVERIFIED")
	}
	token, err := tenant.api.CoreV1().Secrets("kube-system").Get(ctx, "bootstrap-token-"+id, metav1.GetOptions{})
	if err != nil {
		return fail("BOOTSTRAP_TOKEN_UNAVAILABLE")
	}
	if code := validateBootstrapToken(token, dpu, id, secret, time.Now()); code != "" {
		return fail(code)
	}
	info, err := tenant.api.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil || !bootstrapClusterInfoValid(info, endpoint, cluster.CertificateAuthorityData, id, secret) {
		return fail("BOOTSTRAP_JWS_UNVERIFIED")
	}
	// Referenced credentials and public signature must remain the same through
	// the read. Routine unrelated resources are not part of this closure.
	joinAgain, err := management.Get(ctx, joinIdentity.DeepCopy())
	if err != nil || !reflect.DeepEqual(joinAgain, joinObject) {
		return fail("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	tokenAgain, err := tenant.api.CoreV1().Secrets("kube-system").Get(ctx, token.Name, metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(tokenAgain, token) {
		return fail("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	infoAgain, err := tenant.api.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(infoAgain, info) {
		return fail("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	if code := validateBootstrapToken(tokenAgain, dpu, id, secret, time.Now()); code != "" {
		return fail(code)
	}
	_, err = tenant.api.CoreV1().Nodes().Get(ctx, dpu.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("BOOTSTRAP_REGISTRATION_UNMEASURED")
	}
	return verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal)
}

func bootstrapDPUMatches(current, expected *unstructured.Unstructured, tenant *tenantClient) bool {
	if current == nil || current.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || current.GetKind() != "DPU" {
		return false
	}
	if current.GetName() != expected.GetName() || current.GetNamespace() != expected.GetNamespace() || current.GetUID() != expected.GetUID() || current.GetGeneration() != expected.GetGeneration() {
		return false
	}
	if current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil || !reflect.DeepEqual(current.Object["spec"], expected.Object["spec"]) {
		return false
	}
	name, _, e1 := unstructured.NestedString(current.Object, "spec", "cluster", "name")
	namespace, _, e2 := unstructured.NestedString(current.Object, "spec", "cluster", "namespace")
	return e1 == nil && e2 == nil && name == tenant.expected.Name && namespace == tenant.expected.Namespace
}

func verifyBootstrapIdentityClosure(ctx context.Context, management ResourceClient, tenant *tenantClient, dpu *unstructured.Unstructured, journal *executionJournal) error {
	current, err := management.Get(ctx, dpu.DeepCopy())
	if err != nil || !bootstrapDPUMatches(current, dpu, tenant) {
		return bootstrapCredentialFailure(journal, "BOOTSTRAP_DPU_CHANGED")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return bootstrapCredentialFailure(journal, "BOOTSTRAP_TENANT_UNVERIFIED")
	}
	if ctx.Err() != nil {
		return bootstrapCredentialFailure(journal, "BOOTSTRAP_CREDENTIALS_UNMEASURED")
	}
	return nil
}

func bootstrapJoinSecret(object, dpu *unstructured.Unstructured) (*corev1.Secret, bool) {
	if object == nil || object.GetAPIVersion() != "v1" || object.GetKind() != "Secret" || object.GetName() != dpu.GetName()+"-kubeadm-join" || object.GetNamespace() != dpu.GetNamespace() || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
		return nil, false
	}
	secret := &corev1.Secret{}
	if runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, secret) != nil || len(secret.Data["join"]) == 0 || len(secret.Data["join"]) > 8192 {
		return nil, false
	}
	owned := false
	for _, owner := range secret.OwnerReferences {
		if owner.Kind == "DPU" {
			if owner.APIVersion != dpu.GetAPIVersion() || owner.Name != dpu.GetName() || owner.UID != dpu.GetUID() {
				return nil, false
			}
			owned = true
		}
	}
	return secret, owned
}

// parseBootstrapJoin accepts only the pinned controller's bounded command
// grammar. It never invokes a shell or resolves a discovered endpoint.
func parseBootstrapJoin(command, endpoint string, ca []byte) (string, string, bool) {
	if len(command) == 0 || len(command) > 8192 {
		return "", "", false
	}
	fields := strings.Split(command, " ")
	if len(fields) < 8 || len(fields) > 22 || len(fields)%2 != 0 || fields[0] != "kubeadm" || fields[1] != "join" || "https://"+fields[2] != endpoint || fields[3] != "--token" || !bootstrapTokenPattern.MatchString(fields[4]) || fields[5] != "--v=5" {
		return "", "", false
	}
	pins := map[string]bool{}
	for len(ca) > 0 {
		block, rest := pem.Decode(ca)
		if block == nil || block.Type != "CERTIFICATE" || len(pins) >= 8 {
			return "", "", false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", "", false
		}
		hash := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		pins["sha256:"+hex.EncodeToString(hash[:])] = false
		ca = rest
	}
	if len(pins) == 0 || len(fields) != 6+2*len(pins) {
		return "", "", false
	}
	for i := 6; i < len(fields); i += 2 {
		seen, exists := pins[fields[i+1]]
		if fields[i] != "--discovery-token-ca-cert-hash" || !exists || seen {
			return "", "", false
		}
		pins[fields[i+1]] = true
	}
	token := strings.Split(fields[4], ".")
	return token[0], token[1], true
}

func validateBootstrapToken(token *corev1.Secret, dpu *unstructured.Unstructured, id, secret string, now time.Time) string {
	if token == nil || token.Name != "bootstrap-token-"+id || token.Namespace != "kube-system" || token.UID == "" || token.ResourceVersion == "" || token.DeletionTimestamp != nil || token.Type != corev1.SecretTypeBootstrapToken {
		return "BOOTSTRAP_TOKEN_UNVERIFIED"
	}
	if token.Labels["provisioning.dpu.nvidia.com/dpu-name"] != dpu.GetName() || token.Labels["provisioning.dpu.nvidia.com/dpu-namespace"] != dpu.GetNamespace() {
		return "BOOTSTRAP_TOKEN_UNVERIFIED"
	}
	if string(token.Data["token-id"]) != id || subtle.ConstantTimeCompare(token.Data["token-secret"], []byte(secret)) != 1 {
		return "BOOTSTRAP_TOKEN_UNVERIFIED"
	}
	if string(token.Data["usage-bootstrap-authentication"]) != "true" || string(token.Data["usage-bootstrap-signing"]) != "true" || string(token.Data["auth-extra-groups"]) != "system:bootstrappers:kubeadm:default-node-token" {
		return "BOOTSTRAP_TOKEN_UNVERIFIED"
	}
	expires, err := time.Parse(time.RFC3339, string(token.Data["expiration"]))
	if err != nil {
		return "BOOTSTRAP_TOKEN_UNVERIFIED"
	}
	if !now.Before(expires) {
		return "BOOTSTRAP_TOKEN_EXPIRED"
	}
	return ""
}

func bootstrapClusterInfoValid(info *corev1.ConfigMap, endpoint string, ca []byte, id, secret string) bool {
	if info == nil || info.Name != "cluster-info" || info.Namespace != "kube-public" || info.UID == "" || info.ResourceVersion == "" || info.DeletionTimestamp != nil {
		return false
	}
	content, signature := info.Data["kubeconfig"], info.Data["jws-kubeconfig-"+id]
	if len(content) == 0 || len(content) > 1<<20 || len(signature) == 0 || len(signature) > 8192 || !jws.DetachedTokenIsValid(signature, content, id, secret) {
		return false
	}
	config, err := clientcmd.Load([]byte(content))
	if err != nil || len(config.Clusters) != 1 {
		return false
	}
	for _, cluster := range config.Clusters {
		if cluster == nil || cluster.Server != endpoint || cluster.InsecureSkipTLSVerify || cluster.CertificateAuthority != "" || cluster.TLSServerName != "" || cluster.ProxyURL != "" || !reflect.DeepEqual(cluster.CertificateAuthorityData, ca) {
			return false
		}
	}
	return true
}

// verifySelectedBootstrapCredentials consumes the already ownership-qualified
// activation snapshot; missing or replaced selected identities cannot be omitted.
func verifySelectedBootstrapCredentials(ctx context.Context, client ResourceClient, tenant *tenantClient, selected []activationSelectedDevice, indexes []int, observed activationObservation, journal *executionJournal) error {
	for _, index := range indexes {
		if index < 0 || index >= len(selected) {
			return bootstrapCredentialFailure(journal, "BOOTSTRAP_DPU_CHANGED")
		}
		dpu := observed.dpus[selected[index].dpuName]
		if dpu == nil {
			return bootstrapCredentialFailure(journal, "BOOTSTRAP_DPU_CHANGED")
		}
		if err := verifyAbsentRegistrationCredentials(ctx, client, tenant, dpu, journal); err != nil {
			return err
		}
	}
	return nil
}
