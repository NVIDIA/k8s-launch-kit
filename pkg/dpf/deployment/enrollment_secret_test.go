// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const enrollmentTestPassword = "a-test-only-secret-value"
const enrollmentTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type enrollmentSecretJournalFile struct {
	bytes.Buffer
	syncs     int
	failSyncs int
}

func (f *enrollmentSecretJournalFile) Sync() error {
	f.syncs++
	if f.syncs == f.failSyncs {
		return errors.New("report sync failed")
	}
	return nil
}

func (*enrollmentSecretJournalFile) Close() error { return nil }

type enrollmentSecretFake struct {
	file      *enrollmentSecretJournalFile
	current   *unstructured.Unstructured
	getErr    error
	createErr error
	createObj *unstructured.Unstructured
	gets      int
	creates   int
	onGet     func(int)
	onCreate  func(context.Context)
}

func (f *enrollmentSecretFake) Get(_ context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.gets++
	if f.onGet != nil {
		f.onGet(f.gets)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.current == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, wanted.GetName())
	}
	return f.current.DeepCopy(), nil
}

func (f *enrollmentSecretFake) Create(ctx context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.creates++
	if f.file.syncs < 2 || dryRun {
		return nil, errors.New("create did not follow a durable journal attempt")
	}
	if desired.GetAPIVersion() != "v1" || desired.GetKind() != "Secret" || desired.GetName() != enrollmentSecretName || desired.GetNamespace() == "" {
		return nil, errors.New("unexpected Secret identity")
	}
	if f.onCreate != nil {
		f.onCreate(ctx)
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	created := desired.DeepCopy()
	if f.createObj != nil {
		created = f.createObj.DeepCopy()
	}
	if created.GetUID() == "" {
		created.SetUID(types.UID("secret-uid"))
	}
	if created.GetResourceVersion() == "" {
		created.SetResourceVersion("1")
	}
	f.current = created.DeepCopy()
	return created, nil
}

func newEnrollmentSecretFixture(t *testing.T) (*enrollmentSecretFake, *executionJournal) {
	t.Helper()
	file := &enrollmentSecretJournalFile{}
	journal := &executionJournal{file: file, digest: enrollmentTestDigest}
	return &enrollmentSecretFake{file: file}, journal
}

func enrollmentContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), time.Second)
}

func TestExecuteEnrollmentSecretCreatesOpaqueCredentialWithoutLoggingIt(t *testing.T) {
	fake, journal := newEnrollmentSecretFixture(t)
	ctx, cancel := enrollmentContext(t)
	defer cancel()
	receipt, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
	if err != nil {
		t.Fatalf("executeEnrollmentSecret() failed: %s", safeAPIError("test", err))
	}
	if fake.gets != 1 || fake.creates != 1 || receipt.Name != enrollmentSecretName || receipt.Namespace != "dpf-system" || receipt.UID != "secret-uid" || receipt.ResourceVersion != "1" || !receipt.Created {
		t.Fatalf("unexpected create receipt or calls: receipt=%#v gets=%d creates=%d", receipt, fake.gets, fake.creates)
	}
	if fake.current.GetAPIVersion() != "v1" || fake.current.GetKind() != "Secret" || fake.current.GetName() != enrollmentSecretName || fake.current.GetNamespace() != "dpf-system" || fake.current.GetUID() == "" || fake.current.GetResourceVersion() == "" {
		t.Fatal("created Secret identity is incomplete")
	}
	secretType, found, err := unstructured.NestedString(fake.current.Object, "type")
	if err != nil || !found || secretType != enrollmentSecretType {
		t.Fatal("created Secret type is not Opaque")
	}
	data, found, err := unstructured.NestedStringMap(fake.current.Object, "data")
	if err != nil || !found || len(data) != 1 || data[enrollmentSecretDataKey] != base64.StdEncoding.EncodeToString([]byte(enrollmentTestPassword)) {
		t.Fatal("created Secret data does not match the DPF credential contract")
	}
	if value, found, err := unstructured.NestedString(fake.current.Object, "metadata", "annotations", BundleDigestAnnotation); err != nil || !found || value != enrollmentTestDigest {
		t.Fatal("created Secret is not marked with the current bundle digest")
	}
	if strings.Contains(fake.file.String(), enrollmentTestPassword) {
		t.Fatal("credential bytes leaked into the report or error")
	}
	var event executionEvent
	lines := strings.Split(strings.TrimSpace(fake.file.String()), "\n")
	if len(lines) < 3 || json.Unmarshal([]byte(lines[0]), &event) != nil {
		t.Fatal("journal event was not valid JSON")
	}
	if event.State != "attempted" || event.ReasonCode != "ENROLLMENT_SECRET_INSPECTION_STARTED" {
		t.Fatal("journal did not record the inspection before API calls")
	}
}

