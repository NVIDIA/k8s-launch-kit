// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	blueFieldSoftwareAPIVersion = "provisioning.dpu.nvidia.com/v1alpha1"
	softwareComponentOSISO      = "osISO"
	softwareComponentPLDM       = "pldm"
	maxSelectedSoftwarePSIDs    = 256
	maxSoftwareCacheFilename    = 255
)

// softwareCacheTarget binds one selected software digest to the exact cache
// path derived by the pinned DPF BlueFieldSoftware controller. CachePath is a
// registry URL path, never an arbitrary filesystem path from status.
type softwareCacheTarget struct {
	component string
	psid      string
	cachePath string
	sha256    string
}

// softwareCacheTargets validates the retained software locks against both the
// rendered BlueFieldSoftware and a fresh, ready live observation. selectedPSIDs
// is the canonical device selection bound to the same retained generation.
func softwareCacheTargets(desired, current *unstructured.Unstructured, locks config.Software, selectedPSIDs []string) ([]softwareCacheTarget, error) {
	canonicalPSIDs, err := canonicalSoftwareSelection(selectedPSIDs)
	if err != nil {
		return nil, err
	}
	if err := validateSoftwareObjects(desired, current); err != nil {
		return nil, err
	}

	software, issues, err := config.ValidateProvisioningSoftware(&config.Provisioning{Software: locks}, canonicalPSIDs)
	if err != nil || len(issues) != 0 {
		return nil, fmt.Errorf("retained software locks are malformed or incomplete")
	}
	if err := softwareSpecMatches(desired, software, canonicalPSIDs); err != nil {
		return nil, err
	}
	if err := softwareReadiness(current); err != nil {
		return nil, err
	}

	namespace, name := desired.GetNamespace(), desired.GetName()
	osBase, err := dpfSoftwareFilename(software.OSISO.URL)
	if err != nil {
		return nil, fmt.Errorf("retained OS ISO URL cannot produce a safe DPF cache filename")
	}
	osFilename := namespace + "-" + name + "-" + osBase
	if !validSoftwareCacheFilename(osFilename) {
		return nil, fmt.Errorf("retained OS ISO filename exceeds the supported DPF cache path")
	}
	osPath := path.Join("/bfb", osFilename)
	statusOSPath, found, statusErr := unstructured.NestedString(current.Object, "status", "downloadedComponents", "osIso")
	if statusErr != nil || !found || statusOSPath != osPath {
		return nil, fmt.Errorf("BlueFieldSoftware OS ISO status path does not match the retained DPF filename")
	}

	statusPLDM, found, statusErr := unstructured.NestedStringMap(current.Object, "status", "downloadedComponents", "pldmFwBundle")
	if statusErr != nil || !found {
		return nil, fmt.Errorf("BlueFieldSoftware PLDM status paths are missing or malformed")
	}
	targets := make([]softwareCacheTarget, 0, len(canonicalPSIDs)+1)
	targets = append(targets, softwareCacheTarget{
		component: softwareComponentOSISO,
		cachePath: osPath,
		sha256:    software.OSISO.SHA256,
	})
	for _, psid := range canonicalPSIDs {
		artifact := software.PLDM[psid]
		base, err := dpfSoftwareFilename(artifact.URL)
		if err != nil {
			return nil, fmt.Errorf("retained PLDM URL cannot produce a safe DPF cache filename")
		}
		filename := namespace + "-" + name + "-" + psid + "-" + base
		if !validSoftwareCacheFilename(filename) {
			return nil, fmt.Errorf("retained PLDM filename exceeds the supported DPF cache path")
		}
		wantPath := path.Join("/bfb/components", filename)
		statusPath, err := uniquePSIDPath(statusPLDM, psid)
		if err != nil || statusPath != wantPath {
			return nil, fmt.Errorf("BlueFieldSoftware PLDM status path does not match the retained DPF filename")
		}
		targets = append(targets, softwareCacheTarget{
			component: softwareComponentPLDM,
			psid:      psid,
			cachePath: wantPath,
			sha256:    artifact.SHA256,
		})
	}
	return targets, nil
}

