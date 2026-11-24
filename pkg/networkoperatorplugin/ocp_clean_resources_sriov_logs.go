// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RH 4.20 uses controller-runtime reconcileIDs; RH 4.18 has one serialized
// nodeStateSyncHandler worker per node. Both log the fetched generation before
// applying it. Only the matching handler success is proof; its generic queue
// success message can also describe skipped or incomplete reconciliation.
// The caller binds logs to one stable current Pod/container and NodeState UID,
// then rechecks the API generation and all selected-PF postconditions.
func ocpSriovLogProvesGeneration(data []byte, state *unstructured.Unstructured, containerStart time.Time) bool {
	if sriovLogProvesGeneration(data, state, containerStart) {
		return true
	}
	boundary := state.GetCreationTimestamp().Time
	if containerStart.After(boundary) {
		boundary = containerStart
	}
	captured := false
	for _, raw := range bytes.Split(data, []byte("\n")) {
		timestamp, rest, ok := strings.Cut(string(raw), " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || at.Before(boundary) {
			continue
		}
		fields := map[string]interface{}{}
		prefix := strings.TrimSpace(rest)
		if start := strings.IndexByte(rest, '{'); start >= 0 {
			decoder := json.NewDecoder(strings.NewReader(rest[start:]))
			decoder.UseNumber()
			if decoder.Decode(&fields) != nil {
				continue
			}
			prefix = strings.TrimSpace(rest[:start])
		}
		message, _ := fields["msg"].(string)
		if message == "" {
			message, _ = fields["message"].(string)
		}
		isMessage := func(want string) bool { return message == want || strings.HasSuffix(prefix, want) }
		if isMessage("nodeStateSyncHandler(): new generation") {
			generation, ok := fields["generation"].(json.Number)
			value, err := generation.Int64()
			captured = ok && err == nil && value == state.GetGeneration()
		}
		if isMessage("nodeStateSyncHandler(): sync succeeded") && captured {
			return true
		}
	}
	return false
}
