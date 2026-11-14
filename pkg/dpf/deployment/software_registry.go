// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	softwareRegistryPodName    = "bfb-registry"
	softwareLeaderLeaseName    = "provisioning.dpu.nvidia.com"
	softwareCABundleName       = "dpf-ca-trust-bundle"
	softwareCABundleKey        = "ca.crt"
	softwareRegistryConfigName = "bfb-registry-config"
	softwareRegistryConfigKey  = "nginx.conf"
	// Derived from internal/operator/inventory/manifests/bfb-registry.yaml at
	// DPF source fc4974ed3bc026ae27634673c2fdf818a2972235.
	softwareRegistryConfigSHA256 = "5f24656c176fe6ed2f9945befeececbba555cd601198adec9478ef77175f9028"
	softwareBFBHostPath          = "/var/lib/nvidia/dpf/bfb"
	softwareBFBMountPath         = "/bfb"
	softwareHTTPSPort            = int32(8443)
)

// registrySnapshot binds a registry Pod's HTTPS bytes to the active
// provisioning manager and its /bfb storage for one point-in-time read. It
// validates the declared NGINX ConfigMap and invocation, but cannot attest
// that a running process loaded those bytes after a ConfigMap subPath update.
// It does not bind an external load balancer or prove that a later consumer
// read the same bytes. The `/bfb` URL-to-file mapping relies on the pinned DPF
// registry configuration; it is intentionally not a general NGINX config
// verifier.
type registrySnapshot struct {
	namespace            string
	caPEM                []byte
	caName               string
	caUID                types.UID
	registryConfigUID    types.UID
	registryConfigSHA256 string
	registry             registryPodBinding
	manager              registryManagerBinding
	storage              registryStorage
	lease                registryLeaseBinding
}

type registryPodBinding struct {
	name      string
	uid       types.UID
	node      string
	container registryContainerBinding
}

type registryManagerBinding struct {
	name          string
	uid           types.UID
	node          string
	container     registryContainerBinding
	registryImage string
}

type registryContainerBinding struct {
	image   string
	imageID string
	command []string
	args    []string
}

type registryLeaseBinding struct {
	uid         types.UID
	holder      string
	transitions *int32
}

type registryStorage struct {
	kind       string
	claimName  string
	claimUID   types.UID
	volumeName string
	hostPath   string
	readOnly   bool
}

