// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestResumeHostPrerequisiteUsesOriginalInspectOnlyTransaction(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name  string
		edit  func(*sshprobe.HostNetworkRequest, *sshprobe.HostNetworkReceipt) error
		pass  bool
		calls int
	}{
		{"original complete", func(*sshprobe.HostNetworkRequest, *sshprobe.HostNetworkReceipt) error { return nil }, true, 1},
		{"lost caller result still inspects", func(*sshprobe.HostNetworkRequest, *sshprobe.HostNetworkReceipt) error { return nil }, true, 1},
		{"partial journal", func(_ *sshprobe.HostNetworkRequest, r *sshprobe.HostNetworkReceipt) error {
			r.State = "unknown"
			r.Blocking = true
			return nil
		}, false, 1},
		{"missing file proof", func(_ *sshprobe.HostNetworkRequest, r *sshprobe.HostNetworkReceipt) error {
			r.FileVerified = false
			return nil
		}, false, 1},
		{"missing runtime proof", func(_ *sshprobe.HostNetworkRequest, r *sshprobe.HostNetworkReceipt) error {
			r.RuntimeVerified = false
			return nil
		}, false, 1},
		{"missing journal", func(*sshprobe.HostNetworkRequest, *sshprobe.HostNetworkReceipt) error {
			return errors.New("private-journal-canary")
		}, false, 1},
		{"different physical interface", func(r *sshprobe.HostNetworkRequest, _ *sshprobe.HostNetworkReceipt) error {
			r.Identity.InterfaceName = "other"
			return nil
		}, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			f.inventory.DPUs[0].ObservationState = "observed"
			host := &f.inventory.Hosts[0]
			host.ObservationState = "observed"
			host.Adapters[0].PCIAddress = "0000:21:00.0"
			host.Adapters[0].PCIFunctions = []inventory.PCIFunction{{Address: "0000:21:00.0", Driver: "mlx5_core", Netdevs: []string{"workload0"}}, {Address: "0000:21:00.1", Driver: "mlx5_core", Netdevs: []string{"workload1"}}}
			host.Runtime = &inventory.RuntimeObservation{Interfaces: []inventory.Interface{{Name: "workload0", Index: 10, Addresses: []string{}}, {Name: "workload1", Index: 11, Addresses: []string{}}}, Routes: []inventory.Route{}, Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "workload0", PCIAddress: "0000:21:00.0", PhysicalPortName: "p0", PermanentMAC: "02:00:00:00:00:01"}, {Name: "workload1", PCIAddress: "0000:21:00.1", PhysicalPortName: "p1", PermanentMAC: "02:00:00:00:00:02"}}}, Observations: map[string]inventory.Observation{"interfaces": {State: "observed"}, "network-links": {State: "observed"}, "network-link-details": {State: "observed"}, "routes": {State: "observed"}}}

			op, _ := checkpointFixture(t, f)
			f.prepared.operation = op
			deployment, err := retainedDeploymentObject(execution.verificationInputs())
			if err != nil {
				t.Fatal(err)
			}
			selected, _, err := bindActivationHosts(f.prepared.selected, f.controlPlane.enrollment, f.inventory, f.request.Targets, deployment)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(selected[0].host, selected[0].adapter.Key, selected[0].adapter.SerialNumber)
			if err != nil {
				t.Fatal(err)
			}
			request := sshprobe.HostNetworkRequest{Identity: identity, LeaseBinding: "sha256:" + strings.Repeat("a", 64), MTU: 9000, HostAddressPrefix: "198.51.100.2/29", Gateway: "198.51.100.1", WorkloadCIDR: "198.51.100.0/24"}
			receipt := sshprobe.HostNetworkReceipt{State: "complete", RuntimeVerified: true, FileVerified: true}
			inspectErr := test.edit(&request, &receipt)
			op.state.Hosts[selected[0].item.device.Serial] = operationHost{Request: request}
			if test.name == "original complete" {
				prior := op.state.Hosts[selected[0].item.device.Serial]
				prior.Result = &HostWorkloadNetworkResult{Configuration: receipt}
				op.state.Hosts[selected[0].item.device.Serial] = prior
			}
			before, _ := json.Marshal(op.state.Hosts)
			calls := 0
			checks, err := inspectOwnedResumeHosts(f.ctx, f.prepared, ownedResumePrerequisites{selected: selected}, f.inventory, time.Second, func(_ context.Context, target config.Target, got sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
				calls++
				if target.Endpoint != selected[0].hostTarget.Endpoint || !reflect.DeepEqual(got, request) {
					t.Fatal("inspection replaced the original target/request")
				}
				return receipt, inspectErr
			})
			if (err == nil) != test.pass || calls != test.calls {
				t.Fatalf("checks=%+v err=%v calls=%d", checks, err, calls)
			}
			if test.pass && (len(checks) != 1 || checks[0].State != "passed" || checks[0].ReasonCode != "OWNED_RESUME_HOST_PATH_VERIFIED") {
				t.Fatalf("owned path not assessed: %+v", checks)
			}
			after, _ := json.Marshal(op.state.Hosts)
			if string(before) != string(after) {
				t.Fatal("inspection rewrote durable request/results")
			}
		})
	}
}

func TestResumePrerequisiteFailureRetainsSafeExplicitReason(t *testing.T) {
	var progress strings.Builder
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
	before := []inventory.Check{{ID: "raw", State: "failed", ReasonCode: "DPU_ALREADY_ENROLLED"}}
	result, err := ownedPrerequisiteFailure(ctx, PrerequisiteVerification{Checks: before}, BeforeActivation, "OWNED_RESUME_HOST_TRANSACTION_UNVERIFIED", "Inspect the original host transaction; preserve owned state.", errors.New("private-password-canary"))
	encoded, _ := json.Marshal(result.Checks)
	if err == nil || len(result.Checks) != 2 || len(before) != 1 || result.Checks[1].ReasonCode != "OWNED_RESUME_HOST_TRANSACTION_UNVERIFIED" || strings.Contains(string(encoded)+progress.String()+err.Error(), "private-password-canary") {
		t.Fatalf("failure lost explicit safe provenance: checks=%s err=%v", encoded, err)
	}
}