func TestExecuteEnrollmentSecretReusesExactSecretWithoutMutation(t *testing.T) {
	fake, journal := newEnrollmentSecretFixture(t)
	fake.current = existingEnrollmentSecret("dpf-system", enrollmentTestPassword)
	labels := map[string]any{"site": "preserve"}
	_ = unstructured.SetNestedMap(fake.current.Object, labels, "metadata", "labels")
	_ = unstructured.SetNestedMap(fake.current.Object, map[string]any{"owner": "external"}, "metadata", "annotations")
	before := fake.current.DeepCopy()
	ctx, cancel := enrollmentContext(t)
	defer cancel()
	receipt, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
	if err != nil {
		t.Fatalf("exact existing Secret was rejected: %s", safeAPIError("test", err))
	}
	if fake.gets != 1 || fake.creates != 0 || receipt.Created || receipt.UID != before.GetUID() || receipt.ResourceVersion != before.GetResourceVersion() || !secretObjectEqual(fake.current, before) {
		t.Fatal("existing exact Secret was changed or not returned as reused")
	}
	if !strings.Contains(fake.file.String(), "ENROLLMENT_SECRET_REUSED") || strings.Contains(fake.file.String(), enrollmentTestPassword) {
		t.Fatal("reuse receipt is missing or contains credential bytes")
	}
}

func TestExecuteEnrollmentSecretRejectsConflictingExistingSecret(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "wrong api version", mutate: func(s *unstructured.Unstructured) { s.SetAPIVersion("v2") }},
		{name: "wrong kind", mutate: func(s *unstructured.Unstructured) { s.SetKind("ConfigMap") }},
		{name: "wrong name", mutate: func(s *unstructured.Unstructured) { s.SetName("other") }},
		{name: "wrong namespace", mutate: func(s *unstructured.Unstructured) { s.SetNamespace("other") }},
		{name: "missing uid", mutate: func(s *unstructured.Unstructured) { s.SetUID("") }},
		{name: "missing resource version", mutate: func(s *unstructured.Unstructured) { s.SetResourceVersion("") }},
		{name: "deleting", mutate: func(s *unstructured.Unstructured) { now := metav1.Now(); s.SetDeletionTimestamp(&now) }},
		{name: "owner reference", mutate: func(s *unstructured.Unstructured) {
			s.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Job", Name: "owner", UID: "owner-uid"}})
		}},
		{name: "wrong type", mutate: func(s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "kubernetes.io/basic-auth", "type")
		}},
		{name: "wrong password", mutate: func(s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, base64.StdEncoding.EncodeToString([]byte("another-value")), "data", enrollmentSecretDataKey)
		}},
		{name: "extra data key", mutate: func(s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "dXNlcm5hbWU=", "data", "username")
		}},
		{name: "missing password key", mutate: func(s *unstructured.Unstructured) { delete(s.Object["data"].(map[string]any), enrollmentSecretDataKey) }},
		{name: "stringData present", mutate: func(s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "unused", "stringData", "other")
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			fake, journal := newEnrollmentSecretFixture(t)
			fake.current = existingEnrollmentSecret("dpf-system", enrollmentTestPassword)
			test.mutate(fake.current)
			before := fake.current.DeepCopy()
			ctx, cancel := enrollmentContext(t)
			defer cancel()
			_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
			if err == nil || fake.gets != 1 || fake.creates != 0 || !secretObjectEqual(fake.current, before) {
				t.Fatal("conflicting existing Secret was adopted or changed")
			}
			if strings.Contains(err.Error(), enrollmentTestPassword) || strings.Contains(fake.file.String(), enrollmentTestPassword) {
				t.Fatal("conflicting Secret diagnostic leaked credential bytes")
			}
		})
	}
}