// observeRegistrySnapshot reads only the namespaced objects that establish
// which DPF registry Pod is serving and whether it shares /bfb with the active
// provisioning leader. It never reads the registry's private TLS Secret.
func (c *APIClient) observeRegistrySnapshot(ctx context.Context, namespace string) (registrySnapshot, error) {
	if c == nil || c.access.Client == nil || ctx == nil || namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return registrySnapshot{}, fmt.Errorf("a bound management client, context and valid provisioning namespace are required")
	}
	if err := ctx.Err(); err != nil {
		return registrySnapshot{}, err
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return registrySnapshot{}, err
	}

	registry, err := c.access.Client.CoreV1().Pods(namespace).Get(ctx, softwareRegistryPodName, metav1.GetOptions{})
	if err != nil {
		return registrySnapshot{}, safeAPIError("read BFB registry Pod", err)
	}
	owner, err := registryLeaderOwner(registry, namespace)
	if err != nil {
		return registrySnapshot{}, err
	}
	manager, err := c.access.Client.CoreV1().Pods(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return registrySnapshot{}, safeAPIError("read provisioning leader Pod", err)
	}
	lease, err := c.access.Client.CoordinationV1().Leases(namespace).Get(ctx, softwareLeaderLeaseName, metav1.GetOptions{})
	if err != nil {
		return registrySnapshot{}, safeAPIError("read provisioning leader Lease", err)
	}
	if err := validateRegistryLeader(registry, manager, lease, owner); err != nil {
		return registrySnapshot{}, err
	}

	caName, err := managerCABundleName(manager)
	if err != nil {
		return registrySnapshot{}, err
	}
	ca, err := c.access.Client.CoreV1().ConfigMaps(namespace).Get(ctx, caName, metav1.GetOptions{})
	if err != nil {
		return registrySnapshot{}, safeAPIError("read provisioning CA bundle ConfigMap", err)
	}
	caPEM := ca.Data[softwareCABundleKey]
	if ca.Name != caName || ca.Namespace != namespace || ca.UID == "" || ca.DeletionTimestamp != nil || caPEM == "" || !validCertificateBundle([]byte(caPEM)) {
		return registrySnapshot{}, fmt.Errorf("provisioning CA bundle ConfigMap is deleting or has no valid public CA certificate bundle")
	}
	registryConfig, err := c.access.Client.CoreV1().ConfigMaps(namespace).Get(ctx, softwareRegistryConfigName, metav1.GetOptions{})
	if err != nil {
		return registrySnapshot{}, safeAPIError("read BFB registry configuration ConfigMap", err)
	}
	nginxConfig := registryConfig.Data[softwareRegistryConfigKey]
	nginxConfigDigest := sha256.Sum256([]byte(nginxConfig))
	if registryConfig.Name != softwareRegistryConfigName || registryConfig.Namespace != namespace || registryConfig.UID == "" || registryConfig.DeletionTimestamp != nil || nginxConfig == "" || registryConfig.BinaryData[softwareRegistryConfigKey] != nil || hex.EncodeToString(nginxConfigDigest[:]) != softwareRegistryConfigSHA256 {
		return registrySnapshot{}, fmt.Errorf("BFB registry configuration differs from the pinned DPF mapping")
	}

	registryContainer, registryStatus, err := readyContainer(registry, "bfb-registry")
	if err != nil {
		return registrySnapshot{}, fmt.Errorf("BFB registry container is unavailable: %w", err)
	}
	managerContainer, managerStatus, err := readyContainer(manager, "manager")
	if err != nil {
		return registrySnapshot{}, fmt.Errorf("provisioning manager container is unavailable: %w", err)
	}
	registryImage, err := managerRegistryImage(managerContainer)
	if err != nil || registryImage != registryContainer.Image {
		return registrySnapshot{}, fmt.Errorf("BFB registry image differs from the active provisioning manager configuration")
	}
	if !hasRegistryHTTPSPort(registryContainer) {
		return registrySnapshot{}, fmt.Errorf("BFB registry container does not expose the pinned HTTPS port")
	}
	if err := validateRegistryConfigMount(registry, registryContainer); err != nil {
		return registrySnapshot{}, err
	}
	registryStorage, err := podBFBStorage(registry, registryContainer)
	if err != nil {
		return registrySnapshot{}, fmt.Errorf("BFB registry storage binding is invalid: %w", err)
	}
	managerStorage, err := podBFBStorage(manager, managerContainer)
	if err != nil {
		return registrySnapshot{}, fmt.Errorf("provisioning manager storage binding is invalid: %w", err)
	}
	if err := compareBFBStorage(registryStorage, managerStorage, registry.Spec.NodeName, manager.Spec.NodeName); err != nil {
		return registrySnapshot{}, err
	}
	if registryStorage.kind == "pvc" {
		claim, getErr := c.access.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, registryStorage.claimName, metav1.GetOptions{})
		if getErr != nil {
			return registrySnapshot{}, safeAPIError("read BFB storage claim", getErr)
		}
		if claim.Name != registryStorage.claimName || claim.Namespace != namespace || claim.UID == "" || claim.DeletionTimestamp != nil || claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
			return registrySnapshot{}, fmt.Errorf("BFB storage claim is not a current bound claim")
		}
		registryStorage.claimUID, registryStorage.volumeName = claim.UID, claim.Spec.VolumeName
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return registrySnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return registrySnapshot{}, err
	}

	return registrySnapshot{
		namespace: namespace, caPEM: []byte(caPEM), caName: ca.Name, caUID: ca.UID,
		registryConfigUID: registryConfig.UID, registryConfigSHA256: hex.EncodeToString(nginxConfigDigest[:]),
		registry: registryPodBinding{name: registry.Name, uid: registry.UID, node: registry.Spec.NodeName,
			container: registryContainerBinding{image: registryContainer.Image, imageID: registryStatus.ImageID,
				command: slices.Clone(registryContainer.Command), args: slices.Clone(registryContainer.Args)}},
		manager: registryManagerBinding{name: manager.Name, uid: manager.UID, node: manager.Spec.NodeName,
			container: registryContainerBinding{image: managerContainer.Image, imageID: managerStatus.ImageID,
				command: slices.Clone(managerContainer.Command), args: slices.Clone(managerContainer.Args)}, registryImage: registryImage},
		storage: registryStorage,
		lease:   registryLeaseBinding{uid: lease.UID, holder: *lease.Spec.HolderIdentity, transitions: cloneInt32(lease.Spec.LeaseTransitions)},
	}, nil
}

