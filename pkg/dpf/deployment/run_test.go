// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRunRejectsIncompleteBoundaryAndUnboundedControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request RunRequest
	}{
		{name: "missing bundle"},
		{name: "missing callbacks", request: RunRequest{ReportPath: "report", ProbeJournalPath: "probe"}},
		{name: "invalid control", request: RunRequest{PollInterval: 2 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRunRequest(tc.request); err == nil {
				t.Fatal("incomplete or unbounded run request was accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, RunRequest{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("run without bounded deadline = %v", err)
	}
}

func TestRunDefaultSoftwareLimitAdmitsLargeReleaseWhilePreservingExplicitBound(t *testing.T) {
	// Release media can exceed 4 GiB. Advertise that size without transferring
	// it: the accepted header must reach stream verification, which still fails
	// on this deliberately empty body. A small explicit limit must stop earlier.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt((4<<30)+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, test := range []struct {
		name string
		max  int64
		want string
	}{
		{name: "default admits release media", max: 0, want: "stream ended before verification completed"},
		{name: "explicit smaller cap remains authoritative", max: 1 << 30, want: "exceeds the verification limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, maximum := normalizeRunControls(RunRequest{MaxSoftwareBytes: test.max})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := verifyRegistryStream(ctx, server.Client(), server.URL+"/release.iso", strings.Repeat("a", 64), maximum); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("software header/body boundary error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRunPersistsPrerequisiteFailureBeforeManagementEffects(t *testing.T) {
	api := vacantOperationAPIFixture(t)
	generated, err := generateFixtureBundle(operationRunInputs(t, api))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	reportPath := filepath.Join(root, "run.jsonl")
	probePath := filepath.Join(root, "probe.jsonl")
	marker := errors.New("observer sentinel")
	called := false
	target := config.Target{Role: "dpus", Endpoint: "https://192.0.2.10:443", InputRef: "dpu-input"}
	var request RunRequest
	request = RunRequest{
		Bundle:     generated,
		Access:     config.Request{Targets: []config.Target{target}, Kubeconfig: api.request.Kubeconfig, Context: api.request.Context},
		ReportPath: reportPath, ProbeJournalPath: probePath,
		VerifyPrerequisites: func(ctx context.Context, observed config.Request, plan *generation.Plan, _ PrerequisitePhase) (PrerequisiteVerification, error) {
			called = true
			if ctx == nil || plan == nil || plan.Digest() != generated.Index().BundleDigest {
				t.Fatal("observer did not receive the bounded context and validated retained plan")
			}
			observed.Targets[0].Endpoint = "https://192.0.2.11:443"
			if observed.Targets[0].Endpoint == request.Access.Targets[0].Endpoint {
				t.Fatal("observer request mutation did not take effect in the isolated copy")
			}
			return PrerequisiteVerification{Checks: []inventory.Check{{
				ID: "dpf.hbn.management.caller-rbac", Subject: "dpf-operator-system", Vantage: "management-cluster",
				RequiredPhase: "deploy", State: "unknown", ReasonCode: "PERMISSION_OBSERVATION_INCOMPLETE",
				Message: "The current caller permission check was incomplete.", EvidenceRefs: []string{"internal-only"},
			}}}, marker
		},
		VerifyChartSource: func(context.Context, *generation.Plan) (PreflightCheck, error) {
			t.Fatal("chart-source callback ran before prerequisites")
			return PreflightCheck{}, nil
		},
		Rebooter: runRebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
			t.Fatal("reboot callback ran before prerequisites")
			return ActivationObservation{}, nil
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defaults := preflightGenerationInputs(t).Workload.Generation
	result, runErr := runWithImageResolver(ctx, request, func(ctx context.Context, reference string, timeout time.Duration, transport http.RoundTripper) (hbnPodImageIdentity, error) {
		if reference != defaults.ImageRepository+":"+defaults.ImageTag+"@"+defaults.ImageDigest {
			t.Fatalf("early check changed the release-selected default image reference: %s", reference)
		}
		return retainedTestImageResolver(ctx, reference, timeout, transport)
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "prerequisite observation failed") || !called {
		t.Fatalf("Run = %#v, %v; observer called=%v", result, runErr, called)
	}
	if request.Access.Targets[0].Endpoint != target.Endpoint {
		t.Fatalf("observer mutated caller-owned request: %q", request.Access.Targets[0].Endpoint)
	}
	if result.State != "failed" || result.BundleDigest != generated.Index().BundleDigest || len(result.Checks) < 3 ||
		result.Checks[2].State != "unknown" || result.Checks[2].ReasonCode != "PERMISSION_OBSERVATION_INCOMPLETE" {
		t.Fatalf("failure result did not preserve its safe prerequisite state: %#v", result)
	}
	if len(result.Checks[2].EvidenceRefs) != 0 {
		t.Fatal("run result exposed prerequisite evidence references")
	}
	if result.TrafficValidated {
		t.Fatal("failed run reported traffic validation")
	}
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), marker.Error()) {
		t.Fatal("raw observer error was persisted in the execution report")
	}
	var events []executionEvent
	for _, line := range strings.Split(strings.TrimSpace(string(report)), "\n") {
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) < 2 || events[0].ReasonCode != "EXECUTION_STARTED" || events[len(events)-1].State != "failed" {
		t.Fatalf("execution report lacks durable start/failure records: %#v", events)
	}
	probeReport, err := os.ReadFile(probePath)
	if err != nil || !strings.Contains(string(probeReport), `"receipts":[]`) {
		t.Fatalf("probe journal was not initialized before prerequisites: %q, %v", probeReport, err)
	}
}

func TestRunPrerequisitesPreserveResolvedSwitchAccessAndIsolateInputs(t *testing.T) {
	api := vacantOperationAPIFixture(t)
	generated, err := generateFixtureBundle(operationRunInputs(t, api))
	if err != nil {
		t.Fatal(err)
	}
	for _, authentication := range []string{"password", "private-key"} {
		t.Run(authentication, func(t *testing.T) {
			access, knownHosts, privateKey := resolvedPrerequisiteAccess(t, authentication)
			access.Kubeconfig, access.Context = api.request.Kubeconfig, api.request.Context
			requirePrerequisiteAccessShape(t, access)
			root := t.TempDir()
			called := false
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, runErr := runWithImageResolver(ctx, RunRequest{
				Bundle: generated, Access: access,
				ReportPath: filepath.Join(root, "run.jsonl"), ProbeJournalPath: filepath.Join(root, "probe.jsonl"),
				VerifyPrerequisites: func(_ context.Context, observed config.Request, _ *generation.Plan, _ PrerequisitePhase) (PrerequisiteVerification, error) {
					called = true
					requirePrerequisiteAccessShape(t, observed)
					for _, candidate := range []config.Request{access, observed} {
						encoded, err := json.Marshal(candidate)
						if err != nil {
							t.Fatal("serialize prerequisite request for privacy check")
						}
						requireNoPrerequisiteSecrets(t, encoded, privateKey)
					}
					sw := &observed.Switches[0]
					if sw.Access == nil || sw.Access == access.Switches[0].Access ||
						!bytes.Equal(sw.Access.KnownHosts(), knownHosts) || !bytes.Equal(sw.Access.PrivateKey(), privateKey) {
						t.Fatal("prerequisite observer lost resolved switch trust/key or shares mutable access")
					}
					user, password := sw.Access.Credentials()
					wantPassword := ""
					if authentication == "password" {
						wantPassword = "clone-secret-canary"
					}
					if user != "reader" || password != wantPassword || sw.Access.Sudo != access.Switches[0].Access.Sudo {
						t.Fatal("prerequisite observer lost switch authentication or explicit sudo")
					}
					// Getter results are copies even though immutable access snapshots can share storage.
					trustCopy := sw.Access.KnownHosts()
					trustCopy[0] ^= 1
					if keyCopy := sw.Access.PrivateKey(); len(keyCopy) > 0 {
						keyCopy[0] ^= 1
					}
					for i := range observed.Targets {
						target := &observed.Targets[i]
						if target.SSH != nil {
							u, p := target.SSH.Credentials()
							if u != user || p != password || !bytes.Equal(target.SSH.KnownHosts(), knownHosts) ||
								!bytes.Equal(target.SSH.PrivateKey(), privateKey) {
								t.Fatal("prerequisite observer lost original host SSH snapshot")
							}
							target.SSH.Sudo = !target.SSH.Sudo
						} else if u, p := target.Access.Credentials(); u != "admin" || p != "clone-secret-canary" || !target.Access.InsecureSkipVerify() {
							t.Fatal("prerequisite observer lost original BMC access snapshot")
						}
						target.Endpoint = "observer-only"
					}
					sw.Access.Sudo = !sw.Access.Sudo
					sw.Ports[0].Name = "swp999"
					sw.Endpoint = "observer-only"
					observed.Input.Site.Discovery.Switches[0].Ports[0].Name = "swp998"
					observed.Input.Source[0] = 'X'
					observed.Input.Document.Content[0].Content[0].Value = "observer-only"
					observed.HBN.FabricVRF = "observer-only"
					return PrerequisiteVerification{}, errors.New("stop before management access")
				},
				VerifyChartSource: func(context.Context, *generation.Plan) (PreflightCheck, error) {
					t.Fatal("chart access ran after failed prerequisites")
					return PreflightCheck{}, nil
				},
				Rebooter: runRebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
					t.Fatal("reboot ran after failed prerequisites")
					return ActivationObservation{}, nil
				}),
			}, retainedTestImageResolver)
			if !called || runErr == nil || !strings.Contains(runErr.Error(), "prerequisite observation failed") {
				t.Fatal("run did not stop at the prerequisite observer")
			}
			for _, name := range []string{"run.jsonl", "probe.jsonl"} {
				data, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal("read retained prerequisite journal for privacy check")
				}
				requireNoPrerequisiteSecrets(t, data, privateKey)
			}
			if access.Switches[0].Access.Sudo != (authentication == "private-key") ||
				access.Switches[0].Endpoint != "ssh://leaf.example:22" || access.Switches[0].Ports[0].Name != "swp1" ||
				!bytes.Equal(access.Switches[0].Access.KnownHosts(), knownHosts) || !bytes.Equal(access.Switches[0].Access.PrivateKey(), privateKey) ||
				access.Input.Site.Discovery.Switches[0].Ports[0].Name != "swp1" || access.Input.Source[0] != 'd' ||
				access.Input.Document.Content[0].Content[0].Value != "dpf" || access.HBN.FabricVRF != "default" {
				t.Fatal("observer mutated retained caller switch access, port bindings or request intent")
			}
			for _, target := range access.Targets {
				if target.Endpoint == "observer-only" || target.SSH != nil && target.SSH.Sudo != (authentication == "private-key") {
					t.Fatal("observer mutated retained caller target")
				}
			}
		})
	}
}

func requireNoPrerequisiteSecrets(t *testing.T, data, privateKey []byte) {
	t.Helper()
	if bytes.Contains(data, []byte("clone-secret-canary")) || len(privateKey) > 0 &&
		(bytes.Contains(data, privateKey) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(privateKey)))) {
		t.Fatal("prerequisite request or journal exposed resolved authentication")
	}
}