func canonicalSoftwareSelection(selectedPSIDs []string) ([]string, error) {
	if len(selectedPSIDs) == 0 || len(selectedPSIDs) > maxSelectedSoftwarePSIDs {
		return nil, fmt.Errorf("selected software PSID scope is empty or exceeds the supported device count")
	}
	seen := make(map[string]struct{}, len(selectedPSIDs))
	for _, psid := range selectedPSIDs {
		canonical := strings.ToUpper(psid)
		seen[canonical] = struct{}{}
	}
	canonical := make([]string, 0, len(seen))
	for psid := range seen {
		canonical = append(canonical, psid)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func validateSoftwareObjects(desired, current *unstructured.Unstructured) error {
	if desired == nil || current == nil || desired.GetAPIVersion() != blueFieldSoftwareAPIVersion || desired.GetKind() != "BlueFieldSoftware" ||
		current.GetAPIVersion() != blueFieldSoftwareAPIVersion || current.GetKind() != "BlueFieldSoftware" {
		return fmt.Errorf("desired and observed BlueFieldSoftware objects are required")
	}
	if desired.GetNamespace() == "" || desired.GetName() == "" || desired.GetUID() != "" || desired.GetResourceVersion() != "" || desired.GetGeneration() != 0 || desired.GetDeletionTimestamp() != nil {
		return fmt.Errorf("retained BlueFieldSoftware identity is invalid")
	}
	if current.GetNamespace() != desired.GetNamespace() || current.GetName() != desired.GetName() || current.GetUID() == "" || current.GetGeneration() <= 0 || current.GetDeletionTimestamp() != nil {
		return fmt.Errorf("live BlueFieldSoftware identity is missing, deleting or differs from retained intent")
	}
	wantSpec, wantFound, wantErr := unstructured.NestedMap(desired.Object, "spec")
	gotSpec, gotFound, gotErr := unstructured.NestedMap(current.Object, "spec")
	if wantErr != nil || gotErr != nil || !wantFound || !gotFound || !reflect.DeepEqual(wantSpec, gotSpec) {
		return fmt.Errorf("live BlueFieldSoftware immutable spec differs from retained intent")
	}
	return nil
}

func softwareSpecMatches(desired *unstructured.Unstructured, software config.Software, selectedPSIDs []string) error {
	osURL, found, err := unstructured.NestedString(desired.Object, "spec", "osIso")
	if err != nil || !found || osURL != software.OSISO.URL {
		return fmt.Errorf("BlueFieldSoftware OS ISO spec differs from retained locks")
	}
	pldm, found, err := unstructured.NestedStringMap(desired.Object, "spec", "pldmFwBundle")
	if err != nil || !found || len(pldm) != len(selectedPSIDs) {
		return fmt.Errorf("BlueFieldSoftware PLDM spec differs from the selected retained locks")
	}
	for _, psid := range selectedPSIDs {
		canonical := strings.ToUpper(psid)
		if pldm[canonical] != software.PLDM[canonical].URL {
			return fmt.Errorf("BlueFieldSoftware PLDM spec differs from the selected retained locks")
		}
	}
	return nil
}

func softwareReadiness(current *unstructured.Unstructured) error {
	if readiness := EvaluateReadiness(current); !readiness.Ready {
		return fmt.Errorf("BlueFieldSoftware is not ready: %s", readiness.ReasonCode)
	}
	return nil
}

// dpfSoftwareFilename mirrors the pinned DPF URL-basename behavior while
// rejecting paths whose URL encoding could change the selected filename.
func dpfSoftwareFilename(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("unsupported software URL")
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") || strings.HasSuffix(u.Path, "/") || path.Clean(u.Path) != u.Path {
		return "", fmt.Errorf("software URL path is not a canonical file path")
	}
	filename := path.Base(u.Path)
	if filename == "." || filename == "/" || filename == "" || filename == ".." || strings.HasPrefix(filename, "..") || strings.ContainsAny(filename, `/\\%`) {
		return "", fmt.Errorf("software URL basename is unsafe")
	}
	for _, char := range filename {
		if unicode.IsControl(char) {
			return "", fmt.Errorf("software URL basename contains a control character")
		}
	}
	escapedBase := path.Base(u.EscapedPath())
	decodedBase, err := url.PathUnescape(escapedBase)
	if err != nil || decodedBase != filename || escapedBase != url.PathEscape(filename) {
		return "", fmt.Errorf("software URL basename has ambiguous escaping")
	}
	return filename, nil
}

func validSoftwareCacheFilename(filename string) bool {
	return filename != "" && len(filename) <= maxSoftwareCacheFilename && path.Base(filename) == filename && !strings.HasPrefix(filename, "..") && !strings.ContainsAny(filename, `/\\%`)
}

func uniquePSIDPath(paths map[string]string, selectedPSID string) (string, error) {
	var result string
	count := 0
	for psid, cachePath := range paths {
		if strings.EqualFold(psid, selectedPSID) {
			result = cachePath
			count++
		}
	}
	if count != 1 || result == "" {
		return "", fmt.Errorf("selected PLDM status path is missing or ambiguous")
	}
	return result, nil
}
