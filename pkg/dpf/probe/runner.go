// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package probe

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Assets are release-owned. A verified published digest is still subject to
// controlled Kubernetes admission/runtime qualification before CLI activation.
type Assets struct {
	Repository      string
	ManifestDigest  string
	PlatformDigests map[string]string
	Qualified       bool
}

func (a Assets) Validate() error {
	if a.Repository == "" || !validDigest(a.ManifestDigest) || len(a.PlatformDigests) == 0 {
		return fmt.Errorf("qualified DPF probe assets are not installed in this build")
	}
	for _, d := range a.PlatformDigests {
		if !validDigest(d) {
			return fmt.Errorf("invalid platform image digest")
		}
	}
	if !a.Qualified {
		return fmt.Errorf("DPF probe image is pinned but Kubernetes runtime qualification is pending")
	}
	return nil
}
func validDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(s[7:])
	return err == nil
}

type Receipt struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid,omitempty"`
}
type Runner struct {
	Assets  Assets
	Journal func([]Receipt) error
}

func (runner Runner) Run(ctx context.Context, r config.Request, mg discovery.ManagementClient, inv *inventory.Inventory) (rows []inventory.ProbeResult, cleanup inventory.Cleanup, err error) {
	cleanup = inventory.Cleanup{State: "complete", Leftovers: []string{}}
	if ctx == nil || inv == nil || inv.Management == nil {
		return nil, cleanup, fmt.Errorf("probe requires a context and management inventory")
	}
	if mg.Client == nil || mg.REST == nil || mg.Namespace == "" {
		return nil, cleanup, fmt.Errorf("probe requires a management client, REST configuration, and namespace")
	}
	if r.Timeout < time.Second {
		return nil, cleanup, fmt.Errorf("probe timeout must be at least one second")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if err := runner.Assets.Validate(); err != nil {
		return nil, cleanup, err
	}
	if runner.Journal == nil {
		return nil, cleanup, fmt.Errorf("probe resource journal required")
	}
	if err := validateManagementNodes(inv.Management.Nodes, runner.Assets.PlatformDigests); err != nil {
		return nil, cleanup, err
	}
	api, err := url.Parse(mg.REST.Host)
	if err != nil || api.Scheme != "https" || api.Hostname() == "" || api.User != nil || api.RawQuery != "" || api.Fragment != "" {
		return nil, cleanup, fmt.Errorf("probe requires a valid HTTPS management API endpoint")
	}
	ns, err := mg.Client.CoreV1().Namespaces().Get(ctx, mg.Namespace, metav1.GetOptions{})
	if err != nil || ns == nil || ns.DeletionTimestamp != nil {
		return nil, cleanup, fmt.Errorf("probe namespace unavailable")
	}
	request := Request{RequestTimeout: r.RequestTimeout, Version: ProtocolVersion, RunID: inv.Collection.RunID, Deadline: time.Now().Add(r.Timeout), Destinations: []Destination{}}
	if r.UseCase == "hbn-bgp" && r.Input.Site.Provisioning != nil && r.Input.Site.Provisioning.Bootstrap != nil && r.Input.Site.Provisioning.Bootstrap.Tenant.VIP != nil {
		request.TenantVIP = *r.Input.Site.Provisioning.Bootstrap.Tenant.VIP
	}
	request.Destinations = append(request.Destinations, Destination{ID: "management-api", Endpoint: api.Scheme + "://" + api.Host, Path: path.Join("/", api.Path, "version"), ServerName: mg.REST.ServerName, CA: mg.REST.CAData})
	for _, t := range r.Targets {
		if t.Role == "dpus" {
			request.Destinations = append(request.Destinations, Destination{ID: inventory.Digest("probe-destination-v1", t.Endpoint), Endpoint: t.Endpoint, Path: "/redfish/v1/", CA: t.Access.PublicCA(), InsecureSkipVerify: t.Access.InsecureSkipVerify()})
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		request.Deadline = deadline
	}
	files, err := requestFiles(request)
	if err != nil {
		return nil, cleanup, err
	}
	receipts := []Receipt{}
	pods := map[string]*corev1.Pod{}
	var configMap *corev1.ConfigMap
	journal := func(kind, name string, uid types.UID) error {
		receipts = append(receipts, Receipt{Kind: kind, Namespace: mg.Namespace, Name: name, UID: uid})
		return runner.Journal(receipts)
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		for i := len(receipts) - 1; i >= 0; i-- {
			receipt := receipts[i]
			if receipt.UID == "" {
				if receipt.Kind == "Pod" {
					live, e := mg.Client.CoreV1().Pods(mg.Namespace).Get(cleanCtx, receipt.Name, metav1.GetOptions{})
					if e == nil && samePod(pods[receipt.Name], live) {
						receipt.UID = live.UID
					}
				} else {
					live, e := mg.Client.CoreV1().ConfigMaps(mg.Namespace).Get(cleanCtx, receipt.Name, metav1.GetOptions{})
					if e == nil && sameConfigMap(configMap, live) {
						receipt.UID = live.UID
					}
				}
				if receipt.UID == "" {
					cleanup.State = "incomplete"
					cleanup.Leftovers = append(cleanup.Leftovers, receipt.Kind+"/"+receipt.Name+": create outcome unknown")
					continue
				}
				receipts[i] = receipt
				_ = runner.Journal(receipts)
			}
			var cleanupErr error
			for {
				if cleanCtx.Err() != nil {
					cleanupErr = cleanCtx.Err()
					break
				}
				options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &receipt.UID}}
				if receipt.Kind == "Pod" {
					live, e := mg.Client.CoreV1().Pods(mg.Namespace).Get(cleanCtx, receipt.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(e) {
						cleanupErr = nil
						break
					}
					if e != nil {
						cleanupErr = e
						break
					}
					if live.UID != receipt.UID {
						cleanupErr = nil
						break
					}
					if !samePod(pods[receipt.Name], live) {
						cleanupErr = fmt.Errorf("probe ownership/spec changed")
						break
					}
					options.Preconditions.ResourceVersion = &live.ResourceVersion
					cleanupErr = mg.Client.CoreV1().Pods(mg.Namespace).Delete(cleanCtx, receipt.Name, options)
				} else {
					live, e := mg.Client.CoreV1().ConfigMaps(mg.Namespace).Get(cleanCtx, receipt.Name, metav1.GetOptions{})
					if apierrors.IsNotFound(e) {
						cleanupErr = nil
						break
					}
					if e != nil {
						cleanupErr = e
						break
					}
					if live.UID != receipt.UID {
						cleanupErr = nil
						break
					}
					if !sameConfigMap(configMap, live) {
						cleanupErr = fmt.Errorf("request ownership/data changed")
						break
					}
					options.Preconditions.ResourceVersion = &live.ResourceVersion
					cleanupErr = mg.Client.CoreV1().ConfigMaps(mg.Namespace).Delete(cleanCtx, receipt.Name, options)
				}
				if apierrors.IsNotFound(cleanupErr) {
					cleanupErr = nil
					break
				}
				if cleanupErr != nil && !apierrors.IsConflict(cleanupErr) {
					break
				}
				select {
				case <-cleanCtx.Done():
					cleanupErr = cleanCtx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if cleanupErr != nil && !apierrors.IsConflict(cleanupErr) {
					break
				}
			}
			if cleanupErr != nil {
				cleanup.State = "incomplete"
				cleanup.Leftovers = append(cleanup.Leftovers, receipt.Kind+"/"+receipt.Name)
			}
		}
		if cleanup.State != "complete" && err == nil {
			err = fmt.Errorf("probe cleanup incomplete; see resource journal")
		}
	}()
	name := "l8k-dpf-" + strings.TrimPrefix(inventory.Digest("run", inv.Collection.RunID), "sha256:")[:16]
	labels := map[string]string{"app.kubernetes.io/managed-by": "l8k-dpf-discovery", "l8k.nvidia.com/discovery-run": name}
	if err = journal("ConfigMap", name, ""); err != nil {
		return nil, cleanup, err
	}
	desired := &corev1.ConfigMap{Immutable: ptr(true), ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mg.Namespace, Labels: labels}, Data: files}
	configMap = desired
	configMap, err = mg.Client.CoreV1().ConfigMaps(mg.Namespace).Create(ctx, desired, metav1.CreateOptions{})
	if err != nil {
		configMap = desired
		if createRejected(err) {
			receipts = receipts[:len(receipts)-1]
		}
		return nil, cleanup, fmt.Errorf("probe ConfigMap create outcome unavailable; inspect journal")
	}
	receipts[len(receipts)-1].UID = configMap.UID
	if !sameConfigMap(desired, configMap) {
		configMap = desired
		return nil, cleanup, fmt.Errorf("admission changed probe request data/ownership")
	}
	if err = runner.Journal(receipts); err != nil {
		return nil, cleanup, err
	}
	if inv.Evidence == nil {
		inv.Evidence = make(map[string]inventory.Evidence)
	}
	pathFailures := false
	for _, node := range inv.Management.Nodes {
		for _, mode := range []string{"pod-network", "node-network"} {
			output := ui.FromContext(ctx)
			output.Info("[RUNNING] Management node %s: %s probe", node.Name, mode)
			modePathFailed := false
			podName := name + "-" + strings.TrimPrefix(inventory.Digest("node", node.UID+mode), "sha256:")[:12]
			image := runner.Assets.Repository
			if r.ImageRepository != "" {
				image = r.ImageRepository
			}
			// Pull the same architecture-specific manifest that the runtime identity
			// check expects; CRI may report the requested index when pulling an index.
			image += "@" + runner.Assets.PlatformDigests[node.Architecture]
			desiredPod := Pod(podName, mg.Namespace, labels, node, mode, image, name, r.ImagePullSecrets, int64(r.Timeout/time.Second))
			if err = journal("Pod", podName, ""); err != nil {
				return rows, cleanup, err
			}
			pods[podName] = desiredPod
			created, createErr := mg.Client.CoreV1().Pods(mg.Namespace).Create(ctx, desiredPod, metav1.CreateOptions{})
			if createErr != nil {
				if createRejected(createErr) {
					receipts = receipts[:len(receipts)-1]
				}
				return rows, cleanup, fmt.Errorf("probe Pod create outcome unavailable; inspect journal")
			}
			receipts[len(receipts)-1].UID = created.UID
			pods[podName] = created
			if err = runner.Journal(receipts); err != nil {
				return rows, cleanup, err
			}
			if !samePod(desiredPod, created) {
				return rows, cleanup, fmt.Errorf("probe admission changed required invariants")
			}
			var live *corev1.Pod
			for {
				live, err = mg.Client.CoreV1().Pods(mg.Namespace).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return rows, cleanup, fmt.Errorf("probe Pod inspection failed")
				}
				if !samePod(created, live) || live.UID != created.UID {
					return rows, cleanup, fmt.Errorf("probe Pod identity/spec changed")
				}
				if live.Status.Phase == corev1.PodSucceeded {
					break
				}
				if live.Status.Phase == corev1.PodFailed {
					return rows, cleanup, fmt.Errorf("probe execution failed")
				}
				select {
				case <-ctx.Done():
					return rows, cleanup, ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
			currentNode, err := mg.Client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			if err != nil || string(currentNode.UID) != node.UID || live.Spec.NodeName != node.Name {
				return rows, cleanup, fmt.Errorf("probe node identity changed")
			}
			if len(live.Status.ContainerStatuses) != 1 || live.Status.ContainerStatuses[0].ImageID != image {
				return rows, cleanup, fmt.Errorf("probe pulled image digest differs from qualified asset")
			}
			stream, err := mg.Client.CoreV1().Pods(mg.Namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "probe", LimitBytes: ptr(int64(4 << 20))}).Stream(ctx)
			if err != nil {
				return rows, cleanup, fmt.Errorf("probe log access failed")
			}
			body, readErr := io.ReadAll(io.LimitReader(stream, (4<<20)+1))
			stream.Close()
			if readErr != nil || len(body) > 4<<20 {
				return rows, cleanup, fmt.Errorf("probe result too large")
			}
			var result Response
			if json.Unmarshal(body, &result) != nil || ValidateResponse(result, request, node.UID, string(live.UID), mode) != nil || ValidateNetworkResponse(result, request, mode) != nil {
				return rows, cleanup, fmt.Errorf("probe result binding/coverage invalid")
			}
			frozenMap, mapErr := mg.Client.CoreV1().ConfigMaps(mg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if mapErr != nil || frozenMap.UID != configMap.UID || !sameConfigMap(configMap, frozenMap) {
				return rows, cleanup, fmt.Errorf("probe request identity changed")
			}
			if mode == "node-network" {
				for i := range inv.Management.Nodes {
					if inv.Management.Nodes[i].UID == node.UID {
						inv.Management.Nodes[i].Interfaces = result.Interfaces
						inv.Management.Nodes[i].Routes = result.Routes
						inv.Management.Nodes[i].TenantVIPRoute = result.TenantVIPRoute
					}
				}
			}
			for _, row := range result.Results {
				id := inventory.Digest("probe-evidence-v1", []string{node.UID, mode, row.Destination})
				row.EvidenceRef = id
				inv.Evidence[id] = inventory.Evidence{Kind: "connectivity-probe", ObservedAt: result.FinishedAt, Vantage: node.UID + "/" + mode, ResourceURI: "pods/" + string(live.UID), Namespace: mg.Namespace, PodUID: string(live.UID), NodeUID: node.UID, ImageDigest: runner.Assets.PlatformDigests[node.Architecture], DNSPolicy: string(live.Spec.DNSPolicy), DestinationEndpoint: destinationEndpoint(request, row.Destination)}
				rows = append(rows, row)
				if row.DNS == "unavailable" || len(row.Addresses) == 0 {
					pathFailures = true
					modePathFailed = true
					continue
				}
				expectedTLS := "verified"
				if destinationAllowsUnverified(request, row.Destination) {
					expectedTLS = "unverified"
				}
				for _, a := range row.Addresses {
					if a.TCP != "reachable" || a.TLS != expectedTLS {
						pathFailures = true
						modePathFailed = true
						break
					}
				}
			}
			if modePathFailed {
				output.Info("[BLOCKED] Management node %s: %s probe has a failed required path", node.Name, mode)
			} else {
				output.Info("[PASS] Management node %s: %s probe paths passed", node.Name, mode)
			}
		}
	}
	if pathFailures {
		return rows, cleanup, fmt.Errorf("one or more required probe paths failed; see per-destination checks")
	}
	return rows, cleanup, nil
}

func validateManagementNodes(nodes []inventory.Node, platformDigests map[string]string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("probe requires a non-empty management node scope")
	}
	names, uids := map[string]bool{}, map[string]bool{}
	for _, node := range nodes {
		if node.Name == "" || node.UID == "" || names[node.Name] || uids[node.UID] {
			return fmt.Errorf("management probe node scope has a missing or duplicate name/UID")
		}
		names[node.Name], uids[node.UID] = true, true
		if !node.Ready || node.Unschedulable || node.OS != "linux" || platformDigests[node.Architecture] == "" {
			return fmt.Errorf("management node cannot run the qualified probe")
		}
	}
	return nil
}
func ptr[T any](value T) *T { return &value }
func Pod(name, namespace string, labels map[string]string, node inventory.Node, mode, image, configMap string, secrets []string, seconds int64) *corev1.Pod {
	container := corev1.Container{
		Name: "probe", Image: image,
		Command:    []string{"/bin/bash", "/request/collect.sh"},
		Args:       []string{"/request/request.json", node.UID, mode},
		WorkingDir: "/scratch",
		Env:        []corev1.EnvVar{{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "request", MountPath: "/request", ReadOnly: true}, {Name: "scratch", MountPath: "/scratch"}},
	}
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr(false), ActiveDeadlineSeconds: &seconds,
		HostNetwork: mode == "node-network", DNSPolicy: corev1.DNSClusterFirst,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(65532)),
			FSGroup:        ptr(int64(65532)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node.Name}}}}},
			},
		}},
		Tolerations: []corev1.Toleration{
			{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		},
		Containers: []corev1.Container{container},
		Volumes:    []corev1.Volume{{Name: "request", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}}}, {Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse("16Mi"))}}}},
	}
	if mode == "node-network" {
		spec.DNSPolicy = corev1.DNSDefault
	}
	for _, secret := range secrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: secret})
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}, Spec: spec}
}
func samePod(expected, actual *corev1.Pod) bool {
	if expected == nil || actual == nil {
		return false
	}
	a, b := expected.Spec.DeepCopy(), actual.Spec.DeepCopy()
	// Only known harmless server/scheduler defaults are excluded from comparison.
	normalize := func(s *corev1.PodSpec) {
		s.NodeName = ""
		tolerations := []corev1.Toleration{}
		for _, t := range s.Tolerations {
			standard := (t.Key == "node.kubernetes.io/not-ready" || t.Key == "node.kubernetes.io/unreachable") && t.Operator == corev1.TolerationOpExists && t.Effect == corev1.TaintEffectNoExecute && t.TolerationSeconds != nil && *t.TolerationSeconds == 300
			if !standard {
				tolerations = append(tolerations, t)
			}
		}
		s.Tolerations = tolerations
		s.ServiceAccountName = ""
		s.DeprecatedServiceAccount = ""
		s.SchedulerName = ""
		s.TerminationGracePeriodSeconds = nil
		s.EnableServiceLinks = nil
		s.PreemptionPolicy = nil
		s.Priority = nil

		for i := range s.Containers {
			s.Containers[i].ImagePullPolicy = ""
			s.Containers[i].TerminationMessagePath = ""
			s.Containers[i].TerminationMessagePolicy = ""
			for j := range s.Containers[i].Env {
				if src := s.Containers[i].Env[j].ValueFrom; src != nil && src.FieldRef != nil {
					src.FieldRef.APIVersion = ""
				}
			}
		}
		for i := range s.Volumes {
			if s.Volumes[i].ConfigMap != nil {
				s.Volumes[i].ConfigMap.DefaultMode = nil
			}
		}
	}
	normalize(a)
	normalize(b)
	// Kubernetes protobuf decoding can change resource.Quantity internals without
	// changing the value. Compare API semantics after the explicit default normalization.
	return apiequality.Semantic.DeepEqual(a, b) && reflect.DeepEqual(expected.Labels, actual.Labels) && reflect.DeepEqual(expected.OwnerReferences, actual.OwnerReferences) && reflect.DeepEqual(expected.Annotations, actual.Annotations)
}
func ValidateResponse(out Response, req Request, node, pod, mode string) error {
	if out.Version != ProtocolVersion || out.RunID != req.RunID || out.NodeUID != node || out.PodUID != pod || out.NetworkMode != mode || len(out.Results) != len(req.Destinations) {
		return fmt.Errorf("probe binding mismatch")
	}
	if out.StartedAt.IsZero() || out.FinishedAt.Before(out.StartedAt) || out.FinishedAt.After(req.Deadline.Add(time.Minute)) {
		return fmt.Errorf("invalid probe time bounds")
	}
	expected := map[string]Destination{}
	for _, d := range req.Destinations {
		expected[d.ID] = d
	}
	for _, r := range out.Results {
		destination, present := expected[r.Destination]
		if !present || r.NodeUID != node || r.NetworkMode != mode {
			return fmt.Errorf("unexpected or duplicate row")
		}
		if err := validateRow(r); err != nil {
			return err
		}
		for _, address := range r.Addresses {
			if address.TLS == "unverified" && (!destination.InsecureSkipVerify || destination.ID == "management-api") || address.TLS == "verified" && destination.InsecureSkipVerify {
				return fmt.Errorf("probe TLS result contradicts requested verification policy")
			}
		}
		delete(expected, r.Destination)
	}
	return nil
}

