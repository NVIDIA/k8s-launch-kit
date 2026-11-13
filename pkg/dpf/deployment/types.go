// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package deployment implements the existing-infrastructure DPF lifecycle slice.
// It creates native objects only; ownership transitions, registration, Helm,
// firmware actions and host reboot commands are not implicit remediation.
package deployment

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Mode string

const (
	Deploy   Mode = "deploy"
	Validate Mode = "validate"
	DryRun   Mode = "dry-run"
)

// Client hides private access credentials and exposes no update/delete operation.
type Client interface {
	ServerVersion(context.Context) (string, error)
	Get(context.Context, bundle.ResourceRef) (*unstructured.Unstructured, error)
	List(context.Context, bundle.ResourceRef, string) ([]*unstructured.Unstructured, error)
	Create(context.Context, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error)
	SecretKeys(context.Context, string, string, []string) error
	VerifyBMC(context.Context, config.Generation, config.DeviceBinding, inventory.Device, string) error
}

type ClientFactory interface {
	Management(context.Context, config.Generation) (Client, error)
	Tenant(context.Context, Client, config.ClusterBinding) (Client, error)
}

// ArtifactVerifier separates source bytes from controller-consumed cached bytes.
// Cached must establish the latter before a DPUSet can cause hardware effects.
type ArtifactVerifier interface {
	Cached(context.Context, Client, config.Generation, *unstructured.Unstructured) error
}

type Check struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	Source     string `json:"source"`
	State      string `json:"state"`
	ReasonCode string `json:"reasonCode"`
	Message    string `json:"message,omitempty"`
}

type Event struct {
	At              time.Time          `json:"at"`
	Stage           string             `json:"stage"`
	Action          string             `json:"action"`
	Resource        bundle.ResourceRef `json:"resource"`
	UID             string             `json:"uid,omitempty"`
	ResourceVersion string             `json:"resourceVersion,omitempty"`
}

type Result struct {
	Mode         Mode    `json:"mode"`
	BundleDigest string  `json:"bundleDigest"`
	Outcome      string  `json:"outcome"`
	Checks       []Check `json:"checks"`
	Events       []Event `json:"events"`
	ReportPath   string  `json:"reportPath"`
}

// Runner requires a private evidence directory outside the immutable bundle.
// Zero client/verifier fields select the native adapters. The cached-artifact
// gate must succeed before any new DPUSet can cause physical-device effects.
type Runner struct {
	Clients      ClientFactory
	Artifacts    ArtifactVerifier
	EvidenceDir  string
	PollInterval time.Duration
	Timeout      time.Duration
	Wait         bool
}
