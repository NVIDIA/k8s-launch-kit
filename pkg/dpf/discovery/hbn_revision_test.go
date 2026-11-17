// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN stable switch application prerequisite", func() {
	It("retains required unknown, mismatch and stable saved outcomes without mutation", func() {
		for _, test := range []struct {
			name        string
			revision    *fabric.NVUERevisionState
			state, code string
		}{
			{name: "unmeasured", state: "unknown", code: "NVUE_REVISION_UNAVAILABLE"},
			{name: "empty identities", revision: &fabric.NVUERevisionState{}, state: "unknown", code: "NVUE_REVISION_UNAVAILABLE"},
			{name: "active differs from saved", revision: &fabric.NVUERevisionState{AppliedApplyID: "rev_2_apply_1", SavedApplyID: "rev_1_apply_1"}, state: "failed", code: "NVUE_ACTIVE_REVISION_NOT_SAVED"},
			{name: "stable and saved", revision: &fabric.NVUERevisionState{AppliedApplyID: "rev_2_apply_1", SavedApplyID: "rev_2_apply_1"}, state: "passed", code: "NVUE_REVISION_STABLE_AND_SAVED"},
		} {
			By(test.name)
			sw := inventory.FabricSwitch{Name: "leaf-a", Snapshot: fabric.Snapshot{Revision: test.revision}}
			before, err := json.Marshal(sw)
			Expect(err).NotTo(HaveOccurred())
			check := hbnRevisionCheck(sw)
			Expect(check.State).To(Equal(test.state))
			Expect(check.ReasonCode).To(Equal(test.code))
			Expect(check.RequiredPhase).To(Equal("deploy"))
			Expect(check.Message).NotTo(BeEmpty())
			after, err := json.Marshal(sw)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
		}
	})
	It("keeps a changed or unreadable application observation required in use-case readiness", func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		inv := inventory.Inventory{HBN: &inventory.HBNInventory{Profile: profile, Switches: []inventory.FabricSwitch{{Name: "leaf-a", Snapshot: fabric.Snapshot{
			Issues: []fabric.Issue{{Input: "revision", Code: "revision_changed", Message: "NVUE applied or saved revision identity changed during collection."}},
		}}}}}
		readiness := evaluateHBN(&inv, nil, nil)
		Expect(readiness.State).To(Equal("blocked"))
		Expect(readiness.Checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.fabric-revision"), HaveField("State", "unknown"), HaveField("RequiredPhase", "deploy"), HaveField("Message", ContainSubstring("changed during collection")))))
	})
	It("blocks an unconfirmed change even while the prior saved alias still matches", func() {
		sw := inventory.FabricSwitch{Name: "leaf-a", Snapshot: fabric.Snapshot{Issues: []fabric.Issue{{Input: "revisionBefore", Code: "revision_confirmation_pending", Message: "An NVUE revision is awaiting confirmation."}}}}
		check := hbnRevisionCheck(sw)
		Expect(check.State).To(Equal("failed"))
		Expect(check.ReasonCode).To(Equal("NVUE_REVISION_UNCONFIRMED"))
		Expect(check.Message).To(ContainSubstring("awaiting confirmation"))
	})
})
