// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKamajiExceptionPreservesPublishedBOMAndSavedDescriptions(t *testing.T) {
	release, err := Resolve("26.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if release.KamajiCompatibility == nil {
		t.Fatal("missing HBN exception")
	}
	found := false
	for _, component := range release.Components {
		if component.Name == "kamaji" {
			found = component.Repository == "ghcr.io/nvidia/kamaji" && component.Version == "v1.36.0-26.6.4"
		}
	}
	if !found {
		t.Fatal("NVIDIA published Kamaji coordinate must remain descriptive and unchanged")
	}
	summary := Summarize(release)
	if summary.KamajiCompatibility == nil || summary.KamajiCompatibility.Image != release.KamajiCompatibility.Image || !strings.Contains(summary.KamajiCompatibility.SupportNote, "edge") {
		t.Fatal("exception must be distinct and disclose support limits")
	}
	saved, err := summary.Release()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(summary)
	again, _ := json.Marshal(Summarize(saved))
	if string(encoded) != string(again) {
		t.Fatal("compact description must round trip")
	}
	// Historical inventories need no new field; execution obtains the current
	// finite rule by selected release, rather than trusting this description.
	summary.KamajiCompatibility = nil
	old, err := summary.Release()
	if err != nil {
		t.Fatal(err)
	}
	if old.Version != "26.8.0" {
		t.Fatal("old inventory lost selected release")
	}
	old.KamajiCompatibility = &KamajiCompatibility{Image: "untrusted"}
	trusted, err := Resolve(old.Version)
	if err != nil || trusted.KamajiCompatibility.Image == old.KamajiCompatibility.Image {
		t.Fatal("saved table overrode embedded contract")
	}
	generic, err := Resolve("26.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if generic.KamajiCompatibility != nil {
		t.Fatal("exception broadened generic release")
	}
}

func TestKamajiExceptionRejectsUnattributedOrUnboundedCoordinates(t *testing.T) {
	release, err := Resolve("26.8.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*KamajiCompatibility){
		func(k *KamajiCompatibility) { k.Image = "docker.io/clastix/kamaji:latest" },
		func(k *KamajiCompatibility) { k.PlatformManifest = "sha256:bad" },
		func(k *KamajiCompatibility) { k.ConfigDigest = "sha256:bad" },
		func(k *KamajiCompatibility) { k.Platform = "linux/arm64" },
		func(k *KamajiCompatibility) { k.SupportNote = "" },
		func(k *KamajiCompatibility) { k.SourceRef = "unknown" },
	} {
		copy := release
		rule := *release.KamajiCompatibility
		copy.KamajiCompatibility = &rule
		mutate(&rule)
		if err := validate(copy.Version, copy); err == nil {
			t.Fatalf("invalid exception accepted: %+v", rule)
		}
	}
	release.KamajiCompatibility.Image = "mutated"
	fresh, err := Resolve("26.8.0")
	if err != nil || fresh.KamajiCompatibility.Image == "mutated" {
		t.Fatal("resolved contract shares mutation")
	}
}