// sameBinding allows routine Lease renewal but rejects leader transitions,
// Pod replacements, image/command changes, storage changes, or CA rotation.
func (s registrySnapshot) sameBinding(other registrySnapshot) bool {
	return s.namespace == other.namespace && s.caName == other.caName && s.caUID == other.caUID && bytes.Equal(s.caPEM, other.caPEM) && s.registryConfigUID == other.registryConfigUID && s.registryConfigSHA256 == other.registryConfigSHA256 &&
		s.registry.name == other.registry.name && s.registry.uid == other.registry.uid && s.registry.node == other.registry.node && equalContainer(s.registry.container, other.registry.container) &&
		s.manager.name == other.manager.name && s.manager.uid == other.manager.uid && s.manager.node == other.manager.node && equalContainer(s.manager.container, other.manager.container) && s.manager.registryImage == other.manager.registryImage &&
		s.storage == other.storage && s.lease.uid == other.lease.uid && s.lease.holder == other.lease.holder && equalInt32(s.lease.transitions, other.lease.transitions)
}

func equalContainer(left, right registryContainerBinding) bool {
	return left.image == right.image && left.imageID == right.imageID && slices.Equal(left.command, right.command) && slices.Equal(left.args, right.args)
}

func registryLeaderOwner(pod *corev1.Pod, namespace string) (metav1.OwnerReference, error) {
	if pod == nil || pod.Namespace != namespace || pod.Name != softwareRegistryPodName || pod.UID == "" || pod.ResourceVersion == "" || pod.DeletionTimestamp != nil || !podReady(pod) ||
		pod.Labels["app.kubernetes.io/part-of"] != "bfb-registry" || pod.Labels["dpu.nvidia.com/component"] != "bfb-registry" {
		return metav1.OwnerReference{}, fmt.Errorf("BFB registry Pod is missing, not ready, deleting, or outside the pinned DPF identity")
	}
	var owner *metav1.OwnerReference
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			if owner != nil {
				return metav1.OwnerReference{}, fmt.Errorf("BFB registry Pod has ambiguous controller owners")
			}
			owner = ref
		}
	}
	if owner == nil || owner.APIVersion != "v1" || owner.Kind != "Pod" || owner.Name == "" || owner.UID == "" {
		return metav1.OwnerReference{}, fmt.Errorf("BFB registry Pod has no exact provisioning manager Pod owner")
	}
	return *owner, nil
}