func TestCloneRequestForPrerequisitesPreservesOptionalRuntimeAccess(t *testing.T) {
	empty, err := cloneRequestForPrerequisites(config.Request{})
	if err != nil || empty.Targets != nil || empty.Switches != nil {
		t.Fatal("empty request lost nil runtime collections")
	}
	original := config.Request{
		Targets: []config.Target{{Endpoint: "https://bmc.example"}},
		Switches: []config.SwitchTarget{
			{Name: "nil-ports"},
			{Name: "empty-ports", Ports: []config.SwitchPort{}},
		},
	}
	cloned, err := cloneRequestForPrerequisites(original)
	if err != nil || len(cloned.Targets) != 1 || len(cloned.Switches) != 2 {
		t.Fatal("request without optional runtime access could not be cloned")
	}
	if cloned.Targets[0].SSH != nil || cloned.Switches[0].Access != nil || cloned.Switches[1].Access != nil ||
		cloned.Switches[0].Ports != nil || cloned.Switches[1].Ports == nil {
		t.Fatal("clone changed absent access or nil versus explicitly empty port bindings")
	}
	cloned.Targets[0].Endpoint = "observer-only"
	cloned.Switches[0].Name = "observer-only"
	if original.Targets[0].Endpoint != "https://bmc.example" || original.Switches[0].Name != "nil-ports" {
		t.Fatal("optional-access clone shares runtime collection storage")
	}
}

