// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestSoftwareCacheTargetsUsePinnedDPFPathsAndSelectedLocks(t *testing.T) {
	desired, current, locks, selected := softwareCacheFixture()
	desiredBefore, currentBefore, locksBefore := desired.DeepCopy(), current.DeepCopy(), cloneSoftwareForTest(locks)

	got, err := softwareCacheTargets(desired, current, locks, selected)
	if err != nil {
		t.Fatalf("softwareCacheTargets() error = %v", err)
	}
	want := []softwareCacheTarget{
		{component: softwareComponentOSISO, cachePath: "/bfb/operator-hbn-software-bf4 os.iso", sha256: strings.Repeat("a", 64)},
		{component: softwareComponentPLDM, psid: "MT_TESTPSID", cachePath: "/bfb/components/operator-hbn-software-MT_TESTPSID-firmware.bin", sha256: strings.Repeat("b", 64)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("softwareCacheTargets() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(desired, desiredBefore) || !reflect.DeepEqual(current, currentBefore) || !reflect.DeepEqual(locks, locksBefore) {
		t.Fatal("softwareCacheTargets() mutated desired/live objects or retained locks")
	}
}

func TestSoftwareCacheTargetsDeduplicateSelectedDevicesByPSID(t *testing.T) {
	desired, current, locks, selected := softwareCacheFixture()
	selected = append(selected, "MT_TESTPSID")

	got, err := softwareCacheTargets(desired, current, locks, selected)
	if err != nil {
		t.Fatalf("softwareCacheTargets() error = %v", err)
	}
	if len(got) != 2 || got[1].component != softwareComponentPLDM || got[1].psid != "MT_TESTPSID" {
		t.Fatalf("softwareCacheTargets() = %#v, want one PLDM target for the repeated selected PSID", got)
	}
}

func TestSoftwareCacheTargetEscapesURLPathCharacters(t *testing.T) {
	desired, current, locks, selected := softwareCacheFixture()
	locks.PLDM["mt_testpsid"] = config.Artifact{URL: "https://artifact.example/firmware%23rev%3Fv1.bin", SHA256: strings.Repeat("b", 64)}
	_ = unstructured.SetNestedField(desired.Object, locks.PLDM["mt_testpsid"].URL, "spec", "pldmFwBundle", "MT_TESTPSID")
	_ = unstructured.SetNestedField(current.Object, locks.PLDM["mt_testpsid"].URL, "spec", "pldmFwBundle", "MT_TESTPSID")
	_ = unstructured.SetNestedField(current.Object, "/bfb/components/operator-hbn-software-MT_TESTPSID-firmware#rev?v1.bin", "status", "downloadedComponents", "pldmFwBundle", "mt_testpsid")

	targets, err := softwareCacheTargets(desired, current, locks, selected)
	if err != nil {
		t.Fatalf("softwareCacheTargets() error = %v", err)
	}
	requestURL := (&url.URL{Scheme: "https", Host: "127.0.0.1:8443", Path: targets[1].cachePath}).String()
	parsed, err := url.Parse(requestURL)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != targets[1].cachePath || parsed.EscapedPath() != "/bfb/components/operator-hbn-software-MT_TESTPSID-firmware%23rev%3Fv1.bin" {
		t.Fatalf("cache path was not encoded as a URL path: %q (%v)", requestURL, err)
	}
}

func TestSoftwareCacheTargetsRejectInvalidBindingsAndStatus(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured, *config.Software, *[]string)
	}{
		{name: "replaced UID", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			current.SetUID(types.UID(""))
		}},
		{name: "deleting live object", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			current.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-14T00:00:00Z"
		}},
		{name: "live immutable spec changed", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, "https://artifact.example/other.iso", "spec", "osIso")
		}},
		{name: "status observed generation stale", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, int64(3), "status", "observedGeneration")
		}},
		{name: "downloaded condition stale", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			setSoftwareCondition(current, "Downloaded", "True", 3)
		}},
		{name: "ready condition false", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			setSoftwareCondition(current, "Ready", "False", 4)
		}},
		{name: "malformed conditions", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, "not-a-list", "status", "conditions")
		}},
		{name: "OS status path mismatch", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, "/bfb/another.iso", "status", "downloadedComponents", "osIso")
		}},
		{name: "PLDM status path mismatch", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, "/bfb/components/other.bin", "status", "downloadedComponents", "pldmFwBundle", "MT_TESTPSID")
		}},
		{name: "ambiguous case-insensitive PLDM status key", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, "/bfb/components/other.bin", "status", "downloadedComponents", "pldmFwBundle", "mt_testpsid")
		}},
		{name: "malformed PLDM status map", mutate: func(_ *unstructured.Unstructured, current *unstructured.Unstructured, _ *config.Software, _ *[]string) {
			_ = unstructured.SetNestedField(current.Object, int64(17), "status", "downloadedComponents", "pldmFwBundle", "MT_TESTPSID")
		}},
		{name: "ambiguous retained PLDM PSID keys", mutate: func(_ *unstructured.Unstructured, _ *unstructured.Unstructured, locks *config.Software, _ *[]string) {
			locks.PLDM["MT_TESTPSID"] = config.Artifact{URL: "https://artifact.example/releases/other.bin", SHA256: strings.Repeat("c", 64)}
		}},
		{name: "unused retained PSID lock", mutate: func(_ *unstructured.Unstructured, _ *unstructured.Unstructured, locks *config.Software, _ *[]string) {
			locks.PLDM["MT_OTHERPSID"] = config.Artifact{URL: "https://artifact.example/unused.bin", SHA256: strings.Repeat("c", 64)}
		}},
		{name: "desired URL differs from retained lock", mutate: func(desired, current *unstructured.Unstructured, locks *config.Software, _ *[]string) {
			locks.OSISO.URL = "https://artifact.example/another.iso"
			_ = unstructured.SetNestedField(desired.Object, "https://artifact.example/bf4%20os.iso", "spec", "osIso")
			_ = unstructured.SetNestedField(current.Object, "https://artifact.example/bf4%20os.iso", "spec", "osIso")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired, current, locks, selected := softwareCacheFixture()
			test.mutate(desired, current, &locks, &selected)
			if got, err := softwareCacheTargets(desired, current, locks, selected); err == nil || got != nil {
				t.Fatalf("softwareCacheTargets() = %#v, %v; want rejection", got, err)
			}
		})
	}
}