func validateRegistryLeader(registry, manager *corev1.Pod, lease *coordinationv1.Lease, owner metav1.OwnerReference) error {
	if manager == nil || manager.Namespace != registry.Namespace || manager.Name != owner.Name || manager.UID != owner.UID || manager.ResourceVersion == "" || manager.DeletionTimestamp != nil || !podReady(manager) || manager.Spec.NodeName == "" || manager.Spec.NodeName != registry.Spec.NodeName {
		return fmt.Errorf("BFB registry owner is not the current ready provisioning manager Pod on the same node")
	}
	if manager.Labels["app.kubernetes.io/part-of"] != "dpf-provisioning-controller-manager" || manager.Labels["dpu.nvidia.com/component"] != "dpf-provisioning-controller-manager" ||
		manager.Spec.Hostname != "" && manager.Spec.Hostname != manager.Name {
		return fmt.Errorf("BFB registry owner is not an identifiable pinned provisioning manager Pod")
	}
	if lease == nil || lease.Namespace != manager.Namespace || lease.Name != softwareLeaderLeaseName || lease.UID == "" || lease.ResourceVersion == "" || lease.DeletionTimestamp != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 || lease.Spec.RenewTime == nil || lease.Spec.RenewTime.IsZero() {
		return fmt.Errorf("provisioning leader Lease is missing or malformed")
	}
	if !lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).After(time.Now()) {
		return fmt.Errorf("provisioning leader Lease has expired")
	}
	leaderName, ok := leaderPodName(*lease.Spec.HolderIdentity)
	if !ok || leaderName != manager.Name || (manager.Spec.Hostname != "" && manager.Spec.Hostname != leaderName) {
		return fmt.Errorf("BFB registry owner differs from the active provisioning leader")
	}
	managerContainer, _, err := readyContainer(manager, "manager")
	if err != nil {
		return fmt.Errorf("active provisioning manager container is unavailable")
	}
	if !leaderElectionEnabled(managerContainer.Args) {
		return fmt.Errorf("provisioning manager does not use the pinned leader-election contract")
	}
	return nil
}

func leaderPodName(holder string) (string, bool) {
	name, suffix, ok := strings.Cut(holder, "_")
	if !ok || name == "" || !validUUID(suffix) {
		return "", false
	}
	return name, true
}

func validUUID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		for _, char := range part {
			if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
				return false
			}
		}
	}
	return true
}

func leaderElectionEnabled(args []string) bool {
	seen := false
	for _, arg := range args {
		name, value, hasValue, isFlag := managerFlag(arg)
		if !isFlag {
			continue
		}
		if name != "leader-elect" {
			continue
		}
		if seen {
			return false
		}
		seen = true
		if !hasValue {
			continue
		}
		enabled, err := strconv.ParseBool(value)
		if err != nil || !enabled {
			return false
		}
	}
	return seen
}

func managerCABundleName(manager *corev1.Pod) (string, error) {
	container, _, err := readyContainer(manager, "manager")
	if err != nil || len(container.Command) != 1 || container.Command[0] != "/provisioning" {
		return "", fmt.Errorf("provisioning manager command differs from the pinned binary contract")
	}
	name := softwareCABundleName
	seen := false
	for i := 0; i < len(container.Args); i++ {
		key, value, hasValue, isFlag := managerFlag(container.Args[i])
		if !isFlag {
			continue
		}
		if key != "ca-trust-bundle-configmap-name" {
			continue
		}
		if seen {
			return "", fmt.Errorf("provisioning manager CA ConfigMap override is ambiguous")
		}
		seen = true
		if hasValue {
			name = value
		} else {
			if i+1 >= len(container.Args) || strings.HasPrefix(container.Args[i+1], "-") {
				return "", fmt.Errorf("provisioning manager CA ConfigMap override has no value")
			}
			i++
			name = container.Args[i]
		}
	}
	if name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return "", fmt.Errorf("provisioning manager CA ConfigMap name is invalid")
	}
	return name, nil
}

func managerFlag(arg string) (name, value string, hasValue, ok bool) {
	if strings.HasPrefix(arg, "---") || !strings.HasPrefix(arg, "-") {
		return "", "", false, false
	}
	arg = strings.TrimPrefix(arg, "--")
	arg = strings.TrimPrefix(arg, "-")
	if arg == "" || strings.HasPrefix(arg, "-") {
		return "", "", false, false
	}
	name, value, hasValue = strings.Cut(arg, "=")
	return name, value, hasValue, name != ""
}

