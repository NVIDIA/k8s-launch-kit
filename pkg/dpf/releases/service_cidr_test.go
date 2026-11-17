// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"bytes"
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCatalogOwnsTheSelectedTenantServiceCIDR(t *testing.T) {
	before := bytes.Clone(catalogYAML)
	for _, value := range []string{"172.31.0.0/16", "", "bad", "172.31.1.0/16", "2001:db8::/64", "0.0.0.0/0"} {
		var data map[string]any
		if err := yaml.Unmarshal(catalogYAML, &data); err != nil {
			t.Fatal(err)
		}
		row := data["releases"].(map[string]any)["26.8.0"].(map[string]any)
		row["tenantServiceCIDR"] = value
		raw, err := yaml.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolve(raw, "26.8.0")
		if value == "172.31.0.0/16" {
			if err != nil || got.TenantServiceCIDR != value {
				t.Fatalf("alternate sourced CIDR lost: %+v %v", got, err)
			}
		} else if err == nil {
			t.Fatalf("invalid HBN controller CIDR accepted: %q", value)
		}
		delete(row, "tenantServiceCIDR")
		raw, err = yaml.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(raw, "26.8.0"); err == nil {
			t.Fatal("missing HBN controller CIDR accepted")
		}
	}
	if !bytes.Equal(before, catalogYAML) {
		t.Fatal("synthetic resolution mutated catalog")
	}
}

func TestSavedTenantServiceCIDRPreservesLegacyUnknownWithoutHydration(t *testing.T) {
	release, err := DefaultForUseCase(UseCaseHBNBGP)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "172.31.0.0/16"} {
		summary := Summarize(release)
		summary.TenantServiceCIDR = value
		for _, format := range []struct {
			name   string
			encode func(any) ([]byte, error)
			decode func([]byte, any) error
		}{{"json", json.Marshal, json.Unmarshal}, {"yaml", yaml.Marshal, yaml.Unmarshal}} {
			raw, err := format.encode(summary)
			if err != nil {
				t.Fatal(err)
			}
			if value == "" && bytes.Contains(raw, []byte("tenantServiceCIDR")) {
				t.Fatal("legacy encoding acquired a field")
			}
			var saved Summary
			if err := format.decode(raw, &saved); err != nil {
				t.Fatal(err)
			}
			restored, err := saved.Release()
			if err != nil || restored.TenantServiceCIDR != value {
				t.Fatalf("saved %s fact hydrated/replaced: %q %v", format.name, restored.TenantServiceCIDR, err)
			}
			// Preserve the old wire document, including unrelated pre-existing
			// collection representations. Restored semantics must keep this
			// scalar unknown; decoding never fills it from a newer catalog.
			if Summarize(restored).TenantServiceCIDR != value {
				t.Fatal("scalar roundtrip changed saved authority")
			}
			again, err := format.encode(saved)
			if err != nil || !bytes.Equal(raw, again) {
				t.Fatalf("saved %s bytes changed: %v", format.name, err)
			}
		}
	}
	for _, invalid := range []string{"bad", "172.31.1.0/16", "2001:db8::/64", "0.0.0.0/0"} {
		summary := Summarize(release)
		summary.TenantServiceCIDR = invalid
		if _, err := summary.Release(); err == nil {
			t.Fatalf("malformed saved scalar accepted: %s", invalid)
		}
	}
}
