// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	activationRebootGuardLabel = "nvidia.kubernetes-launch-kit/external-reboot-guard"
	activationRebootGuardName  = "dpf-rb-"
)

type activationRebootGuardDevice struct {
	SerialNumber string    `json:"serialNumber"`
	DPUName      string    `json:"dpuName"`
	DPUUID       types.UID `json:"dpuUID"`
	SourcePhase  string    `json:"sourcePhase"`
}

type activationRebootGuardNode struct {
	NodeName     string                        `json:"nodeName"`
	NodeUID      types.UID                     `json:"nodeUID"`
	RebootMethod string                        `json:"rebootMethod"`
	SelectedDPUs []activationRebootGuardDevice `json:"selectedDPUs"`
}

type activationRebootGuardIdentity struct {
	Version        int                         `json:"version"`
	BundleDigest   string                      `json:"bundleDigest"`
	DeploymentUID  types.UID                   `json:"deploymentUID"`
	HostSystemUUID string                      `json:"hostSystemUUID"`
	DPUNodes       []activationRebootGuardNode `json:"dpuNodes"`
}

// claimAndRebootExternal atomically claims this complete host cohort before
// the physical action. A prior or uncertain claim is never adopted or replayed.
func claimAndRebootExternal(ctx context.Context, client ResourceClient, deployment *unstructured.Unstructured, bundleDigest string, request ActivationRequest,
	sourcePhases map[types.UID]string, journal *executionJournal, reboot ExternalRebooter,
) (ActivationObservation, error) {
	if ctx == nil || client == nil || deployment == nil || journal == nil || reboot == nil {
		return ActivationObservation{}, activationFailure(journal, "EXTERNAL_REBOOT_GUARD_INPUT_INVALID", "A durable host reboot claim requires the selected deployment, management client, report and reboot executor.", nil)
	}
	if err := ctx.Err(); err != nil {
		return ActivationObservation{}, activationFailure(journal, "ACTIVATION_CANCELED", "The host reboot claim was canceled before its effect.", err)
	}
	if err := request.VerifyResetSafetyFresh(); err != nil {
		code := "ACTIVATION_RESET_SAFETY_UNVERIFIED"
		if errors.Is(err, ErrActivationResetSafetyExpired) {
			code = "ACTIVATION_RESET_SAFETY_EXPIRED"
		}
		return ActivationObservation{}, activationFailure(journal, code, "The BMC reset-safety proof is absent, invalid or expired before the one-time claim; no reboot was submitted.", err)
	}
	desired, identityDigest, err := activationRebootGuardObject(deployment, bundleDigest, request, sourcePhases)
	if err != nil {
		return ActivationObservation{}, activationFailure(journal, "EXTERNAL_REBOOT_GUARD_INPUT_INVALID", "The selected host reboot claim identity is incomplete or ambiguous.", err)
	}
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "claim-external-host-reboot", Subject: desired.GetNamespace() + "/" + desired.GetName(),
		State: "attempted", ReasonCode: "EXTERNAL_REBOOT_GUARD_CREATE_STARTED", Message: fmt.Sprintf("Claim the immutable one-time reboot identity for the selected host and complete DPU cohort; the earliest local BMC proof expires %s.", request.ResetSafetyExpiresAt.UTC().Format(time.RFC3339Nano))}); err != nil {
		return ActivationObservation{}, err
	}
	created, err := client.Create(ctx, desired, false)
	if err != nil {
		code, message := "EXTERNAL_REBOOT_GUARD_CREATE_UNCERTAIN", "The one-time host reboot claim could not be confirmed; do not retry the physical action until the claim is inspected."
		if apierrors.IsAlreadyExists(err) {
			code, message = "EXTERNAL_REBOOT_ALREADY_CLAIMED", "A one-time host reboot claim already exists for this selected deployment and cohort; inspect the prior attempt before proceeding."
		}
		return ActivationObservation{}, activationFailure(journal, code, message, err)
	}
	if !activationRebootGuardMatches(created, desired, identityDigest) {
		return ActivationObservation{}, activationFailure(journal, "EXTERNAL_REBOOT_GUARD_CREATE_UNVERIFIED", "The created one-time host reboot claim did not return its exact immutable identity; no reboot was attempted.", nil)
	}
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "claim-external-host-reboot", Subject: desired.GetNamespace() + "/" + desired.GetName(),
		State: "confirmed", ReasonCode: "EXTERNAL_REBOOT_GUARD_CREATED", Message: fmt.Sprintf("Immutable one-time host reboot claim UID %s is durable before the host action.", created.GetUID()),
	}); err != nil {
		return ActivationObservation{}, err
	}
	if request.recordClaim != nil {
		if err := request.recordClaim(created.GetUID()); err != nil {
			return ActivationObservation{}, err
		}
	}
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "external-host-reboot", Subject: request.HostInputRef,
		State: "attempted", ReasonCode: "EXTERNAL_HOST_REBOOT_STARTED", Message: "DPF requested a SystemLevelReset for the exact selected DPUNode cohort on this authenticated host."}); err != nil {
		return ActivationObservation{}, err
	}
	// Claim creation and durable journal writes can block. They never extend
	// the observed proof's lifetime or permit an automatic replacement claim.
	if err := request.VerifyResetSafetyFresh(); err != nil {
		return ActivationObservation{}, activationFailure(journal, "ACTIVATION_RESET_SAFETY_EXPIRED", "The BMC reset-safety proof expired after the durable claim but before callback submission; leave the claim and request in place.", err)
	}
	observation, err := reboot.RebootExternal(ctx, request)
	if err != nil {
		if errors.Is(err, ErrActivationResetSafetyExpired) {
			return ActivationObservation{}, activationFailure(journal, "ACTIVATION_RESET_SAFETY_EXPIRED", "The native rebooter refused an expired BMC proof before command submission; retain the one-time claim and do not replay it.", err)
		}
		return ActivationObservation{}, activationFailure(journal, "EXTERNAL_HOST_REBOOT_UNCERTAIN", "The selected host reboot did not return a verifiable completion; leave DPF's request in place and inspect the host before retrying.", err)
	}
	return observation, nil
}