func TestExecuteEnrollmentSecretHandlesReadAndCreateFailuresSafely(t *testing.T) {
	t.Run("read forbidden does not create", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.getErr = apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, enrollmentSecretName, errors.New(enrollmentTestPassword))
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.creates != 0 || strings.Contains(err.Error(), enrollmentTestPassword) || strings.Contains(fake.file.String(), enrollmentTestPassword) {
			t.Fatal("read failure created a Secret or exposed credential bytes")
		}
	})

	t.Run("create error is uncertain and redacted", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.createErr = errors.New(enrollmentTestPassword)
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.creates != 1 || strings.Contains(err.Error(), enrollmentTestPassword) || strings.Contains(fake.file.String(), enrollmentTestPassword) {
			t.Fatal("uncertain create failure was retried or exposed credential bytes")
		}
		if !strings.Contains(fake.file.String(), `"state":"uncertain"`) || !strings.Contains(fake.file.String(), "ENROLLMENT_SECRET_CREATE_UNCERTAIN") {
			t.Fatal("create uncertainty was not durably recorded")
		}
	})

	t.Run("create response missing marker is uncertain", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.createObj = existingEnrollmentSecret("dpf-system", enrollmentTestPassword)
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.creates != 1 || !strings.Contains(fake.file.String(), "ENROLLMENT_SECRET_CREATE_UNCERTAIN") {
			t.Fatal("created Secret without its bundle marker was treated as verified")
		}
	})

	t.Run("wrapped cancellation is normalized before diagnostics", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.createErr = errors.Join(errors.New(enrollmentTestPassword), context.Canceled)
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), enrollmentTestPassword) || strings.Contains(fake.file.String(), enrollmentTestPassword) {
			t.Fatal("wrapped cancellation was not normalized or leaked credential bytes")
		}
	})
}

func TestExecuteEnrollmentSecretVerifiesCreateRace(t *testing.T) {
	t.Run("exact concurrent secret", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, enrollmentSecretName)
		fake.onCreate = func(context.Context) { fake.current = existingEnrollmentSecret("dpf-system", enrollmentTestPassword) }
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		receipt, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err != nil || fake.gets != 2 || fake.creates != 1 || receipt.Created || receipt.UID != "secret-existing" {
			t.Fatal("exact concurrent Secret was not freshly verified and reused")
		}
	})

	t.Run("mismatching concurrent secret", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		fake.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, enrollmentSecretName)
		fake.onCreate = func(context.Context) { fake.current = existingEnrollmentSecret("dpf-system", "different-password") }
		ctx, cancel := enrollmentContext(t)
		defer cancel()
		_, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.gets != 2 || fake.creates != 1 || !strings.Contains(fake.file.String(), "ENROLLMENT_SECRET_RACE_CONFLICT") {
			t.Fatal("mismatching concurrent Secret was accepted or retried")
		}
	})
}

