// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestManagerSpecAcceptsAPIDefaultsWithoutChangingInput(t *testing.T) {
	objects, err := decodeOperatorManifest([]byte(`apiVersion: apps/v1
kind: Deployment
spec:
  replicas: 1
  selector:
    matchLabels: {app: manager}
  template:
    metadata:
      labels: {app: manager}
    spec:
      serviceAccountName: manager
      containers:
      - name: manager
        image: example/manager:v1
        imagePullPolicy: IfNotPresent
        resources: null
        readinessProbe:
          httpGet: {path: /readyz, port: 8081}
      volumes:
      - name: token
        projected:
          sources:
          - serviceAccountToken: {path: token}
`))
	if err != nil || len(objects) != 1 {
		t.Fatalf("decode fixture: %v", err)
	}
	desired := objects[0]
	before := desired.DeepCopy()
	current := desired.DeepCopy()
	for _, field := range []struct {
		path  []string
		value any
	}{
		{[]string{"strategy"}, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"}}},
		{[]string{"revisionHistoryLimit"}, int64(10)},
		{[]string{"progressDeadlineSeconds"}, int64(600)},
		{[]string{"template", "metadata", "creationTimestamp"}, nil},
		{[]string{"template", "spec", "dnsPolicy"}, "ClusterFirst"},
		{[]string{"template", "spec", "restartPolicy"}, "Always"},
		{[]string{"template", "spec", "schedulerName"}, "default-scheduler"},
		{[]string{"template", "spec", "securityContext"}, map[string]any{}},
		{[]string{"template", "spec", "terminationGracePeriodSeconds"}, int64(30)},
		{[]string{"template", "spec", "serviceAccount"}, "manager"},
	} {
		if err := unstructured.SetNestedField(current.Object, field.value, append([]string{"spec"}, field.path...)...); err != nil {
			t.Fatal(err)
		}
	}
	pod := current.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	container := pod["containers"].([]any)[0].(map[string]any)
	container["terminationMessagePath"], container["terminationMessagePolicy"] = "/dev/termination-log", "File"
	container["resources"] = map[string]any{}
	probe := container["readinessProbe"].(map[string]any)
	probe["timeoutSeconds"], probe["periodSeconds"], probe["successThreshold"], probe["failureThreshold"] = int64(1), int64(10), int64(1), int64(3)
	probe["httpGet"].(map[string]any)["scheme"] = "HTTP"
	projected := pod["volumes"].([]any)[0].(map[string]any)["projected"].(map[string]any)
	projected["defaultMode"] = int64(420)
	projected["sources"].([]any)[0].(map[string]any)["serviceAccountToken"].(map[string]any)["expirationSeconds"] = int64(3600)
	if !managerDeploymentIntentMatches(desired, current) {
		t.Fatal("API-defaulted manager spec did not match retained intent")
	}
	if !jsonEqual(before.Object, desired.Object) {
		t.Fatal("comparison changed retained intent")
	}
	for name, mutate := range map[string]func(map[string]any){
		"environment": func(p map[string]any) {
			p["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "KUBECONFIG", "value": "/other"}}
		},
		"envFrom": func(p map[string]any) {
			p["containers"].([]any)[0].(map[string]any)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "foreign"}}}
		},
		"mount": func(p map[string]any) {
			p["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"name": "token", "mountPath": "/operator"}}
		},
		"privileged": func(p map[string]any) {
			p["containers"].([]any)[0].(map[string]any)["securityContext"] = map[string]any{"privileged": true}
		},
		"hostNetwork": func(p map[string]any) { p["hostNetwork"] = true },
		"lifecycle": func(p map[string]any) {
			p["containers"].([]any)[0].(map[string]any)["lifecycle"] = map[string]any{"postStart": map[string]any{"exec": map[string]any{"command": []any{"sh", "-c", "unexpected"}}}}
		},
		"unknown":    func(p map[string]any) { p["futureExecutionSetting"] = "unrecognized" },
		"pullPolicy": func(p map[string]any) { p["containers"].([]any)[0].(map[string]any)["imagePullPolicy"] = "Never" },
		"tokenAudience": func(p map[string]any) {
			p["volumes"].([]any)[0].(map[string]any)["projected"].(map[string]any)["sources"].([]any)[0].(map[string]any)["serviceAccountToken"].(map[string]any)["audience"] = "foreign"
		},
	} {
		t.Run(name, func(t *testing.T) {
			observed := current.DeepCopy()
			mutate(observed.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any))
			if managerDeploymentIntentMatches(desired, observed) {
				t.Fatal("changed manager runtime configuration was accepted")
			}
		})
	}
}

func TestManagerSpecNormalizesServerResourcePrecision(t *testing.T) {
	objects, err := decodeOperatorManifest([]byte(`apiVersion: apps/v1
kind: Deployment
spec:
  selector:
    matchLabels: {app: manager}
  template:
    spec:
      containers:
      - name: manager
        image: example/manager:v1
        resources:
          requests: {cpu: "0.0001"}
`))
	if err != nil || len(objects) != 1 {
		t.Fatalf("decode fixture: %v", err)
	}
	desired := objects[0]
	current := desired.DeepCopy()
	container := current.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	requests := container["resources"].(map[string]any)["requests"].(map[string]any)
	requests["cpu"] = "1m"
	if !managerDeploymentIntentMatches(desired, current) {
		t.Fatal("equivalent server-rounded CPU request was rejected")
	}
	requests["cpu"] = "2m"
	if managerDeploymentIntentMatches(desired, current) {
		t.Fatal("changed CPU request was accepted")
	}
}
