// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type softwareVerificationClient interface {
	resourceReader
	VerifyIdentity(context.Context) error
	observeRegistrySnapshot(context.Context, string) (registrySnapshot, error)
	openSoftwareRegistry(context.Context, string, []byte) (*registryTunnel, error)
}

type softwareIntentVerifier interface {
	Verify(*unstructured.Unstructured, *unstructured.Unstructured) error
}

type softwareVerificationReceipt struct {
	SoftwareUID types.UID
	Generation  int64
	VerifiedAt  time.Time
	Components  []verifiedSoftwareComponent
}

type verifiedSoftwareComponent struct {
	Component string
	PSID      string
	SHA256    string
	Bytes     int64
}

func (c *APIClient) openSoftwareRegistry(ctx context.Context, namespace string, caPEM []byte) (*registryTunnel, error) {
	if c == nil {
		return nil, fmt.Errorf("bound management client is required")
	}
	return openRegistryTunnel(ctx, c.access.REST, namespace, caPEM)
}

// verifyStagedSoftware checks the cache currently served by DPF, not the source
// URL. The caller first creates/verifies the retained BlueFieldSoftware and
// waits for its controller. No resource is created or adopted by this check.
// The result expires as an observation; it cannot guarantee the contents of a
// later asynchronous BMC download or PLDM upload.
func verifyStagedSoftware(ctx context.Context, client softwareVerificationClient, reconciler softwareIntentVerifier,
	desired, current *unstructured.Unstructured, locks config.Software, selectedPSIDs []string,
	registryNamespace, expectedManagerImage string, maxBytes int64,
) (softwareVerificationReceipt, error) {
	if ctx == nil || client == nil || reconciler == nil || desired == nil || current == nil || registryNamespace == "" || expectedManagerImage == "" || maxBytes <= 0 {
		return softwareVerificationReceipt{}, fmt.Errorf("staged software verification requires a bound client, retained resource and locks, registry namespace, expected manager image and positive size limit")
	}
	if _, ok := ctx.Deadline(); !ok {
		return softwareVerificationReceipt{}, fmt.Errorf("staged software verification requires a deadline")
	}
	wanted, initial := desired.DeepCopy(), current.DeepCopy()
	if err := reconciler.Verify(wanted.DeepCopy(), initial.DeepCopy()); err != nil {
		return softwareVerificationReceipt{}, fmt.Errorf("software intent or ownership does not match the retained bundle")
	}
	if _, err := softwareCacheTargets(wanted, initial, locks, selectedPSIDs); err != nil {
		return softwareVerificationReceipt{}, err
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return softwareVerificationReceipt{}, fmt.Errorf("management identity changed before software verification")
	}
	// Retain the UID/generation from the completed create/wait boundary. Never
	// accept a replacement simply because the new object is independently Ready.
	before, targets, err := refreshSoftwareTargets(ctx, client, reconciler, wanted, initial, locks, selectedPSIDs)
	if err != nil {
		return softwareVerificationReceipt{}, err
	}
	registry, err := client.observeRegistrySnapshot(ctx, registryNamespace)
	if err != nil {
		return softwareVerificationReceipt{}, err
	}
	// DPF v26.8 maps the provisioning manager to the release's dpfSystemImage.
	if registry.manager.container.image != expectedManagerImage {
		return softwareVerificationReceipt{}, fmt.Errorf("active provisioning manager image does not match the selected DPF release")
	}
	tunnel, err := client.openSoftwareRegistry(ctx, registryNamespace, slices.Clone(registry.caPEM))
	if err != nil {
		// The native tunnel adapter returns only bounded local diagnostics and
		// context errors. Preserve cleanup uncertainty as well as cancellation.
		return softwareVerificationReceipt{}, fmt.Errorf("open verified software registry tunnel: %w", err)
	}
	if tunnel == nil {
		return softwareVerificationReceipt{}, fmt.Errorf("software registry returned no tunnel")
	}
	// Close is idempotent, including error paths. Successful cleanup is required
	// before issuing the completed receipt below.
	defer func() { _ = tunnel.Close() }()
	receipt := softwareVerificationReceipt{SoftwareUID: before.GetUID(), Generation: before.GetGeneration()}
	for _, target := range targets {
		requestURL := (&url.URL{Scheme: "https", Host: fmt.Sprintf("127.0.0.1:%d", tunnel.port), Path: target.cachePath}).String()
		count, readErr := verifyRegistryStream(ctx, tunnel.client, requestURL, target.sha256, maxBytes)
		if readErr != nil {
			return softwareVerificationReceipt{}, errors.Join(readErr, tunnel.Close())
		}
		receipt.Components = append(receipt.Components, verifiedSoftwareComponent{
			Component: target.component, PSID: target.psid, SHA256: target.sha256, Bytes: count,
		})
	}
	if err := tunnel.Close(); err != nil {
		return softwareVerificationReceipt{}, err
	}
	afterRegistry, err := client.observeRegistrySnapshot(ctx, registryNamespace)
	if err != nil {
		return softwareVerificationReceipt{}, err
	}
	if !registry.sameBinding(afterRegistry) {
		return softwareVerificationReceipt{}, fmt.Errorf("software registry, active controller, storage or public CA changed while reading staged bytes")
	}
	_, afterTargets, err := refreshSoftwareTargets(ctx, client, reconciler, wanted, before, locks, selectedPSIDs)
	if err != nil {
		return softwareVerificationReceipt{}, err
	}
	if !reflect.DeepEqual(targets, afterTargets) {
		return softwareVerificationReceipt{}, fmt.Errorf("software component paths or retained locks changed during verification")
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return softwareVerificationReceipt{}, fmt.Errorf("management identity changed during software verification")
	}
	if err := ctx.Err(); err != nil {
		return softwareVerificationReceipt{}, err
	}
	receipt.VerifiedAt = time.Now().UTC()
	return receipt, nil
}

func refreshSoftwareTargets(ctx context.Context, client resourceReader, reconciler softwareIntentVerifier,
	desired, previous *unstructured.Unstructured, locks config.Software, selectedPSIDs []string,
) (*unstructured.Unstructured, []softwareCacheTarget, error) {
	current, err := client.Get(ctx, desired.DeepCopy())
	if err != nil {
		return nil, nil, safeAPIError("read staged software identity", err)
	}
	if current == nil || current.GetUID() != previous.GetUID() || current.GetGeneration() != previous.GetGeneration() {
		return nil, nil, fmt.Errorf("staged software UID or generation changed after controller readiness")
	}
	if err := reconciler.Verify(desired.DeepCopy(), current.DeepCopy()); err != nil {
		return nil, nil, fmt.Errorf("staged software ownership or retained intent changed")
	}
	targets, err := softwareCacheTargets(desired, current, locks, selectedPSIDs)
	return current, targets, err
}
