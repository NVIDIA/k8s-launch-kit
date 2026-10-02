// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
)

type junitInput struct {
	Data            connectivity.ReportData
	Fabric          string
	RequireCoverage bool
	Configured      bool
	Enabled         bool
	Checks          []connectivity.Check
	Duration        time.Duration
	RunError        error
}

type junitDocument struct {
	XMLName xml.Name     `xml:"testsuites"`
	Name    string       `xml:"name,attr"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     string      `xml:"time,attr,omitempty"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
	Out       string        `xml:"system-out,omitempty"`
	Err       string        `xml:"system-err,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
}

// Names are the catalog names in Network Test Discussion Notes, tab
// t.ph7wskwsjqbn. Deployment type and same/cross rail are subtest details.
var junitFamilies = []struct {
	Check connectivity.Check
	Name  string
}{
	{connectivity.CheckICMP, "K8sEastWestNetworkICMPPing"},
	{connectivity.CheckRPing, "K8sEastWestNetworkRDMAPing"},
	{connectivity.CheckIBWriteBW, "K8sEastWestNetworkIBWriteBandwidth"},
	{connectivity.CheckGPUDirectDMABuf, "K8sEastWestNetworkDMABufBandwidth"},
}

func junitFabric(cfg *config.LaunchKitConfig) (string, error) {
	fabric := ""
	if cfg != nil && cfg.Profile != nil {
		fabric = strings.ToLower(strings.TrimSpace(cfg.Profile.Fabric))
	}
	if fabric != "ethernet" && fabric != "infiniband" {
		return "", fmt.Errorf("profile.fabric must be ethernet or infiniband, got %q", fabric)
	}
	return fabric, nil
}

func junitCheck(kind connectivity.PingTestKind) connectivity.Check {
	switch {
	case kind.IsICMP():
		return connectivity.CheckICMP
	case kind.IsRDMAPing():
		return connectivity.CheckRPing
	case kind.IsRDMABw():
		return connectivity.CheckIBWriteBW
	case kind.IsGPUDirectDMABuf():
		return connectivity.CheckGPUDirectDMABuf
	default:
		return ""
	}
}

func (s *junitSuite) count() {
	s.Tests = len(s.Cases)
	for _, c := range s.Cases {
		if c.Failure != nil {
			s.Failures++
		}
		if c.Error != nil {
			s.Errors++
		}
		if c.Skipped != nil {
			s.Skipped++
		}
	}
}

func junitJSON(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func junitStatus(name string, passed, skipped bool, reason string, evidence any) junitCase {
	c := junitCase{Name: name, Classname: "network.validation", Out: junitJSON(evidence)}
	if skipped {
		c.Skipped = &junitMessage{Message: reason}
	} else if !passed {
		if reason == "" {
			reason = name + " failed"
		}
		c.Failure = &junitMessage{Message: reason}
	}
	return c
}

func buildJUnit(input junitInput) junitDocument {
	doc := junitDocument{Name: "l8k validation tests"}
	static := junitSuite{Name: "network/validation", Time: "0.000"}
	staticDuration := input.Duration
	if input.Data.Matrix != nil {
		for _, duration := range input.Data.Matrix.StageDurations {
			staticDuration -= duration
		}
	}
	if staticDuration > 0 {
		static.Time = fmt.Sprintf("%.3f", staticDuration.Seconds())
	}
	d := input.Data
	if v := d.Release; v != nil {
		static.Cases = append(static.Cases, junitStatus("NetworkOperatorVersion", v.Match, v.Skipped, v.Reason, v))
	}
	if v := d.ComponentCheck; v != nil {
		static.Cases = append(static.Cases, junitStatus("ComponentVersions", v.AllMatch, v.Skipped, v.Reason, v))
	}
	if v := d.HelmValues; v != nil {
		static.Cases = append(static.Cases, junitStatus("HelmValues", v.AllMatch, v.Skipped, v.Reason, v))
	}
	if v := d.StrayCRs; v != nil {
		static.Cases = append(static.Cases, junitStatus("StrayResources", !v.Failed(), v.Skipped, "Stray resource check", v))
	}
	for _, v := range d.Manifests {
		static.Cases = append(static.Cases, junitStatus(v.Kind+"/"+v.Namespace+"/"+v.Name,
			v.State == crstate.StateSuccess, v.State == crstate.StateInProgress, v.Reason, v))
	}
	for _, match := range d.PresetMatches {
		static.Cases = append(static.Cases, junitStatus("TopologyPresets::"+match.Group,
			match.Status == presetmatch.StatusMatch,
			match.Status == presetmatch.StatusSkipped || match.Status == presetmatch.StatusNotFound,
			match.Reason, match))
	}
	if input.RequireCoverage {
		coverage := connectivityCoverageVerdict(d.Matrix, input.Checks)
		static.Cases = append(static.Cases, junitStatus("ConnectivityCoverage", coverage.Pass, false,
			strings.Join(coverage.Reasons, "; "), coverage))
	}
	selected := make(map[connectivity.Check]bool)
	for _, check := range input.Checks {
		selected[check] = true
	}
	if input.Configured && input.Fabric != "" {
		for _, family := range junitFamilies {
			suite := junitSuite{Name: family.Name + "-" + input.Fabric, Time: "0.000"}
			if d.Matrix != nil {
				if duration, ok := d.Matrix.StageDurations[family.Check]; ok {
					suite.Time = fmt.Sprintf("%.3f", duration.Seconds())
				}
				for _, r := range d.Matrix.PingResults {
					if junitCheck(r.Test.Kind) != family.Check {
						continue
					}
					t := r.Test
					// Include pods and IPs to distinguish multiple endpoints on one node.
					name := fmt.Sprintf("%s::%s/%s[%s,%s]→%s/%s[%s,%s]", suite.Name,
						t.SrcNode, t.SrcRail, t.SrcPod, t.SrcIP, t.DstNode, t.DstRail, t.DstPod, t.DstIP)
					c := junitCase{Name: name, Classname: "network.connectivity", Out: junitJSON(r), Err: r.Stderr}
					// OK is the existing expectation-aware verdict, not observed reachability.
					if !r.OK {
						message := "Connectivity expectation not satisfied"
						if r.Err != nil {
							message = r.Err.Error()
						}
						c.Failure = &junitMessage{Message: message}
					}
					suite.Cases = append(suite.Cases, c)
				}
			}
			if len(suite.Cases) == 0 {
				reason := "Check disabled by validation configuration"
				if input.Enabled && selected[family.Check] {
					reason = "No probes executed; validation prerequisites were not met"
					if d.Matrix != nil && d.Matrix.Skipped != nil {
						reason = d.Matrix.Skipped.Reason
					}
					if input.RunError != nil {
						reason = "Not executed: " + input.RunError.Error()
					}
				}
				suite.Cases = append(suite.Cases, junitCase{Name: suite.Name, Classname: "network.connectivity", Skipped: &junitMessage{Message: reason}})
			}
			sort.SliceStable(suite.Cases, func(i, j int) bool { return suite.Cases[i].Name < suite.Cases[j].Name })
			suite.count()
			doc.Suites = append(doc.Suites, suite)
		}
	} else if input.Configured && !input.Enabled {
		static.Cases = append(static.Cases, junitStatus("Connectivity", false, true, "Connectivity disabled", nil))
	}
	if input.RunError != nil {
		var status *apperrors.ExitStatusError
		if errors.As(input.RunError, &status) {
			// Coverage policies can fail even when all emitted rows passed or skipped.
			// Keep that gate visible without duplicating individual failed probes.
			hasFailure := false
			for _, c := range static.Cases {
				hasFailure = hasFailure || c.Failure != nil || c.Error != nil
			}
			for _, s := range doc.Suites {
				hasFailure = hasFailure || s.Failures+s.Errors > 0
			}
			if !hasFailure {
				static.Cases = append(static.Cases, junitStatus("ValidationOutcome", false, false,
					"Validation failed: required check coverage or acceptance criteria were not satisfied", nil))
			}
		} else {
			static.Cases = append(static.Cases, junitCase{Name: "ValidationExecution", Classname: "network.validation", Error: &junitMessage{Message: input.RunError.Error()}})
		}
	}
	if len(static.Cases) > 0 {
		static.count()
		doc.Suites = append([]junitSuite{static}, doc.Suites...)
	}
	return doc
}

func writeJUnitReport(path string, input junitInput) error {
	doc := buildJUnit(input)
	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JUnit: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".l8k-junit-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append([]byte(xml.Header), append(data, '\n')...)); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return nil
}
