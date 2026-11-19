// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net/url"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// Verify the selected per-cluster values' actual credential relationship. No
// token contents are reported or persisted; only selected source equality is
// returned. Managed labels are the controller's cross-cluster ownership model.
func (o *validationObserver) verifyValidationSystemCredentials(ctx context.Context, reader validationManagementReader, tenant *tenantClient, role systemArtifactRole) error {
	if !role.Management {
		return nil
	}
	if tenant == nil {
		return validationFailure(validation.Unverified, validation.ReasonDependency, "Selected tenant identity is unavailable for component credential inspection.")
	}
	intent := o.desired.plan.Intent()
	selected := intent.Bootstrap.Tenant
	suffix := systemClusterSuffix(selected.Name, selected.Namespace)
	account := role.Component + "-" + suffix
	name := systemCredentialRequestStem(role) + "-" + suffix
	secretName := role.Component + "-credentials-" + suffix
	request, err := reader.Get(ctx, validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUServiceCredentialRequest", o.desired.operatorNamespace, name))
	if err != nil {
		return err
	}
	if request.GetUID() == "" || request.GetResourceVersion() == "" || request.GetDeletionTimestamp() != nil {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected component credential request has no current identity.")
	}
	expectedSpec := map[string]any{"targetCluster": map[string]any{"name": selected.Name, "namespace": selected.Namespace}, "serviceAccount": map[string]any{"name": account, "namespace": o.desired.operatorNamespace}, "secret": map[string]any{"name": secretName, "namespace": o.desired.operatorNamespace}, "type": "tokenFile"}
	spec, found, specErr := unstructured.NestedMap(request.Object, "spec")
	if specErr != nil || !found {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "Credential request spec is missing or malformed.")
	}
	for key, wanted := range expectedSpec {
		if !jsonEqual(wanted, spec[key]) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "System component credential target or reference differs from the selected tenant.")
		}
	}
	if err := validationReady(o.desired.rules.SystemCredentialReadiness(request)); err != nil {
		return err
	}
	expires, _, _ := unstructured.NestedString(request.Object, "status", "expirationTimestamp")
	expiry, expiryErr := time.Parse(time.RFC3339Nano, expires)
	if expiryErr != nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected component token credential has no valid expiration evidence.")
	}
	if !expiry.After(time.Now()) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected component token credential has expired.")
	}
	targetUID, hasUID, _ := unstructured.NestedString(request.Object, "status", "targetClusterUID")
	if hasUID && targetUID != tenant.dpuClusterUID {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Component credential was issued for a different current tenant source identity.")
	}

	object, err := reader.Get(ctx, validationObject("v1", "Secret", o.desired.operatorNamespace, secretName))
	if err != nil {
		return err
	}
	var secret corev1.Secret
	encoded, err := json.Marshal(object.Object)
	if err != nil {
		return err
	}
	if json.Unmarshal(encoded, &secret) != nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Selected credential source could not be decoded.")
	}
	if secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil || secret.Labels["svc.dpu.nvidia.com/managed-by-controller"] != "dpuservicecredentialrequestcontroller" || secret.Labels["svc.dpu.nvidia.com/credential-request-name"] != name || secret.Labels["svc.dpu.nvidia.com/credential-request-namespace"] != o.desired.operatorNamespace {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Selected component credential Secret has an unexpected controller binding.")
	}
	endpoint, kubeconfig, err := tenant.bootstrapCredentialSource(ctx)
	if err != nil {
		return err
	}
	origin, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	source, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return err
	}
	context := source.Contexts[source.CurrentContext]
	if context == nil || source.Clusters[context.Cluster] == nil {
		return validationFailure(validation.Unverified, validation.ReasonIncomplete, "Bound tenant public trust source is unavailable.")
	}
	expectedCA := source.Clusters[context.Cluster].CertificateAuthorityData
	if string(secret.Data["KUBERNETES_SERVICE_HOST"]) != origin.Hostname() || string(secret.Data["KUBERNETES_SERVICE_PORT"]) != origin.Port() || !bytes.Equal(secret.Data["KUBERNETES_CA_DATA"], expectedCA) || len(secret.Data["TOKEN_FILE"]) == 0 {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Component credential endpoint or public trust does not match the independently bound tenant.")
	}
	return nil
}