func TestExecuteEnrollmentSecretStopsBeforeCallsOnInvalidInputsOrJournalFailure(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		password  string
		journal   func(*executionJournal)
	}{
		{name: "empty namespace", namespace: "", password: enrollmentTestPassword},
		{name: "invalid namespace", namespace: "bad namespace", password: enrollmentTestPassword},
		{name: "empty password", namespace: "dpf-system", password: ""},
		{name: "oversized password", namespace: "dpf-system", password: strings.Repeat("x", maxEnrollmentSecretBytes+1)},
		{name: "bad journal digest", namespace: "dpf-system", password: enrollmentTestPassword, journal: func(j *executionJournal) { j.digest = "bad" }},
		{name: "journal attempt write failed", namespace: "dpf-system", password: enrollmentTestPassword, journal: func(j *executionJournal) { j.file.(*enrollmentSecretJournalFile).failSyncs = 1 }},
		{name: "journal create-attempt write failed", namespace: "dpf-system", password: enrollmentTestPassword, journal: func(j *executionJournal) { j.file.(*enrollmentSecretJournalFile).failSyncs = 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, journal := newEnrollmentSecretFixture(t)
			if test.journal != nil {
				test.journal(journal)
			}
			ctx, cancel := enrollmentContext(t)
			defer cancel()
			_, err := executeEnrollmentSecret(ctx, fake, test.namespace, test.password, journal)
			if err == nil || fake.creates != 0 {
				t.Fatal("invalid input or failed journal allowed a Secret create")
			}
			if test.name == "journal attempt write failed" && fake.gets != 0 {
				t.Fatal("API read began before durable inspection event")
			}
			if test.name == "journal create-attempt write failed" && fake.gets != 1 {
				t.Fatal("expected one read before failed create-attempt record")
			}
		})
	}

	t.Run("unbounded context", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		_, err := executeEnrollmentSecret(context.Background(), fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.gets != 0 || fake.creates != 0 {
			t.Fatal("unbounded context reached the API")
		}
	})
	t.Run("nil context", func(t *testing.T) {
		fake, journal := newEnrollmentSecretFixture(t)
		var missingContext context.Context
		_, err := executeEnrollmentSecret(missingContext, fake, "dpf-system", enrollmentTestPassword, journal)
		if err == nil || fake.gets != 0 || fake.creates != 0 {
			t.Fatal("nil context reached the API")
		}
	})
}

func TestExecuteEnrollmentSecretCancellationDuringCreateDoesNotReportFailureAsReady(t *testing.T) {
	fake, journal := newEnrollmentSecretFixture(t)
	ctx, cancel := enrollmentContext(t)
	fake.onCreate = func(context.Context) { cancel() }
	receipt, err := executeEnrollmentSecret(ctx, fake, "dpf-system", enrollmentTestPassword, journal)
	if !errors.Is(err, context.Canceled) || !receipt.Created || fake.creates != 1 {
		diagnostic := ""
		if err != nil {
			diagnostic = err.Error()
		}
		if strings.Contains(diagnostic, enrollmentTestPassword) {
			diagnostic = "credential was present in error"
		}
		t.Fatalf("successful create interrupted by cancellation did not preserve its verified receipt and stop: created=%v creates=%d err=%s", receipt.Created, fake.creates, diagnostic)
	}
	if !strings.Contains(fake.file.String(), "ENROLLMENT_SECRET_CREATED") || strings.Contains(fake.file.String(), enrollmentTestPassword) {
		t.Fatal("cancellation report lost verified creation state or leaked credential bytes")
	}
}

func existingEnrollmentSecret(namespace, password string) *unstructured.Unstructured {
	secret := enrollmentSecret(namespace, "sha256:"+strings.Repeat("b", 64), password)
	secret.SetUID(types.UID("secret-existing"))
	secret.SetResourceVersion("17")
	return secret
}

func secretObjectEqual(a, b *unstructured.Unstructured) bool {
	left, _ := json.Marshal(a.Object)
	right, _ := json.Marshal(b.Object)
	return string(left) == string(right)
}

var _ ResourceClient = (*enrollmentSecretFake)(nil)