func readyContainer(pod *corev1.Pod, name string) (*corev1.Container, *corev1.ContainerStatus, error) {
	if pod == nil || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
		return nil, nil, fmt.Errorf("pod is not running and ready")
	}
	var container *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			if container != nil {
				return nil, nil, fmt.Errorf("container name is ambiguous")
			}
			container = &pod.Spec.Containers[i]
		}
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			if status != nil {
				return nil, nil, fmt.Errorf("container status is ambiguous")
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if container == nil || container.Image == "" || len(container.Command) == 0 || status == nil || !status.Ready || status.ImageID == "" {
		return nil, nil, fmt.Errorf("container configuration or runtime image identity is incomplete")
	}
	return container, status, nil
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.UID == "" || pod.ResourceVersion == "" || pod.DeletionTimestamp != nil {
		return false
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			if ready || condition.Status != corev1.ConditionTrue {
				return false
			}
			ready = true
		}
	}
	return ready
}

func podBFBStorage(pod *corev1.Pod, container *corev1.Container) (registryStorage, error) {
	if pod == nil || container == nil {
		return registryStorage{}, fmt.Errorf("pod container is missing")
	}
	var mount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		candidate := &container.VolumeMounts[i]
		mountPath := path.Clean(candidate.MountPath)
		if mountPath == softwareBFBMountPath {
			if mount != nil {
				return registryStorage{}, fmt.Errorf("/bfb mount is ambiguous")
			}
			mount = candidate
		} else if mountPath == "." || mountPath == "/" || strings.HasPrefix(mountPath, softwareBFBMountPath+"/") || strings.HasPrefix(softwareBFBMountPath, strings.TrimSuffix(mountPath, "/")+"/") {
			return registryStorage{}, fmt.Errorf("another mount can obscure /bfb cache files")
		}
	}
	if mount == nil || mount.Name == "" || mount.SubPath != "" || mount.SubPathExpr != "" || mount.MountPath != softwareBFBMountPath {
		return registryStorage{}, fmt.Errorf("container does not expose the exact /bfb mount")
	}
	var volume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == mount.Name {
			if volume != nil {
				return registryStorage{}, fmt.Errorf("/bfb volume name is ambiguous")
			}
			volume = &pod.Spec.Volumes[i]
		}
	}
	if volume == nil {
		return registryStorage{}, fmt.Errorf("/bfb volume is missing")
	}
	storage, err := volumeStorage(*volume, mount.ReadOnly)
	if err != nil {
		return registryStorage{}, err
	}
	return storage, nil
}

func volumeStorage(volume corev1.Volume, readOnly bool) (registryStorage, error) {
	if volume.PersistentVolumeClaim != nil && volume.HostPath == nil && volume.EmptyDir == nil && volume.ConfigMap == nil && volume.Secret == nil {
		if volume.PersistentVolumeClaim.ClaimName == "" {
			return registryStorage{}, fmt.Errorf("/bfb claim name is empty")
		}
		return registryStorage{kind: "pvc", claimName: volume.PersistentVolumeClaim.ClaimName, readOnly: readOnly || volume.PersistentVolumeClaim.ReadOnly}, nil
	}
	if volume.HostPath != nil && volume.PersistentVolumeClaim == nil && volume.EmptyDir == nil && volume.ConfigMap == nil && volume.Secret == nil {
		if volume.HostPath.Path != softwareBFBHostPath || volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathDirectoryOrCreate {
			return registryStorage{}, fmt.Errorf("/bfb hostPath differs from the pinned DPF storage path")
		}
		return registryStorage{kind: "hostPath", hostPath: volume.HostPath.Path, readOnly: readOnly}, nil
	}
	return registryStorage{}, fmt.Errorf("/bfb must use exactly one supported PVC or pinned hostPath source")
}

func compareBFBStorage(registry, manager registryStorage, registryNode, managerNode string) error {
	if registryNode == "" || registryNode != managerNode || registry.kind != manager.kind || registry.readOnly != manager.readOnly {
		return fmt.Errorf("BFB registry and active manager do not share the same storage mode and node")
	}
	switch registry.kind {
	case "pvc":
		if registry.claimName == "" || registry.claimName != manager.claimName {
			return fmt.Errorf("BFB registry and active manager do not reference the same storage claim")
		}
	case "hostPath":
		if registry.hostPath != softwareBFBHostPath || registry.hostPath != manager.hostPath {
			return fmt.Errorf("BFB registry and active manager do not share the pinned hostPath")
		}
	default:
		return fmt.Errorf("BFB storage source is unsupported")
	}
	return nil
}