func activationRebootGuardObject(deployment *unstructured.Unstructured, bundleDigest string, request ActivationRequest, sourcePhases map[types.UID]string) (*unstructured.Unstructured, string, error) {
	if deployment == nil || deployment.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || deployment.GetKind() != "DPUDeployment" || deployment.GetName() == "" ||
		deployment.GetNamespace() == "" || deployment.GetUID() == "" || !validSHA256Digest(bundleDigest) || request.HostInputRef == "" || request.HostEndpoint == "" ||
		!validUUID(request.HostSystemUUID) || request.HostAdapterKey == "" || request.HostAdapterSerial == "" || len(request.DPUNodes) == 0 {
		return nil, "", fmt.Errorf("deployment and selected host identity are required")
	}
	identity := activationRebootGuardIdentity{
		Version: 1, BundleDigest: bundleDigest, DeploymentUID: deployment.GetUID(), HostSystemUUID: strings.ToLower(request.HostSystemUUID),
		DPUNodes: make([]activationRebootGuardNode, 0, len(request.DPUNodes)),
	}
	seenNodeNames, seenNodeUIDs := map[string]bool{}, map[types.UID]bool{}
	seenDPUSerials, seenDPUNames, seenDPUUIDs := map[string]bool{}, map[string]bool{}, map[types.UID]bool{}
	hostAdapterSelected := false
	for _, node := range request.DPUNodes {
		if node.NodeName == "" || node.NodeUID == "" || node.RebootMethod != "SystemLevelReset" || len(node.SelectedDPUs) == 0 || seenNodeNames[node.NodeName] || seenNodeUIDs[node.NodeUID] {
			return nil, "", fmt.Errorf("selected DPUNode identity or reboot method is incomplete or duplicated")
		}
		seenNodeNames[node.NodeName], seenNodeUIDs[node.NodeUID] = true, true
		guardNode := activationRebootGuardNode{NodeName: node.NodeName, NodeUID: node.NodeUID, RebootMethod: node.RebootMethod,
			SelectedDPUs: make([]activationRebootGuardDevice, 0, len(node.SelectedDPUs))}
		for _, dpu := range node.SelectedDPUs {
			if dpu.SerialNumber == "" || dpu.DPUName == "" || dpu.DPUUID == "" || dpu.HostAdapterKey == "" || dpu.AdapterSerial == "" ||
				seenDPUSerials[dpu.SerialNumber] || seenDPUNames[dpu.DPUName] || seenDPUUIDs[dpu.DPUUID] {
				return nil, "", fmt.Errorf("selected DPU and associated adapter identity is incomplete or duplicated")
			}
			seenDPUSerials[dpu.SerialNumber], seenDPUNames[dpu.DPUName], seenDPUUIDs[dpu.DPUUID] = true, true, true
			sourcePhase, found := sourcePhases[dpu.DPUUID]
			if !found || !supportedActivationRebootSourcePhase(sourcePhase) {
				return nil, "", fmt.Errorf("selected DPU source phase is missing or unsupported")
			}
			if dpu.HostAdapterKey == request.HostAdapterKey && dpu.AdapterSerial == request.HostAdapterSerial {
				hostAdapterSelected = true
			}
			guardNode.SelectedDPUs = append(guardNode.SelectedDPUs, activationRebootGuardDevice{
				SerialNumber: dpu.SerialNumber, DPUName: dpu.DPUName, DPUUID: dpu.DPUUID, SourcePhase: sourcePhase,
			})
		}
		sort.Slice(guardNode.SelectedDPUs, func(i, j int) bool {
			return guardNode.SelectedDPUs[i].SerialNumber < guardNode.SelectedDPUs[j].SerialNumber
		})
		identity.DPUNodes = append(identity.DPUNodes, guardNode)
	}
	sort.Slice(identity.DPUNodes, func(i, j int) bool { return identity.DPUNodes[i].NodeName < identity.DPUNodes[j].NodeName })
	if !hostAdapterSelected {
		return nil, "", fmt.Errorf("anchor host adapter is not part of the selected cohort")
	}
	if len(sourcePhases) != len(seenDPUUIDs) {
		return nil, "", fmt.Errorf("source phases do not match the selected DPU cohort")
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, "", fmt.Errorf("encode selected host reboot identity")
	}
	digestBytes := sha256.Sum256(encoded)
	identityDigest := hex.EncodeToString(digestBytes[:])
	name := activationRebootGuardName + identityDigest[:56]
	owner := map[string]any{"apiVersion": deployment.GetAPIVersion(), "kind": deployment.GetKind(), "name": deployment.GetName(), "uid": string(deployment.GetUID())}
	data := map[string]any{
		"bundleDigest": identity.BundleDigest, "deploymentUID": string(identity.DeploymentUID), "hostSystemUUID": identity.HostSystemUUID,
		"cohortDigest": identityDigest,
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": deployment.GetNamespace(),
			"labels":          map[string]any{activationRebootGuardLabel: "true"},
			"ownerReferences": []any{owner},
		},
		"immutable": true,
		"data":      data,
	}}
	return object, "sha256:" + identityDigest, nil
}

