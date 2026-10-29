// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package inventory defines secret-free DPF observations and check results.
package inventory

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

const FormatVersion = "v1alpha1"

type Collection struct {
	RunID      string    `json:"runID" yaml:"runID"`
	StartedAt  time.Time `json:"startedAt" yaml:"startedAt"`
	FinishedAt time.Time `json:"finishedAt" yaml:"finishedAt"`
	Mode       string    `json:"mode" yaml:"mode"`
	Collector  string    `json:"collector" yaml:"collector"`
}
type ScopeEndpoint struct {
	Role      string `json:"role" yaml:"role"`
	Endpoint  string `json:"endpoint" yaml:"endpoint"`
	SystemURI string `json:"systemURI,omitempty" yaml:"systemURI,omitempty"`
}
type Evidence struct {
	Namespace           string    `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	PodUID              string    `json:"podUID,omitempty" yaml:"podUID,omitempty"`
	NodeUID             string    `json:"nodeUID,omitempty" yaml:"nodeUID,omitempty"`
	ImageDigest         string    `json:"imageDigest,omitempty" yaml:"imageDigest,omitempty"`
	DNSPolicy           string    `json:"dnsPolicy,omitempty" yaml:"dnsPolicy,omitempty"`
	DestinationEndpoint string    `json:"destinationEndpoint,omitempty" yaml:"destinationEndpoint,omitempty"`
	Kind                string    `json:"kind" yaml:"kind"`
	InputRef            string    `json:"inputRef,omitempty" yaml:"inputRef,omitempty"`
	ResourceURI         string    `json:"resourceURI,omitempty" yaml:"resourceURI,omitempty"`
	Property            string    `json:"property,omitempty" yaml:"property,omitempty"`
	ObservedAt          time.Time `json:"observedAt" yaml:"observedAt"`
	Vantage             string    `json:"vantage" yaml:"vantage"`
	Adapter             string    `json:"adapter,omitempty" yaml:"adapter,omitempty"`
	Authenticated       bool      `json:"authenticated" yaml:"authenticated"`
}
type Firmware struct {
	ResourceURI  string   `json:"resourceURI,omitempty" yaml:"resourceURI,omitempty"`
	Component    string   `json:"component" yaml:"component"`
	State        string   `json:"state" yaml:"state"`
	RawVersion   string   `json:"rawVersion,omitempty" yaml:"rawVersion,omitempty"`
	Activation   string   `json:"activation" yaml:"activation"`
	ReasonCode   string   `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	EvidenceRefs []string `json:"evidenceRefs" yaml:"evidenceRefs"`
}
type Authentication struct {
	PasswordRead             string `json:"passwordRead" yaml:"passwordRead"`
	Method                   string `json:"method,omitempty" yaml:"method,omitempty"`
	ClientCertificateSupport string `json:"clientCertificateSupport" yaml:"clientCertificateSupport"`
	DPFClientAuthentication  string `json:"dpfClientAuthentication" yaml:"dpfClientAuthentication"`
}
type TimeObservation struct {
	State      string   `json:"state" yaml:"state"`
	DateTime   string   `json:"dateTime,omitempty" yaml:"dateTime,omitempty"`
	Offset     string   `json:"offset,omitempty" yaml:"offset,omitempty"`
	NTPEnabled *bool    `json:"ntpEnabled,omitempty" yaml:"ntpEnabled,omitempty"`
	NTPServers []string `json:"ntpServers,omitempty" yaml:"ntpServers,omitempty"`
	ReasonCode string   `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
}
type Adapter struct {
	Key             string   `json:"adapterKey" yaml:"adapterKey"`
	SerialNumber    string   `json:"serialNumber" yaml:"serialNumber"`
	SerialNumberRaw string   `json:"serialNumberRaw" yaml:"serialNumberRaw"`
	Model           string   `json:"model,omitempty" yaml:"model,omitempty"`
	Slot            string   `json:"slot,omitempty" yaml:"slot,omitempty"`
	Selection       string   `json:"selection" yaml:"selection"`
	EvidenceRefs    []string `json:"evidenceRefs" yaml:"evidenceRefs"`
}

// Device represents every requested endpoint, including failed observations.
type Device struct {
	RecordKey             string          `json:"recordKey" yaml:"recordKey"`
	InputRef              string          `json:"inputRef" yaml:"inputRef"`
	BMCEndpoint           string          `json:"bmcEndpoint" yaml:"bmcEndpoint"`
	SystemURI             string          `json:"systemURI,omitempty" yaml:"systemURI,omitempty"`
	ObservationState      string          `json:"observationState" yaml:"observationState"`
	ReasonCode            string          `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	ID                    string          `json:"id,omitempty" yaml:"id,omitempty"`
	IdentityKey           []string        `json:"identityKey,omitempty" yaml:"identityKey,omitempty"`
	SerialNumber          string          `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	SerialNumberRaw       string          `json:"serialNumberRaw,omitempty" yaml:"serialNumberRaw,omitempty"`
	SystemUUID            string          `json:"systemUUID,omitempty" yaml:"systemUUID,omitempty"`
	Manufacturer          string          `json:"manufacturer,omitempty" yaml:"manufacturer,omitempty"`
	Model                 string          `json:"model,omitempty" yaml:"model,omitempty"`
	ReaderAdapter         string          `json:"readerAdapter,omitempty" yaml:"readerAdapter,omitempty"`
	InventoryCompleteness string          `json:"inventoryCompleteness,omitempty" yaml:"inventoryCompleteness,omitempty"`
	Qualification         string          `json:"qualification" yaml:"qualification"`
	Firmware              []Firmware      `json:"firmware" yaml:"firmware"`
	Authentication        Authentication  `json:"authentication" yaml:"authentication"`
	Time                  TimeObservation `json:"time" yaml:"time"`
	Adapters              []Adapter       `json:"adapters,omitempty" yaml:"adapters,omitempty"`
	AdvertisedResetTypes  []string        `json:"advertisedResetTypes,omitempty" yaml:"advertisedResetTypes,omitempty"`
	EvidenceRefs          []string        `json:"evidenceRefs" yaml:"evidenceRefs"`
}
type Node struct {
	Interfaces    []Interface `json:"interfaces,omitempty" yaml:"interfaces,omitempty"`
	Routes        []Route     `json:"routes,omitempty" yaml:"routes,omitempty"`
	Name          string      `json:"name" yaml:"name"`
	UID           string      `json:"uid" yaml:"uid"`
	ControlPlane  bool        `json:"controlPlane" yaml:"controlPlane"`
	Ready         bool        `json:"ready" yaml:"ready"`
	Unschedulable bool        `json:"unschedulable" yaml:"unschedulable"`
	Architecture  string      `json:"architecture" yaml:"architecture"`
	OS            string      `json:"os" yaml:"os"`
	Addresses     []string    `json:"addresses" yaml:"addresses"`
}
type Observation struct {
	State      string   `json:"state" yaml:"state"`
	Values     []string `json:"values" yaml:"values"`
	ReasonCode string   `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
}
type Management struct {
	Observations       map[string]Observation `json:"observations,omitempty" yaml:"observations,omitempty"`
	APIEndpoint        string                 `json:"apiEndpoint" yaml:"apiEndpoint"`
	SystemNamespaceUID string                 `json:"systemNamespaceUID" yaml:"systemNamespaceUID"`
	TrustDigest        string                 `json:"trustDigest" yaml:"trustDigest"`
	KubernetesVersion  string                 `json:"kubernetesVersion" yaml:"kubernetesVersion"`
	Namespace          string                 `json:"namespace" yaml:"namespace"`
	Nodes              []Node                 `json:"nodes" yaml:"nodes"`
	EvidenceRefs       []string               `json:"evidenceRefs" yaml:"evidenceRefs"`
}
type Association struct {
	DPUID          string   `json:"dpuID" yaml:"dpuID"`
	HostID         string   `json:"hostID" yaml:"hostID"`
	HostAdapterKey string   `json:"hostAdapterKey" yaml:"hostAdapterKey"`
	SerialNumber   string   `json:"serialNumber" yaml:"serialNumber"`
	EvidenceRefs   []string `json:"evidenceRefs" yaml:"evidenceRefs"`
}
type Inventory struct {
	FormatVersion  string              `json:"formatVersion" yaml:"formatVersion"`
	Collection     Collection          `json:"collection" yaml:"collection"`
	RequestedScope []ScopeEndpoint     `json:"requestedScope" yaml:"requestedScope"`
	ScopeDigest    string              `json:"scopeDigest,omitempty" yaml:"scopeDigest,omitempty"`
	Fingerprint    string              `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`
	Management     *Management         `json:"management,omitempty" yaml:"management,omitempty"`
	DPUs           []Device            `json:"dpus" yaml:"dpus"`
	Hosts          []Device            `json:"hosts" yaml:"hosts"`
	Associations   []Association       `json:"associations" yaml:"associations"`
	Probes         []ProbeResult       `json:"probes,omitempty" yaml:"probes,omitempty"`
	Evidence       map[string]Evidence `json:"evidence" yaml:"evidence"`
}
type Check struct {
	ID            string   `json:"id"`
	Subject       string   `json:"subject"`
	Vantage       string   `json:"vantage"`
	RequiredPhase string   `json:"requiredPhase"`
	State         string   `json:"state"`
	ReasonCode    string   `json:"reasonCode"`
	EvidenceRefs  []string `json:"evidenceRefs"`
}
type Problem struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	InputRef string `json:"inputRef,omitempty"`
	ExitCode int    `json:"-"`
}
type Artifact struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}
type Change struct {
	Path          string `json:"path"`
	State         string `json:"state"`
	Previous      any    `json:"previous,omitempty"`
	Current       any    `json:"current,omitempty"`
	RequiredPhase string `json:"requiredPhase"`
}
type Cleanup struct {
	State     string   `json:"state"`
	Leftovers []string `json:"leftovers"`
}
type Discovery struct {
	Outcome string  `json:"outcome"`
	Mode    string  `json:"mode"`
	Checks  []Check `json:"checks"`
	Cleanup Cleanup `json:"cleanup"`
}
type Report struct {
	SchemaVersion string     `json:"schemaVersion"`
	Target        string     `json:"target"`
	Phase         string     `json:"phase"`
	Execution     string     `json:"execution"`
	Acceptance    string     `json:"acceptance"`
	Inventory     *Inventory `json:"inventory,omitempty"`
	Discovery     Discovery  `json:"discovery"`
	Diff          []Change   `json:"diff"`
	PendingInputs []Problem  `json:"pendingInputs"`
	Artifacts     []Artifact `json:"artifacts"`
	Errors        []Problem  `json:"errors"`
}

// Digest hashes an explicit, versioned projection, never raw vendor documents.
func Digest(version string, value any) string {
	b, _ := json.Marshal(value)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(append([]byte(version+"\n"), b...)))
}
func PhysicalID(role string, key []string) string {
	return role + "-" + Digest("dpf-identity-v1", key)[7:]
}
