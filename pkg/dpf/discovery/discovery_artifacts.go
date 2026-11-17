// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifactcheck"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnArtifactSourceChecks(ctx context.Context, input *config.Provisioning, inv *inventory.Inventory, verifier ArtifactVerifier, mode ArtifactSourceMode) []inventory.Check {
	const checkID = "dpf.hbn.artifact-source"
	checks := []inventory.Check{}
	add := func(subject, state, code, message string) {
		checks = append(checks, inventory.Check{ID: checkID, Subject: subject, Vantage: "executor", RequiredPhase: "generate", State: state, ReasonCode: code, Message: message})
	}

	selectedPSIDs := []string{}
	selectedSubjects := []struct{ subject, psid string }{}
	completeSelection := inv != nil && len(inv.DPUs) > 0
	if inv != nil {
		for _, dpu := range inv.DPUs {
			psid, err := ObservedDevicePSID(inv, dpu)
			if err != nil {
				completeSelection = false
				selectedSubjects = append(selectedSubjects, struct{ subject, psid string }{dpu.SerialNumber, ""})
				continue
			}
			selectedPSIDs = append(selectedPSIDs, psid)
			selectedSubjects = append(selectedSubjects, struct{ subject, psid string }{dpu.SerialNumber, psid})
		}
	}
	validationPSIDs := []string(nil)
	if completeSelection {
		validationPSIDs = selectedPSIDs
	}
	software, _, inputErr := config.ValidateProvisioningSoftware(input, validationPSIDs)
	if inputErr != nil {
		add("provisioning.software", "failed", "ARTIFACT_INPUT_INVALID", "Configured artifact inputs are invalid; correct their HTTPS URLs, PSID keys and SHA-256 locks.")
		return checks
	}
	requests := []artifactcheck.Artifact{}
	requestSubjects := []string{}
	if software.OSISO.URL == "" || software.OSISO.SHA256 == "" {
		add("osISO", "unknown", "OS_ISO_ARTIFACT_MISSING", "Configure an OS ISO HTTPS URL and SHA-256 lock before verifying its source bytes.")
	} else {
		subject := "osISO"
		requests = append(requests, artifactcheck.Artifact{Subject: subject, URL: software.OSISO.URL, SHA256: software.OSISO.SHA256})
		requestSubjects = append(requestSubjects, subject)
	}
	if len(selectedSubjects) == 0 {
		add("pldm", "unknown", "SELECTED_PSID_SCOPE_UNAVAILABLE", "Selected DPU PSIDs are not yet verified, so hardware-specific PLDM sources cannot be selected.")
	}
	slices.SortFunc(selectedSubjects, func(a, b struct{ subject, psid string }) int {
		if cmp := strings.Compare(a.psid, b.psid); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.subject, b.subject)
	})
	seenPSIDs := map[string]bool{}
	for _, selected := range selectedSubjects {
		if selected.psid == "" {
			add(selected.subject, "unknown", "SELECTED_PSID_SCOPE_UNAVAILABLE", "The selected DPU has no unambiguous observed PSID; its PLDM artifact cannot be selected safely.")
			continue
		}
		if seenPSIDs[selected.psid] {
			continue
		}
		seenPSIDs[selected.psid] = true
		artifact, ok := software.PLDM[selected.psid]
		if !ok || artifact.URL == "" || artifact.SHA256 == "" {
			add(selected.psid, "unknown", "PLDM_ARTIFACT_MISSING", "Configure the selected PSID's PLDM HTTPS URL and SHA-256 lock before verifying its source bytes.")
			continue
		}
		subject := "pldm/" + selected.psid
		requests = append(requests, artifactcheck.Artifact{Subject: subject, URL: artifact.URL, SHA256: artifact.SHA256})
		requestSubjects = append(requestSubjects, subject)
	}
	if len(requests) == 0 {
		return checks
	}
	if mode == ArtifactInputsOnly {
		for _, subject := range requestSubjects {
			add(subject, "unknown", "ARTIFACT_SOURCE_NOT_RECHECKED", "Origin bytes were not downloaded again after activation. Input URLs, locks and selected PSIDs were checked; current deployment-owned software receipts are verified separately.")
		}
		return checks
	}
	if verifier == nil {
		for _, subject := range requestSubjects {
			add(subject, "unknown", "ARTIFACT_VERIFIER_UNAVAILABLE", "No HTTPS artifact verifier is configured in this discovery execution.")
		}
		return checks
	}
	results := verifier.Verify(ctx, requests)
	if len(results) != len(requests) {
		for _, subject := range requestSubjects {
			add(subject, "unknown", "ARTIFACT_RESULT_INVALID", "Artifact verification did not return a complete result set.")
		}
		return checks
	}
	for i, result := range results {
		expected := requestSubjects[i]
		if result.Subject != expected || !validArtifactResult(result) {
			add(expected, "unknown", "ARTIFACT_RESULT_INVALID", "Artifact verification returned an invalid result for the selected source.")
			continue
		}
		add(expected, result.State, result.ReasonCode, artifactReasonMessage(result.ReasonCode))
	}
	return checks
}