func supportedActivationRebootSourcePhase(phase string) bool {
	switch phase {
	case "Initialize Interface", "Update Firmware", "DPU Config":
		return true
	default:
		return false
	}
}

func activationRebootGuardMatches(created, desired *unstructured.Unstructured, identityDigest string) bool {
	if created == nil || desired == nil || created.GetAPIVersion() != "v1" || created.GetKind() != "ConfigMap" || created.GetName() != desired.GetName() ||
		created.GetNamespace() != desired.GetNamespace() || created.GetUID() == "" || created.GetResourceVersion() == "" {
		return false
	}
	immutable, found, err := unstructured.NestedBool(created.Object, "immutable")
	owners, ownerFound, ownerErr := unstructured.NestedSlice(created.Object, "metadata", "ownerReferences")
	wantOwners, wantOwnerFound, wantOwnerErr := unstructured.NestedSlice(desired.Object, "metadata", "ownerReferences")
	if err != nil || !found || !immutable || !jsonEqual(created.Object["data"], desired.Object["data"]) || ownerErr != nil || wantOwnerErr != nil ||
		!ownerFound || !wantOwnerFound || !jsonEqual(owners, wantOwners) || !validUUID(string(created.GetUID())) {
		return false
	}
	labels := created.GetLabels()
	data, found, err := unstructured.NestedStringMap(created.Object, "data")
	return err == nil && found && labels[activationRebootGuardLabel] == "true" && data["cohortDigest"] == strings.TrimPrefix(identityDigest, "sha256:")
}
