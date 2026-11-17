// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var nvueRevisionApplyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var errNVUERevisionUnconfirmed = errors.New("an NVUE revision is awaiting confirmation; confirm or roll back that change before discovery")

// NVUERevisionState records the explicit active application and its matching
// saved application, or a single distinct saved application for mismatch
// reporting. Historical revision numbers never select the active application.
type NVUERevisionState struct {
	AppliedApplyID string `json:"appliedApplyId" yaml:"appliedApplyId"`
	SavedApplyID   string `json:"savedApplyId" yaml:"savedApplyId"`
}

type nvueRevisionColumn struct {
	start int
	end   int
}

func parseNVUERevisionObservation(before, after []byte, snapshot *Snapshot) {
	beforeState, beforeErr := parseNVUERevisionTable(before)
	if beforeErr != nil {
		addIssue(&snapshot.Issues, "revisionBefore", "$", revisionIssueCode(before, beforeErr), "Cannot establish the pre-collection NVUE revision: "+beforeErr.Error()+".")
	}
	afterState, afterErr := parseNVUERevisionTable(after)
	if afterErr != nil {
		addIssue(&snapshot.Issues, "revisionAfter", "$", revisionIssueCode(after, afterErr), "Cannot establish the post-collection NVUE revision: "+afterErr.Error()+".")
	}
	if beforeErr != nil || afterErr != nil {
		return
	}
	if beforeState != afterState {
		addIssue(&snapshot.Issues, "revision", "$", "revision_changed", "NVUE applied or saved revision identity changed during collection.")
		return
	}
	snapshot.Revision = &beforeState
}

func revisionIssueCode(raw []byte, err error) string {
	if errors.Is(err, errNVUERevisionUnconfirmed) {
		return "revision_confirmation_pending"
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "missing_output"
	}
	return "malformed_revision"
}

func parseNVUERevisionTable(raw []byte) (NVUERevisionState, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return NVUERevisionState{}, fmt.Errorf("empty revision table")
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	headerIndex := -1
	var columns []nvueRevisionColumn
	for index := 0; index+1 < len(lines); index++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[index]), "Rev ID") {
			continue
		}
		columns = revisionColumns(lines[index+1])
		// The following Apply Date column also bounds the Apply ID's trailing
		// separator. A short layout could otherwise hide an overflowing ID.
		if len(columns) < 4 || revisionHeaderField(lines[index], columns[0]) != "Rev ID" ||
			revisionHeaderField(lines[index], columns[1]) != "State" || revisionHeaderField(lines[index], columns[2]) != "Apply ID" ||
			revisionHeaderField(lines[index], columns[3]) != "Apply Date" {
			return NVUERevisionState{}, fmt.Errorf("unexpected revision table columns")
		}
		headerIndex = index
		break
	}
	if headerIndex < 0 {
		return NVUERevisionState{}, fmt.Errorf("revision table header is absent")
	}

	state := NVUERevisionState{}
	seen := map[string]bool{}
	var savedApplyIDs []string
	for _, line := range lines[headerIndex+2:] {
		// Values cannot overflow their declared columns: truncating an apply
		// identity could make two different applications compare equal.
		for column := 0; column < 3; column++ {
			start := columns[column].end
			end := min(columns[column+1].start, len(line))
			if start < end && strings.TrimSpace(line[start:end]) != "" {
				return NVUERevisionState{}, fmt.Errorf("revision field overflows its table column")
			}
		}
		id := revisionColumn(line, columns[0])
		rowState := revisionColumn(line, columns[1])
		applyID := revisionColumn(line, columns[2])
		if id == "" {
			// A wrapped Message has no values in the first three columns.
			if rowState != "" || applyID != "" {
				return NVUERevisionState{}, fmt.Errorf("revision row has fields without an identity")
			}
			continue
		}
		if seen[id] {
			return NVUERevisionState{}, fmt.Errorf("revision identity appears more than once")
		}
		seen[id] = true
		if id != "applied" && id != "startup" && id != "empty" {
			if _, err := strconv.ParseUint(id, 10, 32); err != nil {
				return NVUERevisionState{}, fmt.Errorf("revision identity is unsupported")
			}
		}
		if applyID != "" && !nvueRevisionApplyID.MatchString(applyID) {
			return NVUERevisionState{}, fmt.Errorf("revision application identity is malformed")
		}
		switch {
		case id == "applied":
			if rowState != "applied" || !nvueRevisionApplyID.MatchString(applyID) {
				return NVUERevisionState{}, fmt.Errorf("active revision row is malformed")
			}
			state.AppliedApplyID = applyID
		case rowState == "applied_and_saved":
			if _, err := strconv.ParseUint(id, 10, 32); err != nil || !nvueRevisionApplyID.MatchString(applyID) {
				return NVUERevisionState{}, fmt.Errorf("saved revision row is malformed")
			}
			savedApplyIDs = append(savedApplyIDs, applyID)
		case rowState == "previous" || rowState == "inactive" || rowState == "pending":
			// These known non-active states cannot supersede the applied alias.
		case rowState == "confirm":
			return NVUERevisionState{}, errNVUERevisionUnconfirmed
		default:
			// In-progress or unsupported states must not leave an older applied
			// alias looking stable. A new output format needs a reviewed adapter.
			return NVUERevisionState{}, fmt.Errorf("revision state is unsupported or transitional")
		}
	}
	if state.AppliedApplyID == "" || len(savedApplyIDs) == 0 {
		return NVUERevisionState{}, fmt.Errorf("revision table lacks an explicit active or saved application")
	}
	for _, savedID := range savedApplyIDs {
		if savedID == state.AppliedApplyID {
			if state.SavedApplyID != "" {
				return NVUERevisionState{}, fmt.Errorf("active application has duplicate saved records")
			}
			state.SavedApplyID = savedID
		}
	}
	if state.SavedApplyID == "" {
		if len(savedApplyIDs) != 1 {
			return NVUERevisionState{}, fmt.Errorf("saved application is ambiguous without an active match")
		}
		state.SavedApplyID = savedApplyIDs[0]
	}
	return state, nil
}

func revisionColumns(separator string) []nvueRevisionColumn {
	columns := make([]nvueRevisionColumn, 0, 8)
	for index := 0; index < len(separator); {
		if separator[index] != '-' {
			index++
			continue
		}
		start := index
		for index < len(separator) && separator[index] == '-' {
			index++
		}
		if index-start < 2 {
			return nil
		}
		columns = append(columns, nvueRevisionColumn{start: start, end: index})
	}
	return columns
}

func revisionHeaderField(header string, column nvueRevisionColumn) string {
	if column.start >= len(header) {
		return ""
	}
	end := min(column.end, len(header))
	return strings.TrimSpace(header[column.start:end])
}

func revisionColumn(line string, column nvueRevisionColumn) string {
	if column.start >= len(line) {
		return ""
	}
	end := min(column.end, len(line))
	return strings.TrimSpace(line[column.start:end])
}
