// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package bundle

// kindScope is deliberately closed. A renderer must add a reviewed API kind here
// before a new cohort can rely on namespace/collision checks for that kind.
var kindScope = map[string]bool{
	"/Namespace": false, "/Node": false, "/PersistentVolume": false,
	"/ConfigMap": true, "/Secret": true, "/Service": true, "/ServiceAccount": true,
	"/Pod": true, "/PersistentVolumeClaim": true, "/ResourceQuota": true, "/LimitRange": true,
	"apps/Deployment": true, "apps/DaemonSet": true, "apps/StatefulSet": true,
	"batch/Job": true, "batch/CronJob": true,
	"networking.k8s.io/NetworkPolicy": true, "networking.k8s.io/Ingress": true,
	"rbac.authorization.k8s.io/Role": true, "rbac.authorization.k8s.io/RoleBinding": true,
	"rbac.authorization.k8s.io/ClusterRole": false, "rbac.authorization.k8s.io/ClusterRoleBinding": false,
	"apiextensions.k8s.io/CustomResourceDefinition":                 false,
	"admissionregistration.k8s.io/MutatingWebhookConfiguration":     false,
	"admissionregistration.k8s.io/ValidatingWebhookConfiguration":   false,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicy":        false,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding": false,
	"nfd.k8s-sigs.io/NodeFeatureRule":                               false,
	"storage.k8s.io/StorageClass":                                   false,
	"cert-manager.io/Certificate":                                   true, "cert-manager.io/CertificateRequest": true,
	"cert-manager.io/Issuer": true, "cert-manager.io/ClusterIssuer": false,
	"k8s.cni.cncf.io/NetworkAttachmentDefinition":    true,
	"operator.dpu.nvidia.com/DPFOperatorConfig":      true,
	"provisioning.dpu.nvidia.com/BFB":                true,
	"provisioning.dpu.nvidia.com/BlueFieldSoftware":  true,
	"provisioning.dpu.nvidia.com/DPU":                true,
	"provisioning.dpu.nvidia.com/DPUCluster":         true,
	"provisioning.dpu.nvidia.com/DPUDevice":          true,
	"provisioning.dpu.nvidia.com/DPUDiscovery":       true,
	"provisioning.dpu.nvidia.com/DPUFlavor":          true,
	"provisioning.dpu.nvidia.com/DPUFlavorTemplate":  true,
	"provisioning.dpu.nvidia.com/DPUNode":            true,
	"provisioning.dpu.nvidia.com/DPUNodeMaintenance": true,
	"provisioning.dpu.nvidia.com/DPUSet":             true,
	"svc.dpu.nvidia.com/DPUDeployment":               true,
	"svc.dpu.nvidia.com/DPUService":                  true,
	"svc.dpu.nvidia.com/DPUServiceChain":             true,
	"svc.dpu.nvidia.com/DPUServiceConfiguration":     true,
	"svc.dpu.nvidia.com/DPUServiceCredentialRequest": true,
	"svc.dpu.nvidia.com/DPUServiceInterface":         true,
	"svc.dpu.nvidia.com/DPUServiceIPAM":              true,
	"svc.dpu.nvidia.com/DPUServiceNAD":               true,
	"svc.dpu.nvidia.com/DPUServiceTemplate":          true,
}

// ResourceNamespaced reports the reviewed Kubernetes scope for a known kind.
// The second result is false when no scope decision exists for the kind.
func ResourceNamespaced(group, kind string) (bool, bool) {
	namespaced, known := kindScope[group+"/"+kind]
	return namespaced, known
}
