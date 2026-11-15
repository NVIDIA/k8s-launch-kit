// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Hooks run only after the real stage has created its children. Returning one
// incomplete observation models a controller wait without altering stage order.
type activationHoldFenceClient struct {
	*activationIntegrationFake
	duringWait       func()
	duringRefresh    func()
	afterRelease     func()
	waited           bool
	maintenanceReads int
	releases         int
}

func (c *activationHoldFenceClient) List(ctx context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if c.deployment != nil && wanted.GetKind() == "DPUSet" && !c.waited && c.duringWait != nil {
		c.waited = true
		c.duringWait()
		return []unstructured.Unstructured{}, nil
	}
	if c.deployment != nil && wanted.GetKind() == "DPUNodeMaintenance" {
		c.maintenanceReads++
		if c.maintenanceReads == 2 && c.duringRefresh != nil {
			c.duringRefresh()
		}
	}
	return c.activationIntegrationFake.List(ctx, wanted, selector)
}

func (c *activationHoldFenceClient) patchActivationAnnotation(ctx context.Context, object *unstructured.Unstructured, uid types.UID, rv, key string, value *string) (*unstructured.Unstructured, error) {
	updated, err := c.activationIntegrationFake.patchActivationAnnotation(ctx, object, uid, rv, key, value)
	if err == nil && key == activationHoldKey && value != nil && *value == "false" {
		c.releases++
		if c.releases == 1 && c.afterRelease != nil {
			c.afterRelease()
		}
	}
	return updated, err
}

func bindHoldFenceClient(f *activationIntegrationFixture, c *activationHoldFenceClient) {
	f.controlPlane.tenant.Client.source = c
	f.workload.ControlPlane.tenant.Client.source = c
	f.client.tenant.source = c
	f.client.base.tenant.source = c
}

func replaceHoldFixtureObject(t *testing.T, f *activationIntegrationFixture, kind string) {
	t.Helper()
	for _, object := range f.client.base.resources {
		if object.GetKind() == kind {
			object.SetUID("replacement-uid")
			return
		}
	}
	t.Fatalf("missing fixture %s", kind)
}

func TestActivationHoldReleaseRejectsDependencyDriftDuringChildWait(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	cases := []struct {
		name   string
		mutate func(*testing.T, *activationIntegrationFixture)
	}{
		{"software", func(t *testing.T, f *activationIntegrationFixture) {
			replaceHoldFixtureObject(t, f, "BlueFieldSoftware")
		}},
		{"registry", func(_ *testing.T, f *activationIntegrationFixture) {
			f.client.managerImage = "example.test/foreign:tag"
		}},
		{"flavor", func(t *testing.T, f *activationIntegrationFixture) { replaceHoldFixtureObject(t, f, "DPUFlavor") }},
		{"enrollment", func(t *testing.T, f *activationIntegrationFixture) { replaceHoldFixtureObject(t, f, "DPUDevice") }},
		{"tenant", func(_ *testing.T, f *activationIntegrationFixture) {
			f.client.sources.sources.observed.TenantControlPlane.SetUID("replacement-tenant")
		}},
		{"config", func(t *testing.T, f *activationIntegrationFixture) {
			for _, object := range f.client.base.resources {
				if object.GetKind() == "DPFOperatorConfig" {
					setBootstrapTestReadiness(t, object, "False", "Error")
					return
				}
			}
			t.Fatal("missing Config")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationHoldFenceClient{activationIntegrationFake: f.client}
			c.duringWait = func() { tc.mutate(t, &f) }
			bindHoldFenceClient(&f, c)
			callbacks := 0
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
				RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
					callbacks++
					return ActivationObservation{}, nil
				}))
			if !c.waited || err == nil || len(c.patches) != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || callbacks != 0 {
				t.Fatalf("waited=%v err=%v patches=%v claims=%d reset checks=%d callbacks=%d", c.waited, err, c.patches, c.guardCalls, c.resetSafetyCalls, callbacks)
			}
		})
	}
}

func TestActivationHoldReleaseRechecksDependenciesBetweenSelectedHosts(t *testing.T) {
	f := newActivationIntegrationFixtureForSerials(t, []string{"SERIAL-A", "SERIAL-B"})
	c := &activationHoldFenceClient{activationIntegrationFake: f.client}
	c.afterRelease = func() { replaceHoldFixtureObject(t, &f, "BlueFieldSoftware") }
	bindHoldFenceClient(&f, c)
	callbacks := 0
	_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
		RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
			callbacks++
			return ActivationObservation{}, nil
		}))
	if err == nil || c.releases != 1 || len(c.patches) != 1 || c.guardCalls != 0 || c.resetSafetyCalls != 0 || callbacks != 0 {
		t.Fatalf("err=%v releases=%d patches=%v claims=%d reset checks=%d callbacks=%d", err, c.releases, c.patches, c.guardCalls, c.resetSafetyCalls, callbacks)
	}
}