func validCertificateBundle(pem []byte) bool {
	pool := x509.NewCertPool()
	return pool.AppendCertsFromPEM(pem)
}

func managerRegistryImage(container *corev1.Container) (string, error) {
	var value string
	found := false
	for _, env := range container.Env {
		if env.Name != "BFB_REGISTRY_IMAGE" {
			continue
		}
		if found || env.Value == "" || env.ValueFrom != nil {
			return "", fmt.Errorf("BFB_REGISTRY_IMAGE is missing or ambiguous")
		}
		found, value = true, env.Value
	}
	if !found {
		return "", fmt.Errorf("BFB_REGISTRY_IMAGE is not configured")
	}
	return value, nil
}

func hasRegistryHTTPSPort(container *corev1.Container) bool {
	if container == nil {
		return false
	}
	found := false
	for _, port := range container.Ports {
		if port.Name == "https" && port.ContainerPort == softwareHTTPSPort && (port.Protocol == "" || port.Protocol == corev1.ProtocolTCP) {
			if found {
				return false
			}
			found = true
		}
	}
	return found
}

func validateRegistryConfigMount(pod *corev1.Pod, container *corev1.Container) error {
	const configMountPath = "/nginx/nginx.conf.template"
	const expectedCommand = "/bin/sh"
	const expectedScript = "envsubst '${NGINX_HTTPS_PORT}' < /nginx/nginx.conf.template > /nginx/nginx.conf && /usr/local/nginx/sbin/nginx -c /nginx/nginx.conf -g \"daemon off;\""
	if container == nil || container.Command == nil || len(container.Command) != 3 || container.Command[0] != expectedCommand || container.Command[1] != "-c" || container.Command[2] != expectedScript || len(container.Args) != 0 {
		return fmt.Errorf("BFB registry command or HTTPS environment differs from the pinned DPF invocation")
	}
	portEnvCount := 0
	for _, env := range container.Env {
		if env.Name == "NGINX_HTTPS_PORT" {
			portEnvCount++
			if env.Value != "8443" || env.ValueFrom != nil {
				return fmt.Errorf("BFB registry command or HTTPS environment differs from the pinned DPF invocation")
			}
		}
	}
	if portEnvCount != 1 {
		return fmt.Errorf("BFB registry command or HTTPS environment differs from the pinned DPF invocation")
	}
	var configMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.MountPath == configMountPath {
			if configMount != nil {
				return fmt.Errorf("BFB registry configuration mount is ambiguous")
			}
			configMount = mount
		}
	}
	if configMount == nil || configMount.Name == "" || configMount.SubPath != softwareRegistryConfigKey || configMount.SubPathExpr != "" || !configMount.ReadOnly {
		return fmt.Errorf("BFB registry does not mount the pinned nginx.conf configuration key")
	}
	var configVolume *corev1.Volume
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i]
		if volume.Name == configMount.Name {
			if configVolume != nil {
				return fmt.Errorf("BFB registry configuration volume is ambiguous")
			}
			configVolume = volume
		}
	}
	if configVolume == nil || configVolume.ConfigMap == nil || configVolume.PersistentVolumeClaim != nil || configVolume.HostPath != nil || configVolume.EmptyDir != nil || configVolume.Secret != nil || configVolume.Projected != nil || configVolume.ConfigMap.Name != softwareRegistryConfigName || configVolume.ConfigMap.Optional != nil && *configVolume.ConfigMap.Optional {
		return fmt.Errorf("BFB registry configuration volume does not bind the pinned ConfigMap")
	}
	if len(configVolume.ConfigMap.Items) != 0 {
		return fmt.Errorf("BFB registry configuration volume remaps the pinned ConfigMap key")
	}
	return nil
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func equalInt32(left, right *int32) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
