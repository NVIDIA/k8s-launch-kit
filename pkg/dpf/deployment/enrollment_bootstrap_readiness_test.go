// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type bootstrapReadinessTestClient struct {
	*tenantStageFake
	get       func(*unstructured.Unstructured, *unstructured.Unstructured)
	readError func(*unstructured.Unstructured) error
}

func (f *bootstrapReadinessTestClient) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	current, err := f.tenantStageFake.Get(ctx, wanted)
	if err == nil && f.get != nil {
		f.get(wanted, current)
	}
	if err == nil && f.readError != nil {
		if err := f.readError(wanted); err != nil {
			return nil, err
		}
	}
	return current, err
}

func setBootstrapTestReadiness(t *testing.T, object *unstructured.Unstructured, status, reason string) {
	t.Helper()
	if err := unstructured.SetNestedSlice(object.Object, []any{map[string]any{
		"type": "Ready", "status": status, "reason": reason, "observedGeneration": int64(1),
	}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentWaitsForTenantManagementComponentsBeforeEffects(t *testing.T) {
	execution, fake, bootstrapReceipt, tenantReceipt, password := enrollmentStageFixture(t)
	configReads := 0
	fake.getHook = func(wanted *unstructured.Unstructured) {
		if wanted.GetKind() != "DPFOperatorConfig" {
			return
		}
		configReads++
		current := fake.base.resources[tenantStageKey(wanted)]
		if configReads <= 3 {
			setBootstrapTestReadiness(t, current, "False", "Error")
		} else {
			setBootstrapTestReadiness(t, current, "True", "Success")
		}
	}
	fake.bmcHook = func(int) error {
		if configReads < 4 {
			t.Fatal("BMC access started before the operator configuration recovered")
		}
		return nil
	}
	journal := testJournal(execution)
	result, err := runEnrollmentStage(tenantStageContext(t), fake, execution, bootstrapReceipt, tenantReceipt,
		fake.selected, password, journal, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("normal tenant-service convergence stopped enrollment: %v", err)
	}
	if len(result.Devices) != 1 || fake.base.createCount != 2 || configReads < 4 {
		t.Fatal("the recovered bootstrap did not complete the actual enrollment flow")
	}
	text := journal.file.(*managementMemoryFile).String()
	if !strings.Contains(text, "RESOURCE_NOT_READY") || !strings.Contains(text, "Controller reason: Error") || !strings.Contains(text, "DPF_CONFIG_READY") || strings.Contains(text, password) {
		t.Fatal("the wait did not preserve its readiness reason or credential privacy")
	}
}

func TestEnrollmentBootstrapWaitPreservesHardBoundaries(t *testing.T) {
	input := preflightGenerationInputs(t)
	setSiteAllocation(&input.Operator.Bootstrap.Tenant)
	generated, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(generated, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		waited     bool
	}{
		{name: "config replaced before wait", want: "BOOTSTRAP_CONFIG_IDENTITY_CHANGED", waited: false},
		{name: "config read denied before wait", want: "BOOTSTRAP_CONFIG_READ_FAILED", waited: false},
		{name: "config replaced during wait", want: "DPF_CONFIG_INTENT_CHANGED", waited: true},
		{name: "config deleted during wait", want: "DPF_CONFIG_INTENT_CHANGED", waited: true},
		{name: "config spec changed", want: "DPF_CONFIG_INTENT_CHANGED", waited: true},
		{name: "config ownership changed", want: "retained intent", waited: false},
		{name: "wrong version while pending", want: "retained operator version", waited: false},
		{name: "terminal condition", want: "RESOURCE_TERMINAL_FAILURE", waited: true},
		{name: "manager replaced while pending", want: "manager Deployment", waited: true},
		{name: "namespace replaced while pending", want: "namespace", waited: true},
		{name: "canceled while pending", want: "DPF_CONFIG_READINESS_CANCELED", waited: true},
		{name: "deadline while pending", want: "DPF_CONFIG_READINESS_", waited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newTenantStageFake(t, execution)
			receipt := tenantStageBootstrapReceipt(execution, base)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if tc.name == "deadline while pending" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			configReads := 0
			client := &bootstrapReadinessTestClient{tenantStageFake: base}
			client.get = func(wanted, current *unstructured.Unstructured) {
				if wanted.GetKind() == "DPFOperatorConfig" {
					configReads++
					if configReads <= 3 || tc.name == "deadline while pending" {
						setBootstrapTestReadiness(t, current, "False", "Error")
					} else {
						setBootstrapTestReadiness(t, current, "True", "Success")
					}
					switch tc.name {
					case "config replaced before wait":
						if configReads == 2 {
							current.SetUID("replacement-config")
						}
					case "config replaced during wait":
						if configReads >= 3 {
							current.SetUID("replacement-config")
						}
					case "config deleted during wait":
						if configReads >= 3 {
							now := metav1.Now()
							current.SetDeletionTimestamp(&now)
						}
					case "config spec changed":
						if configReads == 3 {
							if err := unstructured.SetNestedField(current.Object, int64(1400), "spec", "networking", "controlPlaneMTU"); err != nil {
								t.Fatal(err)
							}
						}
					case "config ownership changed":
						annotations := current.GetAnnotations()
						annotations[BundleDigestAnnotation] = "foreign"
						current.SetAnnotations(annotations)
					case "wrong version while pending":
						if err := unstructured.SetNestedField(current.Object, "v0.0.1", "status", "version"); err != nil {
							t.Fatal(err)
						}
					case "terminal condition":
						setBootstrapTestReadiness(t, current, "False", "Failure")
					case "canceled while pending":
						if configReads == 2 {
							cancel()
						}
					}
				}
				if configReads >= 4 && (tc.name == "manager replaced while pending" && wanted.GetKind() == "Deployment" || tc.name == "namespace replaced while pending" && wanted.GetKind() == "Namespace") {
					current.SetUID("replacement-bootstrap-resource")
				}
			}
			client.readError = func(wanted *unstructured.Unstructured) error {
				if tc.name == "config read denied before wait" && wanted.GetKind() == "DPFOperatorConfig" && configReads == 2 {
					return apierrors.NewForbidden(schema.GroupResource{Group: "operator.dpu.nvidia.com", Resource: "dpfoperatorconfigs"}, wanted.GetName(), errors.New("bootstrap-private-canary"))
				}
				return nil
			}
			journal := testJournal(execution)
			err := waitManagementBootstrap(ctx, client, execution, receipt, journal, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.want) || base.createCount != 0 {
				t.Fatalf("invalid bootstrap reached effects or lost its failure: creates=%d err=%v", base.createCount, err)
			}
			report := journal.file.(*managementMemoryFile).String()
			if strings.Contains(report, "bootstrap-private-canary") || strings.Contains(err.Error(), "bootstrap-private-canary") {
				t.Fatal("the bootstrap read failure exposed raw API error text")
			}
			if tc.name == "deadline while pending" {
				check, projected := operationResumeCheck(ctx, execution, "OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", err)
				if check.ReasonCode != "DPF_CONFIG_READINESS_TIMEOUT" || !errors.Is(projected, context.DeadlineExceeded) {
					t.Fatalf("pending deadline was relabeled: %+v %v", check, projected)
				}
			}
			if strings.Contains(report, "DPF_CONFIG_READINESS_STARTED") != tc.waited {
				t.Fatalf("readiness wait branch differs from expected classification: %s", report)
			}
		})
	}
}

func TestEnrollmentRechecksTenantIdentityAfterBootstrapWait(t *testing.T) {
	execution, fake, bootstrapReceipt, tenantReceipt, password := enrollmentStageFixture(t)
	configReads := 0
	fake.getHook = func(wanted *unstructured.Unstructured) {
		if wanted.GetKind() != "DPFOperatorConfig" {
			return
		}
		configReads++
		status := "True"
		if configReads <= 3 {
			status = "False"
		}
		setBootstrapTestReadiness(t, fake.base.resources[tenantStageKey(wanted)], status, "Error")
		if configReads == 3 {
			fake.sources.sources.secret.UID = "replacement-tenant-credential"
		}
	}
	journal := testJournal(execution)
	_, err := runEnrollmentStage(tenantStageContext(t), fake, execution, bootstrapReceipt, tenantReceipt,
		fake.selected, password, journal, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "TENANT_API_IDENTITY_CHANGED") || fake.bmcCalls != 0 || fake.base.createCount != 0 {
		t.Fatalf("stale tenant identity reached enrollment after waiting: err=%v", err)
	}
}

func TestLaterEnrollmentBoundaryDoesNotRetryBootstrapFailure(t *testing.T) {
	execution, fake, bootstrapReceipt, tenantReceipt, password := enrollmentStageFixture(t)
	fake.bmcHook = func(int) error {
		setBootstrapTestReadiness(t, fake.base.resources[tenantStageKey(execution.configObject)], "False", "Error")
		return nil
	}
	journal := testJournal(execution)
	_, err := runEnrollmentStage(tenantStageContext(t), fake, execution, bootstrapReceipt, tenantReceipt,
		fake.selected, password, journal, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "ENROLLMENT_BOOTSTRAP_CHANGED") {
		t.Fatalf("later bootstrap drift did not stop discovery: %v", err)
	}
	if strings.Contains(journal.file.(*managementMemoryFile).String(), "DPF_CONFIG_READINESS_STARTED") {
		t.Fatal("later enrollment fence was silently converted into a wait")
	}
	for _, object := range fake.base.resources {
		if object.GetKind() == "DPUDiscovery" {
			t.Fatal("discovery was created after the later bootstrap fence failed")
		}
	}
}
