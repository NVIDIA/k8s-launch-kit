// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnRevisionCheck(sw inventory.FabricSwitch) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.fabric-revision", Subject: sw.Name, Vantage: "executor", RequiredPhase: "deploy",
		State: "unknown", ReasonCode: "NVUE_REVISION_UNAVAILABLE", Message: "Stable active and saved NVUE application identities have not been observed; verify read access to nv config revision and rerun discovery."}
	revision := sw.Snapshot.Revision
	if revision == nil || strings.TrimSpace(revision.AppliedApplyID) == "" || strings.TrimSpace(revision.SavedApplyID) == "" {
		for _, issue := range sw.Snapshot.Issues {
			if strings.HasPrefix(issue.Input, "revision") {
				check.Message = issue.Message
				if issue.Code == "revision_confirmation_pending" {
					check.State = "failed"
					check.ReasonCode = "NVUE_REVISION_UNCONFIRMED"
				}
				break
			}
		}
		return check
	}
	if revision.AppliedApplyID != revision.SavedApplyID {
		check.State = "failed"
		check.ReasonCode = "NVUE_ACTIVE_REVISION_NOT_SAVED"
		check.Message = fmt.Sprintf("Active NVUE application %s differs from saved application %s; complete the switch configuration save/confirmation and rerun discovery.", revision.AppliedApplyID, revision.SavedApplyID)
		return check
	}
	check.State = "passed"
	check.ReasonCode = "NVUE_REVISION_STABLE_AND_SAVED"
	check.Message = fmt.Sprintf("NVUE active application %s is stable across switch collection and matches one saved-history record; startup-file contents and reboot persistence are not measured.", revision.AppliedApplyID)
	return check
}