func validArtifactResult(result artifactcheck.Result) bool {
	if result.Bytes < 0 {
		return false
	}
	switch result.ReasonCode {
	case "ARTIFACT_SOURCE_VERIFIED", "ARTIFACT_HTTP_STATUS", "ARTIFACT_SOURCE_HTTP_UNAVAILABLE", "ARTIFACT_REDIRECT_REJECTED", "ARTIFACT_REDIRECT_LIMIT", "ARTIFACT_SIZE_LIMIT", "ARTIFACT_TOTAL_SIZE_LIMIT", "ARTIFACT_DIGEST_MISMATCH", "ARTIFACT_CHECK_CANCELLED", "ARTIFACT_SOURCE_TIMEOUT", "ARTIFACT_SOURCE_UNAVAILABLE", "ARTIFACT_READ_FAILED", "ARTIFACT_ENCODING_UNSUPPORTED", "ARTIFACT_URL_INVALID", "ARTIFACT_LOCK_INVALID", "ARTIFACT_REQUEST_INVALID", "ARTIFACT_LIMITS_INVALID", "ARTIFACT_EMPTY":
	default:
		return false
	}
	switch result.State {
	case "passed":
		return result.ReasonCode == "ARTIFACT_SOURCE_VERIFIED"
	case "failed", "unknown":
		return result.ReasonCode != "" && result.ReasonCode != "ARTIFACT_SOURCE_VERIFIED"
	default:
		return false
	}
}

func artifactReasonMessage(code string) string {
	switch code {
	case "ARTIFACT_SOURCE_VERIFIED":
		return "The locked artifact bytes were read and matched from the discovery executor's HTTPS vantage."
	case "ARTIFACT_HTTP_STATUS":
		return "The artifact server did not return HTTP 200."
	case "ARTIFACT_SOURCE_HTTP_UNAVAILABLE":
		return "The artifact server is temporarily unavailable."
	case "ARTIFACT_REDIRECT_REJECTED":
		return "The artifact server redirected to an unsafe URL."
	case "ARTIFACT_REDIRECT_LIMIT":
		return "The artifact server exceeded the redirect limit."
	case "ARTIFACT_SIZE_LIMIT", "ARTIFACT_TOTAL_SIZE_LIMIT":
		return "Artifact verification exceeded the configured byte limit."
	case "ARTIFACT_DIGEST_MISMATCH":
		return "The artifact bytes do not match the configured SHA-256 lock."
	case "ARTIFACT_EMPTY":
		return "The artifact source returned an empty object."
	case "ARTIFACT_CHECK_CANCELLED":
		return "Artifact verification did not complete before the discovery deadline."
	case "ARTIFACT_SOURCE_TIMEOUT":
		return "The artifact source did not respond before the verification deadline."
	case "ARTIFACT_SOURCE_UNAVAILABLE":
		return "The artifact source could not be read from the discovery executor."
	case "ARTIFACT_READ_FAILED":
		return "The artifact response ended before its bytes could be verified."
	case "ARTIFACT_ENCODING_UNSUPPORTED":
		return "The artifact source returned an unsupported content encoding."
	case "ARTIFACT_URL_INVALID", "ARTIFACT_LOCK_INVALID", "ARTIFACT_REQUEST_INVALID", "ARTIFACT_LIMITS_INVALID":
		return "The configured artifact source or lock is invalid."
	default:
		return "Artifact verification did not produce a recognized result."
	}
}
