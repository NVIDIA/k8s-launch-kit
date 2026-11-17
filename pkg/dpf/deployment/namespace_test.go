// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type namespaceTestAPI struct {
	t         *testing.T
	file      *executionTestFile
	current   *unstructured.Unstructured
	getErr    error
	createErr error
	race      bool
	gets      int
	creates   int
}

func (c *namespaceTestAPI) Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.current == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "dpf")
	}
	return c.current.DeepCopy(), nil
}

func (c *namespaceTestAPI) Create(_ context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.creates++
	if c.file.syncs != 1 || !strings.Contains(c.file.String(), `"state":"attempted"`) || dryRun {
		c.t.Fatal("namespace creation preceded durable attempt recording")
	}
	if !reflect.DeepEqual(desired.Object, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "dpf"}}) {
		c.t.Fatal("namespace create introduced extra policy or ownership")
	}
	c.current = activeTestNamespace()
	if c.race {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "dpf")
	}
	if c.createErr != nil {
		return nil, c.createErr
	}
	return c.current.DeepCopy(), nil
}

func activeTestNamespace() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "dpf", "uid": "namespace-uid", "resourceVersion": "1", "labels": map[string]any{"site-policy": "preserve"}},
		"status":   map[string]any{"phase": "Active"},
	}}
}

func TestExecuteNamespaceSharedInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existing    bool
		mutate      func(*unstructured.Unstructured)
		getErr      error
		createErr   error
		race        bool
		failSync    int
		wantCreates int
		wantGets    int
		wantSuccess bool
		wantState   string
	}{
		{name: "reuse shared namespace", existing: true, wantGets: 1, wantSuccess: true, wantState: "confirmed"},
		{name: "create missing namespace", wantCreates: 1, wantGets: 1, wantSuccess: true, wantState: "confirmed"},
		{name: "concurrent creation", race: true, wantCreates: 1, wantGets: 2, wantSuccess: true, wantState: "confirmed"},
		{name: "terminating namespace", existing: true, mutate: func(o *unstructured.Unstructured) { now := metav1.Now(); o.SetDeletionTimestamp(&now) }, wantGets: 1, wantState: "failed"},
		{name: "missing phase", existing: true, mutate: func(o *unstructured.Unstructured) { delete(o.Object, "status") }, wantGets: 1, wantState: "failed"},
		{name: "missing UID", existing: true, mutate: func(o *unstructured.Unstructured) { o.SetUID("") }, wantGets: 1, wantState: "failed"},
		{name: "wrong identity", existing: true, mutate: func(o *unstructured.Unstructured) { o.SetName("other") }, wantGets: 1, wantState: "failed"},
		{name: "forbidden is not absent", getErr: apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "dpf", errors.New("secret-value")), wantGets: 1, wantState: "failed"},
		{name: "lost create response", createErr: errors.New("secret-value"), wantCreates: 1, wantGets: 1, wantState: "uncertain"},
		{name: "attempt write fails", failSync: 1},
		{name: "result write fails", failSync: 2, wantCreates: 1, wantGets: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			file := &executionTestFile{failAt: tc.failSync}
			journal := &executionJournal{file: file, digest: "sha256:" + strings.Repeat("a", 64)}
			client := &namespaceTestAPI{t: t, file: file, getErr: tc.getErr, createErr: tc.createErr, race: tc.race}
			if tc.existing {
				client.current = activeTestNamespace()
				if tc.mutate != nil {
					tc.mutate(client.current)
				}
			}
			before := client.current.DeepCopy()
			result, err := executeNamespace(ctx, client, "dpf", journal)
			if (err == nil) != tc.wantSuccess || client.gets != tc.wantGets || client.creates != tc.wantCreates {
				t.Fatalf("err=%v gets=%d creates=%d", err, client.gets, client.creates)
			}
			if tc.existing && !reflect.DeepEqual(client.current, before) {
				t.Fatal("shared namespace was mutated")
			}
			if tc.wantSuccess && (result == nil || result.GetUID() == "") {
				t.Fatal("missing namespace identity")
			}
			if tc.wantState != "" && !strings.Contains(file.String(), `"state":"`+tc.wantState+`"`) {
				t.Fatalf("missing state %s: %s", tc.wantState, file.String())
			}
			if strings.Contains(file.String(), "secret-value") || err != nil && strings.Contains(err.Error(), "secret-value") {
				t.Fatal("server text leaked")
			}
		})
	}
}

func TestExecuteNamespaceRequiresBoundedValidRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file := &executionTestFile{}
	client := &namespaceTestAPI{t: t, file: file}
	journal := &executionJournal{file: file, digest: "test"}
	for _, request := range []struct {
		ctx  context.Context
		name string
	}{
		{nil, "dpf"}, {context.Background(), "dpf"}, {ctx, "dpf"}, {context.Background(), "invalid.name"},
	} {
		if _, err := executeNamespace(request.ctx, client, request.name, journal); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if client.gets != 0 || client.creates != 0 || file.Len() != 0 {
		t.Fatal("invalid request accessed resources")
	}
}
