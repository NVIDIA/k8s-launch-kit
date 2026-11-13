// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

// HBNManagement contains bounded management-cluster facts needed to plan HBN
// deployment and preserve existing DPF ownership. It omits raw objects,
// collection wrappers, credentials, and timestamps.
type HBNManagement struct {
	Namespace       string                    `json:"namespace" yaml:"namespace"`
	Operator        *HBNManagementOperator    `json:"operator,omitempty" yaml:"operator,omitempty"`
	OperatorConfigs []HBNManagementConfig     `json:"operatorConfigs,omitempty" yaml:"operatorConfigs,omitempty"`
	TenantClusters  []HBNManagementCluster    `json:"tenantClusters,omitempty" yaml:"tenantClusters,omitempty"`
	Enrollments     []HBNManagementEnrollment `json:"enrollments,omitempty" yaml:"enrollments,omitempty"`
	Issues          []HBNManagementIssue      `json:"issues,omitempty" yaml:"issues,omitempty"`
}

type HBNManagementOperator struct {
	Name            string `json:"name" yaml:"name"`
	UID             string `json:"uid,omitempty" yaml:"uid,omitempty"`
	Image           string `json:"image,omitempty" yaml:"image,omitempty"`
	Version         string `json:"version,omitempty" yaml:"version,omitempty"`
	Ready           *bool  `json:"ready,omitempty" yaml:"ready,omitempty"`
	ReadyReplicas   int32  `json:"readyReplicas" yaml:"readyReplicas"`
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty" yaml:"desiredReplicas,omitempty"`
}

type HBNManagementConfig struct {
	Name           string   `json:"name" yaml:"name"`
	UID            string   `json:"uid,omitempty" yaml:"uid,omitempty"`
	DeploymentMode string   `json:"deploymentMode,omitempty" yaml:"deploymentMode,omitempty"`
	Ready          *bool    `json:"ready,omitempty" yaml:"ready,omitempty"`
	Owners         []string `json:"owners,omitempty" yaml:"owners,omitempty"`
}

type HBNManagementCluster struct {
	Name                      string   `json:"name" yaml:"name"`
	Namespace                 string   `json:"namespace" yaml:"namespace"`
	UID                       string   `json:"uid,omitempty" yaml:"uid,omitempty"`
	ResourceVersion           string   `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
	Type                      string   `json:"type,omitempty" yaml:"type,omitempty"`
	Phase                     string   `json:"phase,omitempty" yaml:"phase,omitempty"`
	Version                   string   `json:"version,omitempty" yaml:"version,omitempty"`
	VIP                       string   `json:"vip,omitempty" yaml:"vip,omitempty"`
	Interface                 string   `json:"interface,omitempty" yaml:"interface,omitempty"`
	VirtualRouterID           *int64   `json:"virtualRouterID,omitempty" yaml:"virtualRouterID,omitempty"`
	LaunchKitTenantLabelKey   string   `json:"launchKitTenantLabelKey,omitempty" yaml:"launchKitTenantLabelKey,omitempty"`
	LaunchKitTenantLabelValue string   `json:"launchKitTenantLabelValue,omitempty" yaml:"launchKitTenantLabelValue,omitempty"`
	Owners                    []string `json:"owners,omitempty" yaml:"owners,omitempty"`
}

type HBNManagementEnrollment struct {
	Namespace    string   `json:"namespace" yaml:"namespace"`
	Kind         string   `json:"kind" yaml:"kind"`
	Name         string   `json:"name" yaml:"name"`
	UID          string   `json:"uid,omitempty" yaml:"uid,omitempty"`
	SerialNumber string   `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	PSID         string   `json:"psid,omitempty" yaml:"psid,omitempty"`
	DeviceName   string   `json:"deviceName,omitempty" yaml:"deviceName,omitempty"`
	NodeName     string   `json:"nodeName,omitempty" yaml:"nodeName,omitempty"`
	Owners       []string `json:"owners,omitempty" yaml:"owners,omitempty"`
}

type HBNManagementIssue struct {
	Source    string `json:"source" yaml:"source"`
	Resource  string `json:"resource,omitempty" yaml:"resource,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Field     string `json:"field,omitempty" yaml:"field,omitempty"`
	Code      string `json:"code" yaml:"code"`
	Message   string `json:"message" yaml:"message"`
}
