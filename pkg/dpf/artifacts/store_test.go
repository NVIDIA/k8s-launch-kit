// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifacts(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF artifacts") }

var _ = Describe("safe publication", func() {
	var dir string
	var input config.Input
	BeforeEach(func() {
		var e error
		dir, e = os.MkdirTemp("", "dpf-artifacts-")
		Expect(e).NotTo(HaveOccurred())
		dir, e = filepath.EvalSymlinks(dir)
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		source := []byte("# site intent\nmanagement:\n  kubeconfigRef: {file: credentials/mgmt}\ncredentials: {}\ndiscovery:\n  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: r}]\n  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: r}]\n")
		p := filepath.Join(dir, "input.yaml")
		Expect(os.WriteFile(p, source, 0600)).To(Succeed())
		input, e = config.Load(p)
		Expect(e).NotTo(HaveOccurred())
	})
	report := func(complete bool) *inventory.Report {
		outcome := "partial"
		if complete {
			outcome = "complete"
		}
		return &inventory.Report{Inventory: &inventory.Inventory{FormatVersion: "v1alpha1"}, Discovery: inventory.Discovery{Outcome: outcome}, Artifacts: []inventory.Artifact{}}
	}
	It("preserves normal handoff on partial collection and rebases references", func() {
		s, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		r := report(false)
		Expect(s.Publish(context.Background(), r)).To(Succeed())
		_, e = os.Stat(s.Output)
		Expect(os.IsNotExist(e)).To(BeTrue())
		b, e := os.ReadFile(filepath.Join(s.RunDir, "partial.yaml"))
		Expect(e).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring("# site intent"))
		Expect(string(b)).To(ContainSubstring("../../credentials/mgmt"))
		st, e := os.Stat(filepath.Join(s.RunDir, "report.json"))
		Expect(e).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()).To(Equal(os.FileMode(0600)))
	})
	It("omits BMC evidence bookkeeping from complete and partial saved inventories and reports", func() {
		for _, complete := range []bool{true, false} {
			output := filepath.Join(dir, fmt.Sprintf("no-evidence-%t.yaml", complete))
			store, err := New(input, output, true)
			Expect(err).NotTo(HaveOccurred())
			r := report(complete)
			r.Inventory.Hosts = []inventory.Device{{ID: "host-one", ObservationState: "observed", EvidenceRefs: []string{}, Adapters: []inventory.Adapter{{Key: "card-one", SerialNumber: "CARD-ONE", EvidenceRefs: []string{}}}, Firmware: []inventory.Firmware{{Component: "bmc", State: "known", RawVersion: "iLO 6 v1.50", Activation: "unknown", EvidenceRefs: []string{}}}}}
			r.Inventory.DPUs = []inventory.Device{{ID: "dpu-one", SerialNumber: "CARD-ONE", ObservationState: "observed", Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}}, EvidenceRefs: []string{}}}
			r.Inventory.Associations = []inventory.Association{{HostID: "host-one", DPUID: "dpu-one", HostAdapterKey: "card-one", EvidenceRefs: []string{}}}
			r.Inventory.Evidence = map[string]inventory.Evidence{}
			r.Discovery.Checks = []inventory.Check{{ID: "dpf.management.bmc-administrator", State: "passed", RequiredPhase: "discover", ReasonCode: "BMC_ADMIN_VERIFIED", EvidenceRefs: []string{}}}
			Expect(store.Publish(context.Background(), r)).To(Succeed())
			path := output
			if !complete {
				path = filepath.Join(store.RunDir, "partial.yaml")
			}
			data, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			var saved map[string]any
			Expect(yaml.Unmarshal(data, &saved)).To(Succeed())
			Expect(hasEvidenceKey(saved)).To(BeFalse())
			loaded, err := config.Load(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Site.Inventory.Hosts[0].Firmware[0].RawVersion).To(Equal("iLO 6 v1.50"))
			Expect(loaded.Site.Inventory.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
			data, err = os.ReadFile(filepath.Join(store.RunDir, "report.json"))
			Expect(err).NotTo(HaveOccurred())
			var detailed map[string]any
			Expect(json.Unmarshal(data, &detailed)).To(Succeed())
			Expect(hasEvidenceKey(detailed)).To(BeFalse())
		}
	})
	It("refreshes legacy BMC evidence without carrying it forward or changing the backup", func() {
		legacy := &inventory.Inventory{FormatVersion: "v1alpha1", Evidence: map[string]inventory.Evidence{"legacy-ref": {Kind: "redfish", Authenticated: true}}, Hosts: []inventory.Device{{EvidenceRefs: []string{"legacy-ref"}}}}
		data, err := config.Saved(input, legacy, input.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(input.Path, data, 0600)).To(Succeed())
		previous, err := config.Load(input.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(previous.Site.Inventory.Evidence).To(HaveKey("legacy-ref"))
		store, err := New(previous, previous.Path, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Publish(context.Background(), report(true))).To(Succeed())
		fresh, err := os.ReadFile(previous.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fresh)).NotTo(ContainSubstring("legacy-ref"))
		backup, err := os.ReadFile(filepath.Join(store.RunDir, "source-before.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(backup).To(Equal(data))
	})
	It("retains nonempty evidence from other discovery sources", func() {
		inv := &inventory.Inventory{Evidence: map[string]inventory.Evidence{"ssh-ref": {Kind: "ssh", Authenticated: true}}, Hosts: []inventory.Device{{SSHEndpoint: "ssh://192.0.2.10:22", EvidenceRefs: []string{"ssh-ref"}}}}
		data, err := config.Saved(input, inv, filepath.Join(dir, "mixed.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("ssh-ref"))
		encoded, err := json.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).To(ContainSubstring(`"evidenceRefs":["ssh-ref"]`))
	})

	It("creates an in-place source backup before replacement", func() {
		s, e := New(input, input.Path, true)
		Expect(e).NotTo(HaveOccurred())
		Expect(s.Publish(context.Background(), report(true))).To(Succeed())
		b, e := os.ReadFile(filepath.Join(s.RunDir, "source-before.yaml"))
		Expect(e).NotTo(HaveOccurred())
		Expect(b).To(Equal(input.Source))
	})
	It("rejects concurrent source edits before replacing output", func() {
		s, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		Expect(os.WriteFile(input.Path, []byte("changed"), 0600)).To(Succeed())
		Expect(s.Publish(context.Background(), report(true))).NotTo(Succeed())
		_, e = os.Stat(s.Output)
		Expect(os.IsNotExist(e)).To(BeTrue())
	})
	It("rejects existing unrelated output and symlinked run directories", func() {
		out := filepath.Join(dir, "out.yaml")
		Expect(os.WriteFile(out, []byte("unrelated: true"), 0600)).To(Succeed())
		_, e := New(input, out, true)
		Expect(e).To(HaveOccurred())
		Expect(os.Remove(out)).To(Succeed())
		Expect(os.Symlink(dir, filepath.Join(dir, "out.discovery"))).To(Succeed())
		_, e = New(input, out, true)
		Expect(e).To(HaveOccurred())
	})
	It("keeps a stable lock and detects a second cooperating writer", func() {
		a, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		b, e := New(input, "", false)
		Expect(e).NotTo(HaveOccurred())
		Expect(a.Publish(context.Background(), report(true))).To(Succeed())
		Expect(b.Publish(context.Background(), report(true))).NotTo(Succeed())
		_, e = os.Stat(a.Output + ".lock")
		Expect(e).NotTo(HaveOccurred())
	})
	It("checks the destination baseline even when source already contains inventory", func() {
		output := filepath.Join(dir, "prior.yaml")
		prior := &inventory.Inventory{FormatVersion: "v1alpha1", ScopeDigest: "cluster-B"}
		saved, e := config.Saved(input, prior, output)
		Expect(e).NotTo(HaveOccurred())
		Expect(os.WriteFile(output, saved, 0600)).To(Succeed())
		input.Site.Inventory = &inventory.Inventory{FormatVersion: "v1alpha1", ScopeDigest: "cluster-A"}
		store, e := New(input, output, true)
		Expect(e).NotTo(HaveOccurred())
		r := report(true)
		r.Inventory.ScopeDigest = "cluster-A"
		Expect(store.Publish(context.Background(), r)).NotTo(Succeed())
		unchanged, e := os.ReadFile(output)
		Expect(e).NotTo(HaveOccurred())
		Expect(unchanged).To(Equal(saved))
	})
	It("does not overwrite a destination collected with changed or unrecorded Redfish trust", func() {
		for index, priorTrust := range []string{"", "system-roots", "custom-ca-before", "verification-disabled"} {
			output := filepath.Join(dir, []string{"legacy.yaml", "system.yaml", "custom.yaml", "insecure.yaml"}[index])
			prior := &inventory.Inventory{FormatVersion: "v1alpha1", Collection: inventory.Collection{Mode: "inventory"}, RequestedScope: []inventory.ScopeEndpoint{{Role: "hosts", Endpoint: "https://192.0.2.2", TLSTrustDigest: priorTrust}}}
			discovery.Canonicalize(prior)
			saved, err := config.Saved(input, prior, output)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(output, saved, 0600)).To(Succeed())
			store, err := New(input, output, true)
			Expect(err).NotTo(HaveOccurred())
			r := report(true)
			r.Inventory.Collection.Mode = "inventory"
			r.Inventory.RequestedScope = []inventory.ScopeEndpoint{{Role: "hosts", Endpoint: "https://192.0.2.2", TLSTrustDigest: "custom-ca-after"}}
			discovery.Canonicalize(r.Inventory)
			Expect(store.Publish(context.Background(), r)).To(MatchError(ContainSubstring("conflicts with resolved scope")))
			unchanged, err := os.ReadFile(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(unchanged).To(Equal(saved))
		}
	})
	It("preserves a legacy destination even when a fresh source would otherwise permit publication", func() {
		output := filepath.Join(dir, "legacy.yaml")
		key := inventory.Digest("dpf-adapter-v1", []string{"host", "CARD-ONE"})
		prior := &inventory.Inventory{
			FormatVersion: "v1alpha1",
			Hosts:         []inventory.Device{{ID: "host", Adapters: []inventory.Adapter{{Key: key, SerialNumber: "CARD-ONE"}}}},
			DPUs:          []inventory.Device{{ID: "card", ReaderAdapter: "host-card-v1", RecordKey: inventory.Digest("dpf-host-card-v1", []string{"host", key})}},
		}
		saved, err := config.Saved(input, prior, output)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(output, saved, 0600)).To(Succeed())
		store, err := New(input, output, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Publish(context.Background(), report(true))).To(MatchError(ContainSubstring("legacy Redfish card bindings")))
		unchanged, err := os.ReadFile(output)
		Expect(err).NotTo(HaveOccurred())
		Expect(unchanged).To(Equal(saved))
	})

})

// Inspect parsed documents so scalar diagnostic text never resembles a field match.
func hasEvidenceKey(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if strings.EqualFold(key, "evidence") || strings.EqualFold(key, "evidenceRef") || strings.EqualFold(key, "evidenceRefs") || hasEvidenceKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasEvidenceKey(child) {
				return true
			}
		}
	}
	return false
}