func requirePrerequisiteAccessShape(t *testing.T, request config.Request) {
	t.Helper()
	if len(request.Switches) != 1 || request.Switches[0].Access == nil || len(request.Switches[0].Ports) != 1 ||
		len(request.Targets) != 2 || request.HBN == nil || len(request.Input.Source) == 0 ||
		len(request.Input.Site.Discovery.Switches) != 1 || len(request.Input.Site.Discovery.Switches[0].Ports) != 1 ||
		request.Input.Document == nil || len(request.Input.Document.Content) != 1 || request.Input.Document.Content[0] == nil ||
		len(request.Input.Document.Content[0].Content) == 0 || request.Input.Document.Content[0].Content[0] == nil {
		t.Fatal("prerequisite access request has an incomplete resolved shape")
	}
}

func resolvedPrerequisiteAccess(t *testing.T, authentication string) (config.Request, []byte, []byte) {
	t.Helper()
	root := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := []byte(knownhosts.Line([]string{"leaf.example", "host.example"}, key) + "\n")
	if err := os.WriteFile(filepath.Join(root, "known_hosts"), knownHosts, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DPF_CLONE_TEST_PASSWORD", "clone-secret-canary")
	auth := "passwordRef: {env: DPF_CLONE_TEST_PASSWORD}"
	var privateKey []byte
	if authentication == "private-key" {
		der, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		privateKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(filepath.Join(root, "identity"), privateKey, 0600); err != nil {
			t.Fatal(err)
		}
		auth = "privateKeyRef: {file: identity}"
	}
	document := fmt.Sprintf(`dpf: {useCase: hbn-bgp}
credentials:
  ssh: {username: reader, %s}
  bmc: {username: admin, passwordRef: {env: DPF_CLONE_TEST_PASSWORD}}
discovery:
  hosts:
    - credentialRef: ssh
      ssh: {address: host.example, knownHostsFile: known_hosts, sudo: %t}
  dpus:
    - credentialRef: bmc
      bmcEndpoint: https://bmc.example
      tls: {insecureSkipVerify: true}
  switches:
    - name: leaf-a
      credentialRef: ssh
      ssh: {address: leaf.example, knownHostsFile: known_hosts, sudo: %t}
      ports: [{name: swp1, dpuSerial: DPU-A, dpuInterface: p0}]
`, auth, authentication == "private-key", authentication == "private-key")
	in, err := config.Decode([]byte(document), filepath.Join(root, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	access, err := config.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	// Prerequisites must use the resolved snapshots, never reopen/re-resolve files.
	if err := os.Remove(filepath.Join(root, "known_hosts")); err != nil {
		t.Fatal(err)
	}
	if privateKey != nil {
		if err := os.Remove(filepath.Join(root, "identity")); err != nil {
			t.Fatal(err)
		}
	}
	return access, knownHosts, privateKey
}

func TestValidatePrerequisiteVerificationRequiresCompleteDeployChecks(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	intent := plan.Intent()
	current := &inventory.Inventory{
		FormatVersion: inventory.FormatVersion,
		Management: &inventory.Management{
			APIEndpoint: intent.Management.APIEndpoint, SystemNamespaceUID: intent.Management.SystemNamespaceUID,
			TrustDigest: intent.Management.TrustDigest, KubernetesVersion: intent.KubernetesVersion,
		},
	}
	current.DPUs = []inventory.Device{{ID: "dpu-a", RecordKey: "dpu-a", InputRef: "/discovery/dpus/0", ReaderAdapter: "bluefield-card-v1", BMCEndpoint: "https://192.0.2.30", SerialNumber: intent.Devices[0].Serial, ObservationState: "observed"}}
	current.Hosts = []inventory.Device{{ID: "host-a", RecordKey: "host-a", InputRef: "/discovery/hosts/0", ReaderAdapter: "linux-host-ssh-v1", SystemUUID: "12345678-1234-1234-1234-123456789abc", SSHEndpoint: "ssh://192.0.2.20:22", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "card-a", IdentityState: "known", SerialNumber: intent.Devices[0].Serial, PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}, {Address: "0000:21:00.1"}}}}, Runtime: &inventory.RuntimeObservation{VirtualMachine: new(bool), SSHServerAddress: "192.0.2.20", Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "mgmt0", PhysicalPFRoots: []string{"0000:35:00.0"}}}}, Interfaces: []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{"192.0.2.20/24"}}}, Routes: []inventory.Route{{Destination: "192.0.2.0/24", InterfaceIndex: 2, Table: 254}}, Observations: map[string]inventory.Observation{"interfaces": {State: "observed"}, "routes": {State: "observed"}, "host-management-roots": {State: "observed"}}}}}
	current.Associations = []inventory.Association{{DPUID: "dpu-a", HostID: "host-a", HostAdapterKey: "card-a", SerialNumber: intent.Devices[0].Serial}}
	verification := PrerequisiteVerification{Inventory: current, Checks: []inventory.Check{{
		ID: "caller.permission", Subject: "dpf-operator-system", Vantage: "management-cluster", RequiredPhase: "deploy", State: "passed",
	}}}
	required := discovery.HostHBNChecks(current, nil)
	for _, check := range required {
		check.State = "passed"
		check.ReasonCode = "PROVIDER_FIXTURE"
		check.Message = "Complete provider row for the validator unit test."
		verification.Checks = append(verification.Checks, check)
	}
	for _, missing := range required {
		candidate := verification
		candidate.Checks = nil
		for _, check := range verification.Checks {
			if check.ID != missing.ID || check.Subject != missing.Subject || check.Vantage != missing.Vantage {
				candidate.Checks = append(candidate.Checks, check)
			}
		}
		if err := validatePrerequisiteVerification(plan, candidate); err == nil {
			t.Fatalf("omitted required host row accepted: %s/%s", missing.ID, missing.Subject)
		}
	}
	if err := validatePrerequisiteVerification(plan, PrerequisiteVerification{Inventory: current, Checks: verification.Checks[:1]}); err == nil {
		t.Fatal("omitted required physical-root check accepted")
	}
	if err := validatePrerequisiteVerification(plan, verification); err != nil {
		t.Fatalf("valid complete prerequisite observation: %v", err)
	}
	for _, mutate := range []struct {
		name string
		fn   func(*PrerequisiteVerification)
	}{
		{name: "no deploy check", fn: func(v *PrerequisiteVerification) {
			for i := range v.Checks {
				v.Checks[i].RequiredPhase = "generate"
			}
		}},
		{name: "host row cannot change subject", fn: func(v *PrerequisiteVerification) { v.Checks[1].Subject = "foreign host" }},
		{name: "host row cannot change vantage", fn: func(v *PrerequisiteVerification) { v.Checks[1].Vantage = "foreign" }},
		{name: "physical proof cannot be generate only", fn: func(v *PrerequisiteVerification) { v.Checks[1].RequiredPhase = "generate" }},
		{name: "unknown without reason", fn: func(v *PrerequisiteVerification) { v.Checks[0].State = "unknown" }},
		{name: "duplicate identity", fn: func(v *PrerequisiteVerification) { v.Checks = append(v.Checks, v.Checks[0]) }},
		{name: "inventory mismatch", fn: func(v *PrerequisiteVerification) { v.Inventory.Management.TrustDigest = "other" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			candidateInventory, err := cloneInventory(current)
			if err != nil {
				t.Fatal(err)
			}
			candidate := PrerequisiteVerification{Inventory: candidateInventory, Checks: append([]inventory.Check(nil), verification.Checks...)}
			if err := validatePrerequisiteVerification(plan, candidate); err != nil {
				t.Fatalf("cloned baseline invalid: %v", err)
			}
			mutate.fn(&candidate)
			if err := validatePrerequisiteVerification(plan, candidate); err == nil {
				t.Fatal("invalid prerequisite observation was accepted")
			}
		})
	}
}