// FileJournal contains no auth material and records intended creates before dispatch.
func FileJournal(filename string) func([]Receipt) error {
	return func(receipts []Receipt) error {
		b, err := json.MarshalIndent(receipts, "", "  ")
		if err != nil {
			return err
		}
		return artifacts.AtomicWrite(filename, b)
	}
}

func sameConfigMap(a, b *corev1.ConfigMap) bool {
	return a != nil && b != nil && a.Immutable != nil && b.Immutable != nil && *a.Immutable && *b.Immutable && reflect.DeepEqual(a.Data, b.Data) && reflect.DeepEqual(a.BinaryData, b.BinaryData) && reflect.DeepEqual(a.Labels, b.Labels) && reflect.DeepEqual(a.OwnerReferences, b.OwnerReferences) && reflect.DeepEqual(a.Annotations, b.Annotations)
}
func destinationEndpoint(request Request, id string) string {
	for _, d := range request.Destinations {
		if d.ID == id {
			return d.Endpoint
		}
	}
	return ""
}
func destinationAllowsUnverified(request Request, id string) bool {
	for _, destination := range request.Destinations {
		if destination.ID == id {
			return id != "management-api" && destination.InsecureSkipVerify
		}
	}
	return false
}
func validateRow(r inventory.ProbeResult) error {
	if r.DNS != "resolved" && r.DNS != "not-applicable" && r.DNS != "unavailable" {
		return fmt.Errorf("invalid DNS state")
	}
	if len(r.Addresses) > 32 {
		return fmt.Errorf("address limit exceeded")
	}
	seen := map[string]bool{}
	for _, a := range r.Addresses {
		if _, err := netip.ParseAddr(a.Address); err != nil || seen[a.Address] {
			return fmt.Errorf("invalid/duplicate IP address")
		}
		seen[a.Address] = true
		if a.TCP != "reachable" && a.TCP != "unavailable" || a.TLS != "verified" && a.TLS != "unverified" && a.TLS != "unavailable" || (a.TLS == "verified" || a.TLS == "unverified") && a.TCP != "reachable" {
			return fmt.Errorf("invalid TCP/TLS state")
		}
		switch a.HTTP {
		case "observed", "authentication-required", "redirect-refused", "resource-unavailable", "application-error", "http-unavailable", "invalid-response":
		default:
			return fmt.Errorf("invalid HTTP state")
		}
	}
	return nil
}

func createRejected(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsAlreadyExists(err) || apierrors.IsInvalid(err)
}
