// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
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
		return "The bound tenant API or its credential source could not be verified."
	case "BOOTSTRAP_DPU_CHANGED":
		return "The selected DPU identity or tenant reference could not be verified."
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

// bootstrapReadCause is private diagnostic state, not a persisted credential or
// schema field. Only these classified values may enter a public failure.
type bootstrapReadCause string

func (bootstrapReadCause) Error() string { return "bootstrap material read failed" }

func classifyBootstrapReadCause(err error) bootstrapReadCause {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "timeout-or-canceled"
	case apierrors.IsNotFound(err):
		return "not-found"
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "access-denied"
	default:
		return "other-api-failure"
	}
}

func bootstrapReadDiagnostic(err error) string {
	var cause bootstrapReadCause
	if !errors.As(err, &cause) {
		return ""
	}
	switch cause {
	case "not-found", "access-denied", "timeout-or-canceled", "other-api-failure":
		return " Read cause: " + string(cause) + "."
	default:
		return ""
	}
}

func bootstrapCredentialFailure(journal *executionJournal, code string) error {
	return bootstrapCredentialReadFailure(journal, code, nil)
}

func bootstrapCredentialReadFailure(journal *executionJournal, code string, readErr error) error {
	var cause error
	if readErr != nil {
		cause = classifyBootstrapReadCause(readErr)
	}
	message := bootstrapCredentialReason(code) + bootstrapReadDiagnostic(cause) + " Preserve the operation and reboot requests. Follow https://nvidia.github.io/k8s-launch-kit/user/dpf-deployment/#recover-from-unavailable-bootstrap-credentials before continuing."
	return activationFailure(journal, code, message, cause)
}

// Cancellation is an activation stop, not a claim that enrollment credentials
// need repair. It never permits a later registration to waive the stop.
func bootstrapInspectionCanceled(journal *executionJournal, cause error) error {
	safeCause := classifyBootstrapReadCause(cause)
	return activationFailure(journal, "ACTIVATION_CANCELED", "Activation stopped during bootstrap prerequisite inspection."+bootstrapReadDiagnostic(safeCause)+" Preserve the operation and current state before continuing.", errors.Join(safeCause, cause))
}

func bootstrapCredentialDiagnostic(err error) string {
	var failure *ReconciliationError
	if errors.As(err, &failure) && failure != nil {
		if reason := bootstrapCredentialReason(failure.ReasonCode); reason != "" {
			return " Bootstrap inspection: " + failure.ReasonCode + "; " + reason + bootstrapReadDiagnostic(failure)
		}
	}
	return ""
}