func TestActivationHoldReleaseUsesFreshSelectedMaintenance(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	cases := []struct {
		name   string
		mutate func(*testing.T, *activationHoldFenceClient)
	}{
		{"replaced UID", func(_ *testing.T, c *activationHoldFenceClient) { c.maint[0].SetUID("replacement") }},
		{"requestors", func(t *testing.T, c *activationHoldFenceClient) {
			if err := unstructured.SetNestedStringSlice(c.maint[0].Object, []string{"foreign"}, "spec", "requestor"); err != nil {
				t.Fatal(err)
			}
		}},
		{"action", func(t *testing.T, c *activationHoldFenceClient) {
			if err := unstructured.SetNestedField(c.maint[0].Object, false, "spec", "nodeEffect", "hold"); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing hold", func(_ *testing.T, c *activationHoldFenceClient) {
			a := c.maint[0].GetAnnotations()
			delete(a, activationHoldKey)
			c.maint[0].SetAnnotations(a)
		}},
		{"disappeared", func(_ *testing.T, c *activationHoldFenceClient) { c.maint = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationHoldFenceClient{activationIntegrationFake: f.client}
			mutated := false
			c.duringRefresh = func() {
				if len(c.maint) != 1 {
					t.Fatalf("maintenance count=%d", len(c.maint))
				}
				mutated = true
				tc.mutate(t, c)
			}
			bindHoldFenceClient(&f, c)
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
				RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
					t.Error("unexpected reboot")
					return ActivationObservation{}, nil
				}))
			if !mutated || err == nil || len(c.patches) != 0 || c.guardCalls != 0 || c.resetSafetyCalls != 0 {
				t.Fatalf("mutated=%v err=%v patches=%v claims=%d reset checks=%d", mutated, err, c.patches, c.guardCalls, c.resetSafetyCalls)
			}
		})
	}
}

func TestActivationHoldReleaseAcceptsFreshVersionAndReleasedCohorts(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, tc := range []struct {
		name                      string
		sameHost, alreadyReleased bool
	}{{"two hosts fresh version", false, false}, {"same host two DPUs", true, false}, {"already released", false, true}, {"same host already released", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, tc.sameHost)
			c := &activationHoldFenceClient{activationIntegrationFake: f.client}
			if tc.alreadyReleased {
				c.duringWait = func() {
					for i := range c.maint {
						a := c.maint[i].GetAnnotations()
						a[activationHoldKey] = "false"
						c.maint[i].SetAnnotations(a)
					}
				}
			}
			refreshed := false
			c.duringRefresh = func() {
				refreshed = true
				for i := range c.maint {
					c.maint[i].SetResourceVersion("fresh-version")
				}
			}
			bindHoldFenceClient(&f, c)
			callbacks := 0
			receipt, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
				RebooterFunc(func(_ context.Context, r ActivationRequest) (ActivationObservation, error) {
					callbacks++
					for _, node := range r.DPUNodes {
						c.completeReboot(node.NodeName)
					}
					return ActivationObservation{HostSystemUUID: r.HostSystemUUID, HostAdapterSerial: r.HostAdapterSerial, PreviousBootID: "before", CurrentBootID: "after", Reconnected: true}, nil
				}))
			wantReleases := uniqueNodeCount(f.client.activationSelected)
			if tc.alreadyReleased {
				wantReleases = 0
			}
			if err != nil || len(receipt.DPUs) != 2 || callbacks != len(f.hosts) || c.guardCalls != len(f.hosts) || c.releases != wantReleases || !tc.alreadyReleased && !refreshed {
				t.Fatalf("err=%v DPUs=%d callbacks=%d hosts=%d claims=%d releases=%d want=%d refreshed=%v", err, len(receipt.DPUs), callbacks, len(f.hosts), c.guardCalls, c.releases, wantReleases, refreshed)
			}
		})
	}
}

func TestActivationHoldReleaseRequiresFreshBoundary(t *testing.T) {
	if _, err := releaseNextSelectedHold(context.Background(), nil, nil, activationObservation{}, nil, nil, nil); err == nil {
		t.Fatal("missing mandatory boundary accepted")
	}
}