func TestValidateSelectedInventoryBindsExactSelectedEndpoints(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	intent := plan.Intent()
	current := &inventory.Inventory{FormatVersion: inventory.FormatVersion}
	for _, device := range intent.Devices {
		current.DPUs = append(current.DPUs, inventory.Device{
			SerialNumber: device.Serial, BMCEndpoint: (&url.URL{Scheme: "https", Host: device.BMCIP + ":" + strconv.Itoa(device.BMCPort)}).String(),
			ObservationState: "observed", ID: "observed-dpu",
		})
	}
	if err := validateSelectedInventory(plan, current); err != nil {
		t.Fatalf("exact selected inventory rejected: %v", err)
	}
	current.DPUs[0].BMCEndpoint = "https://192.0.2.250:443"
	if err := validateSelectedInventory(plan, current); err == nil {
		t.Fatal("changed selected BMC endpoint was accepted")
	}
}

func TestCloneInventoryPreservesTransientNodeLabels(t *testing.T) {
	input := &inventory.Inventory{Management: &inventory.Management{Nodes: []inventory.Node{{Name: "node-a", Labels: map[string]string{"role": "control-plane"}}}}}
	cloned, err := cloneInventory(input)
	if err != nil {
		t.Fatal(err)
	}
	cloned.Management.Nodes[0].Labels["role"] = "changed"
	if input.Management.Nodes[0].Labels["role"] != "control-plane" {
		t.Fatal("clone shares transient Kubernetes selector labels with caller")
	}
}

