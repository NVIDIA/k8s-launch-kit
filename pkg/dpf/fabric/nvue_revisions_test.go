// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"strings"
	"testing"
)

func revisionTable(activeID, savedID string, extraRows ...string) []byte {
	rows := []string{
		"Rev ID   State              Apply ID             Apply Date           Type  User     Reason         Message",
		"-------  -----------------  -------------------  -------------------  ----  -------  -------------  ------------------------------------------------------",
		"43       previous           rev_43_apply_1       2026-10-11 21:05:19  CLI   user     Config update  An older revision",
		"44       applied_and_saved  " + savedID + "       2026-11-14 16:38:35  CLI   user     Config update  Saved revision",
		"applied  applied            " + activeID + "       2026-11-14 16:38:35  CLI   user     Config update  Applied revision",
		"empty    inactive",
		"startup  previous           rev_startup_apply_3  2026-08-18 17:50:44  None  user     Config update  Startup revision",
	}
	rows = append(rows[:5], append(extraRows, rows[5:]...)...)
	return []byte(strings.Join(rows, "\n") + "\n")
}

func TestParseNVUERevisionObservation(t *testing.T) {
	stable := revisionTable("rev_45_apply_1", "rev_45_apply_1",
		"46       pending            rev_46_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Harmless staged candidate",
		"                                                                                                     applied applied rev_fake inside a continued message",
	)
	got := ParseNVUE(NVUEInputs{RevisionBefore: stable, RevisionAfter: append([]byte(nil), stable...)})
	if got.Revision == nil || got.Revision.AppliedApplyID != "rev_45_apply_1" || got.Revision.SavedApplyID != "rev_45_apply_1" {
		t.Fatalf("unexpected stable revision observation: %#v", got.Revision)
	}
	if hasIssue(got.Issues, "revision", "$", "revision_changed") {
		t.Fatalf("a harmless pending revision changed the stable applied/saved result: %#v", got.Issues)
	}
}

func TestParseNVUERevisionObservationBlocksUnconfirmedAndChangedState(t *testing.T) {
	unconfirmed := revisionTable("rev_45_apply_1", "rev_44_apply_1")
	got := ParseNVUE(NVUEInputs{RevisionBefore: unconfirmed, RevisionAfter: append([]byte(nil), unconfirmed...)})
	if got.Revision == nil || got.Revision.AppliedApplyID == got.Revision.SavedApplyID {
		t.Fatalf("stable but unconfirmed applied revision was not preserved for readiness to reject: %#v", got.Revision)
	}

	changed := ParseNVUE(NVUEInputs{RevisionBefore: revisionTable("rev_44_apply_1", "rev_44_apply_1"), RevisionAfter: revisionTable("rev_45_apply_1", "rev_45_apply_1")})
	if changed.Revision != nil || !hasIssue(changed.Issues, "revision", "$", "revision_changed") {
		t.Fatalf("revision change during collection was not blocked: revision=%#v issues=%#v", changed.Revision, changed.Issues)
	}
}

func TestParseNVUERevisionObservationMatchesActiveSavedHistory(t *testing.T) {
	// NVUE can retain several historical applied_and_saved rows. Their numeric
	// order is not the current application identity: an old revision can be reapplied.
	stable := revisionTable("rev_45_apply_1", "rev_45_apply_1",
		"99       applied_and_saved  rev_99_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Historical saved revision",
	)
	got := ParseNVUE(NVUEInputs{RevisionCollectionRequested: true, RevisionBefore: stable, RevisionAfter: stable})
	if got.Revision == nil || got.Revision.AppliedApplyID != "rev_45_apply_1" || got.Revision.SavedApplyID != "rev_45_apply_1" {
		t.Fatalf("active application was not matched to its saved history: revision=%#v issues=%#v", got.Revision, got.Issues)
	}
}

func TestParseNVUERevisionObservationRejectsConfirmationWindow(t *testing.T) {
	// Captured NVUE behavior: while a new revision awaits confirmation, the
	// applied alias can still refer to the preceding applied_and_saved record.
	unconfirmed := revisionTable("rev_44_apply_1", "rev_44_apply_1", "45       confirm                                                                                    Unconfirmed change")
	got := ParseNVUE(NVUEInputs{RevisionCollectionRequested: true, RevisionBefore: unconfirmed, RevisionAfter: unconfirmed})
	if got.Revision != nil || !hasIssue(got.Issues, "revisionBefore", "$", "revision_confirmation_pending") {
		t.Fatalf("an older saved alias masked an unconfirmed change: revision=%#v issues=%#v", got.Revision, got.Issues)
	}
}

func TestParseNVUERevisionObservationRejectsMalformedOrAmbiguousTables(t *testing.T) {
	valid := revisionTable("rev_45_apply_1", "rev_45_apply_1")
	duplicateApplied := revisionTable("rev_45_apply_1", "rev_45_apply_1", "applied  applied            rev_fake_apply_1")
	ambiguousSaved := revisionTable("rev_45_apply_1", "rev_44_apply_1", "46       applied_and_saved  rev_46_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Neither saved application matches active")
	duplicateID := revisionTable("rev_45_apply_1", "rev_45_apply_1", "44       applied_and_saved  rev_45_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Duplicate revision identity")
	applying := revisionTable("rev_45_apply_1", "rev_45_apply_1", "46       applying           rev_46_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Application in progress")
	unknownState := revisionTable("rev_45_apply_1", "rev_45_apply_1", "46       unsupported        rev_46_apply_1       2026-11-14 17:00:00  CLI   user     Config update  Unknown transition")
	shortTable := []byte("Rev ID   State              Apply ID\n-------  -----------------  -------------------\n44       applied_and_saved  rev_000000000_apply_1\napplied  applied            rev_000000000_apply_2\n")
	badHeader := []byte(strings.Replace(string(valid), "Rev ID   State", "Rev ID   Status", 1))
	noSaved := []byte(strings.Replace(string(valid), "44       applied_and_saved  rev_45_apply_1", "44       previous           rev_45_apply_1", 1))
	blankSemantic := revisionTable("rev_45_apply_1", "rev_45_apply_1", "         applied            rev_fake_apply_1")
	overflow := revisionTable("rev_000000000_apply_1", "rev_000000000_apply_2")
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "bad header", data: badHeader},
		{name: "duplicate active row", data: duplicateApplied},
		{name: "ambiguous saved history", data: ambiguousSaved},
		{name: "duplicate revision identity", data: duplicateID},
		{name: "application in progress", data: applying},
		{name: "unsupported revision state", data: unknownState},
		{name: "unsupported short layout cannot truncate application identities", data: shortTable},
		{name: "missing saved row", data: noSaved},
		{name: "blank id with semantic fields", data: blankSemantic},
		{name: "apply id overflows its declared column", data: overflow},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ParseNVUE(NVUEInputs{RevisionCollectionRequested: true, RevisionBefore: test.data, RevisionAfter: test.data})
			if got.Revision != nil || !hasIssue(got.Issues, "revisionBefore", "$", "missing_output") && !hasIssue(got.Issues, "revisionBefore", "$", "malformed_revision") {
				t.Fatalf("malformed revision table was not retained as an issue: revision=%#v issues=%#v", got.Revision, got.Issues)
			}
		})
	}
}