// verifyAbsentRegistrationCredentials checks only whether missing initial
// registration still has usable bootstrap material at this observation. A
// present expected-name Node is not an ownership or readiness proof and grants
// no exemption from the independent activation, Node, lease or runtime fences.
func verifyAbsentRegistrationCredentials(ctx context.Context, management resourceReader, tenant tenantBootstrapReader, expected *unstructured.Unstructured, journal *executionJournal) error {
	fail := func(code string) error { return bootstrapCredentialFailure(journal, code) }
	if ctx == nil || management == nil || tenant == nil || expected == nil || expected.GetUID() == "" || expected.GetGeneration() <= 0 {
		return fail("BOOTSTRAP_CREDENTIALS_UNMEASURED")
	}
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		if ctx.Err() != nil {
			return bootstrapInspectionCanceled(journal, ctx.Err())
		}
		return fail("BOOTSTRAP_TENANT_UNVERIFIED")
	}
	dpu, err := management.Get(ctx, expected.DeepCopy())
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if err != nil || !bootstrapDPUMatches(dpu, expected, tenant) {
		return fail("BOOTSTRAP_DPU_CHANGED")
	}
	registered, registrationCode := observeBootstrapRegistration(ctx, tenant, dpu)
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if registrationCode != "" {
		return fail(registrationCode)
	}
	if registered {
		return verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal)
	}
	// Only material failures may finish with a final registration observation.
	// Source/identity/cancellation failures above and below remain direct stops.
	materialFailure := func(code string) error {
		return finishBootstrapMaterialFailure(ctx, management, tenant, dpu, code, nil, journal)
	}
	materialReadFailure := func(code string, readErr error) error {
		return finishBootstrapMaterialFailure(ctx, management, tenant, dpu, code, readErr, journal)
	}
	joinIdentity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret"}}
	joinIdentity.SetNamespace(dpu.GetNamespace())
	joinIdentity.SetName(dpu.GetName() + "-kubeadm-join")
	joinObject, err := management.Get(ctx, joinIdentity)
	if err != nil {
		return materialReadFailure("BOOTSTRAP_JOIN_SECRET_UNAVAILABLE", err)
	}
	join, ok := bootstrapJoinSecret(joinObject, dpu)
	if !ok {
		return materialFailure("BOOTSTRAP_JOIN_SECRET_UNVERIFIED")
	}
	endpoint, adminData, err := tenant.bootstrapCredentialSource(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return bootstrapInspectionCanceled(journal, ctx.Err())
		}
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
		return materialFailure("BOOTSTRAP_JOIN_SECRET_UNVERIFIED")
	}
	token, err := tenant.readSecret(ctx, "kube-system", "bootstrap-token-"+id)
	if err != nil {
		return materialReadFailure("BOOTSTRAP_TOKEN_UNAVAILABLE", err)
	}
	if code := validateBootstrapToken(token, dpu, id, secret, time.Now()); code != "" {
		return materialFailure(code)
	}
	info, err := tenant.readConfigMap(ctx, "kube-public", "cluster-info")
	if err != nil {
		return materialReadFailure("BOOTSTRAP_JWS_UNVERIFIED", err)
	}
	if !bootstrapClusterInfoValid(info, endpoint, cluster.CertificateAuthorityData, id, secret) {
		return materialFailure("BOOTSTRAP_JWS_UNVERIFIED")
	}
	// Revalidate the same referenced identities and consumed values. The
	// bootstrap signer may update other tokens' signatures in this ConfigMap;
	// unrelated data and routine metadata/RV changes grant no credential authority.
	joinAgainObject, err := management.Get(ctx, joinIdentity.DeepCopy())
	if err != nil {
		return materialReadFailure("BOOTSTRAP_CREDENTIALS_CHANGED", err)
	}
	joinAgain, valid := bootstrapJoinSecret(joinAgainObject, dpu)
	if !valid || joinAgain.UID != join.UID || !bytes.Equal(joinAgain.Data["join"], join.Data["join"]) {
		return materialFailure("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	tokenAgain, err := tenant.readSecret(ctx, "kube-system", token.Name)
	if err != nil {
		return materialReadFailure("BOOTSTRAP_CREDENTIALS_CHANGED", err)
	}
	if tokenAgain == nil || tokenAgain.UID != token.UID {
		return materialFailure("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	// Identity/value/usage/groups/required labels are all fixed by validation;
	// expiration is also required to remain the exact value read initially.
	if code := validateBootstrapToken(tokenAgain, dpu, id, secret, time.Now()); code != "" {
		return materialFailure(code)
	}
	if !bytes.Equal(tokenAgain.Data["expiration"], token.Data["expiration"]) {
		return materialFailure("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	infoAgain, err := tenant.readConfigMap(ctx, "kube-public", "cluster-info")
	if err != nil {
		return materialReadFailure("BOOTSTRAP_CREDENTIALS_CHANGED", err)
	}
	if !bootstrapClusterInfoValid(infoAgain, endpoint, cluster.CertificateAuthorityData, id, secret) {
		return materialFailure("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	if infoAgain.UID != info.UID || infoAgain.Data["kubeconfig"] != info.Data["kubeconfig"] || infoAgain.Data["jws-kubeconfig-"+id] != info.Data["jws-kubeconfig-"+id] {
		return materialFailure("BOOTSTRAP_CREDENTIALS_CHANGED")
	}
	_, err = tenant.readNode(ctx, dpu.GetName())
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("BOOTSTRAP_REGISTRATION_UNMEASURED")
	}
	return verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal)
}

// observeBootstrapRegistration reports expected-name presence only. Both the
// initial inspection and final material-failure decision use the same stable
// Node observation; neither claims ownership or readiness.
func observeBootstrapRegistration(ctx context.Context, tenant tenantBootstrapReader, dpu *unstructured.Unstructured) (bool, string) {
	if ctx.Err() != nil {
		return false, "BOOTSTRAP_CREDENTIALS_UNMEASURED"
	}
	node, err := tenant.readNode(ctx, dpu.GetName())
	if apierrors.IsNotFound(err) {
		return false, ""
	}
	if err != nil || node == nil || node.Name != dpu.GetName() || node.Namespace != "" || node.UID == "" || node.ResourceVersion == "" || node.DeletionTimestamp != nil {
		return false, "BOOTSTRAP_REGISTRATION_UNMEASURED"
	}
	again, err := tenant.readNode(ctx, dpu.GetName())
	if err != nil || again == nil || again.Name != node.Name || again.Namespace != "" || again.ResourceVersion == "" || again.UID != node.UID || again.DeletionTimestamp != nil {
		return false, "BOOTSTRAP_REGISTRATION_UNMEASURED"
	}
	if ctx.Err() != nil {
		return false, "BOOTSTRAP_CREDENTIALS_UNMEASURED"
	}
	return true, ""
}

// finishBootstrapMaterialFailure handles normal producer progress once, before
// recording a failure: registration can appear while DPF cleans up join data.
// It never retries material reads or waives tenant/DPU/registration identity.
func finishBootstrapMaterialFailure(ctx context.Context, management resourceReader, tenant tenantBootstrapReader, dpu *unstructured.Unstructured, code string, readErr error, journal *executionJournal) error {
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if errors.Is(readErr, context.Canceled) {
		return bootstrapInspectionCanceled(journal, readErr)
	}
	if err := verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal); err != nil {
		return err
	}
	registered, registrationCode := observeBootstrapRegistration(ctx, tenant, dpu)
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if registrationCode != "" {
		return bootstrapCredentialFailure(journal, registrationCode)
	}
	if err := verifyBootstrapIdentityClosure(ctx, management, tenant, dpu, journal); err != nil {
		return err
	}
	if registered {
		return nil
	}
	return bootstrapCredentialReadFailure(journal, code, readErr)
}

func bootstrapDPUMatches(current, expected *unstructured.Unstructured, tenant tenantBootstrapReader) bool {
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
	return e1 == nil && e2 == nil && name == tenant.expectation().Name && namespace == tenant.expectation().Namespace
}

func verifyBootstrapIdentityClosure(ctx context.Context, management resourceReader, tenant tenantBootstrapReader, dpu *unstructured.Unstructured, journal *executionJournal) error {
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	current, err := management.Get(ctx, dpu.DeepCopy())
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
	}
	if err != nil || !bootstrapDPUMatches(current, dpu, tenant) {
		return bootstrapCredentialFailure(journal, "BOOTSTRAP_DPU_CHANGED")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		if ctx.Err() != nil {
			return bootstrapInspectionCanceled(journal, ctx.Err())
		}
		return bootstrapCredentialFailure(journal, "BOOTSTRAP_TENANT_UNVERIFIED")
	}
	if ctx.Err() != nil {
		return bootstrapInspectionCanceled(journal, ctx.Err())
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
func verifySelectedBootstrapCredentials(ctx context.Context, client resourceReader, tenant tenantBootstrapReader, selected []activationSelectedDevice, indexes []int, observed activationObservation, journal *executionJournal) error {
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