func TestSoftwareCacheTargetsRejectQueryAndUnsafeURLPaths(t *testing.T) {
	for _, rawURL := range []string{
		"https://artifact.example/bf4.iso?token=private",
		"https://artifact.example/releases/../bf4.iso",
		"https://artifact.example/releases/%2e%2e/bf4.iso",
		"https://artifact.example/releases%2fbf4.iso",
		"https://artifact.example/releases/bf4.iso/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			desired, current, locks, selected := softwareCacheFixture()
			locks.OSISO.URL = rawURL
			_ = unstructured.SetNestedField(desired.Object, rawURL, "spec", "osIso")
			_ = unstructured.SetNestedField(current.Object, rawURL, "spec", "osIso")
			if got, err := softwareCacheTargets(desired, current, locks, selected); err == nil || got != nil {
				t.Fatalf("softwareCacheTargets() accepted unsafe or query-bearing URL: %#v, %v", got, err)
			}
		})
	}
}

func softwareCacheFixture() (*unstructured.Unstructured, *unstructured.Unstructured, config.Software, []string) {
	selected := []string{"mt_testpsid"}
	locks := config.Software{
		OSISO: config.Artifact{URL: "https://artifact.example/releases/bf4%20os.iso", SHA256: strings.Repeat("a", 64)},
		PLDM: map[string]config.Artifact{
			"mt_testpsid": {URL: "https://artifact.example/releases/firmware.bin", SHA256: strings.Repeat("b", 64)},
		},
	}
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": blueFieldSoftwareAPIVersion,
		"kind":       "BlueFieldSoftware",
		"metadata":   map[string]any{"namespace": "operator", "name": "hbn-software"},
		"spec": map[string]any{
			"osIso":        locks.OSISO.URL,
			"pldmFwBundle": map[string]any{"MT_TESTPSID": locks.PLDM["mt_testpsid"].URL},
		},
	}}
	current := desired.DeepCopy()
	current.SetUID(types.UID("software-uid"))
	current.SetResourceVersion("12")
	current.SetGeneration(4)
	_ = unstructured.SetNestedField(current.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedField(current.Object, int64(4), "status", "observedGeneration")
	_ = unstructured.SetNestedField(current.Object, "/bfb/operator-hbn-software-bf4 os.iso", "status", "downloadedComponents", "osIso")
	_ = unstructured.SetNestedField(current.Object, map[string]any{
		"mt_testpsid": "/bfb/components/operator-hbn-software-MT_TESTPSID-firmware.bin",
	}, "status", "downloadedComponents", "pldmFwBundle")
	_ = unstructured.SetNestedField(current.Object, []any{
		map[string]any{"type": "Downloaded", "status": "True", "reason": "Success", "observedGeneration": int64(4)},
		map[string]any{"type": "Ready", "status": "True", "reason": "Success", "observedGeneration": int64(4)},
	}, "status", "conditions")
	return desired, current, locks, selected
}

func setSoftwareCondition(object *unstructured.Unstructured, conditionType, status string, observedGeneration int64) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition := item.(map[string]any)
		if condition["type"] == conditionType {
			condition["status"] = status
			condition["observedGeneration"] = observedGeneration
		}
	}
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
}

func cloneSoftwareForTest(input config.Software) config.Software {
	copy := config.Software{OSISO: input.OSISO, PLDM: make(map[string]config.Artifact, len(input.PLDM))}
	for psid, artifact := range input.PLDM {
		copy.PLDM[psid] = artifact
	}
	return copy
}