type runtimeCloseFailFile struct{ *managementMemoryFile }

func (f *runtimeCloseFailFile) Close() error {
	return errors.New("simulated durable report close failure")
}

func TestRunRuntimeFinalDurabilityGate(t *testing.T) {
	for _, scenario := range []string{"validated", "one-host-unmeasured", "stage-failed", "result-sync-failed", "report-close-failed", "probe-close-failed"} {
		t.Run(scenario, func(t *testing.T) {
			runtime := &HBNRuntimeResult{State: "passed", TrafficValidated: true, BGP: []HBNBGPResult{{DPUSerial: "SERIAL-A", State: "passed", Fenced: true}}}
			result := RunResult{State: RunStateFailed, LastCompletedStage: hbnRuntimeStage, TrafficValidated: true, HBNRuntime: runtime,
				HostNetworking: []HostWorkloadNetworkResult{{DPUSerial: "SERIAL-A", LeaseReverified: true}}, Activation: &ActivationSummary{DPUDeploymentUID: "retained-deployment"}}
			memory := &managementMemoryFile{}
			journal := &executionJournal{file: memory, digest: "retained-bundle"}
			closeProbe := func() error { return nil }
			var stageErr error
			switch scenario {
			case "one-host-unmeasured":
				result.TrafficValidated = false
				runtime.TrafficValidated = false
				runtime.State = "unmeasured"
				result.LastCompletedStage = hostWorkloadNetworkStage
			case "stage-failed":
				stageErr = errors.New("HBN_RUNTIME_POST_FENCE_FAILED")
				result.TrafficValidated = false
				runtime.TrafficValidated = false
				runtime.State = "failed"
				result.LastCompletedStage = hostWorkloadNetworkStage
			case "result-sync-failed":
				journal.file = &hostNetworkStageFailingJournalFile{managementMemoryFile: memory, failSyncAt: 1}
			case "report-close-failed":
				journal.file = &runtimeCloseFailFile{managementMemoryFile: memory}
			case "probe-close-failed":
				closeProbe = func() error { return errors.New("probe close failure") }
			}
			err := finishRunReport(&result, stageErr, closeProbe, journal, "selected-workloads")
			switch scenario {
			case "validated":
				if err != nil || result.State != RunStateValidated || !result.TrafficValidated || !result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hbnRuntimeStage {
					t.Fatalf("validated result=%+v err=%v", result, err)
				}
			case "one-host-unmeasured":
				if err != nil || result.State != RunStateProvisioned || result.TrafficValidated || result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hostWorkloadNetworkStage {
					t.Fatalf("one host result=%+v err=%v", result, err)
				}
			default:
				if err == nil || result.State != RunStateFailed || result.TrafficValidated || result.HBNRuntime.TrafficValidated || result.LastCompletedStage != hostWorkloadNetworkStage || result.Checks[len(result.Checks)-1].State != "failed" {
					t.Fatalf("uncertain result=%+v err=%v", result, err)
				}
			}
			if len(result.HostNetworking) != 1 || !result.HostNetworking[0].LeaseReverified || result.Activation.DPUDeploymentUID != "retained-deployment" || len(result.HBNRuntime.BGP) != 1 {
				t.Fatal("finalization lost prior partial receipts")
			}
		})
	}
}

// The Run boundary requires a distinct observation-only recovery method even
// when this test stops before any physical action.
type runRebooterFunc RebooterFunc

func (f runRebooterFunc) RebootExternal(ctx context.Context, request ActivationRequest) (ActivationObservation, error) {
	return RebooterFunc(f).RebootExternal(ctx, request)
}
func (runRebooterFunc) ObserveExternal(context.Context, ActivationRequest, string) (ActivationObservation, error) {
	return ActivationObservation{}, errors.New("observation not reached")
}
