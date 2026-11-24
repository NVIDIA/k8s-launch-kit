// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ocpCleanReceipt struct {
	object                    *corev1.ConfigMap
	owner, digest, identities string
	originalUIDs              map[string]types.UID
	ancestryCheckpoint        string
	ancestryUID               types.UID
}

func ocpCleanReceiptName(owner string) string {
	digest := sha256.Sum256([]byte(owner))
	return fmt.Sprintf("l8k-ocp-clean-receipt-%x", digest[:12])
}

func loadOCPCleanReceipt(ctx context.Context, c client.Client, ns string, refs []cleanObjectRef) (*ocpCleanReceipt, bool, error) {
	type desiredEntry struct {
		Identity string
		Spec     interface{}
		Version  string
	}
	var desired []desiredEntry
	current := map[string]types.UID{}
	owner := ""
	for _, ref := range refs {
		object, err := ocpDesired(ref)
		if err != nil {
			return nil, false, err
		}
		identity := ocpCleanIdentity(ref.GVK, ref.Namespace, ref.Name)
		desired = append(desired, desiredEntry{Identity: identity, Spec: object.Object["spec"], Version: object.GetAnnotations()[launchKitVersionAnnotation]})
		current[identity] = ref.UID
		if marker := object.GetAnnotations()[ocpCleanOwnerAnnotation]; marker != "" {
			if owner != "" && owner != marker {
				return nil, false, fmt.Errorf("cleanup target objects have different deployment owners")
			}
			owner = marker
		}
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Identity < desired[j].Identity })
	encoded, err := json.Marshal(desired)
	if err != nil {
		return nil, false, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	// Production plans always carry the stable deployment marker. The fallback
	// also keeps internal callers without deployment metadata isolated by target.
	if owner == "" {
		owner = digest
	}
	identities, err := json.Marshal(current)
	if err != nil {
		return nil, false, err
	}
	object := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: ns, Name: ocpCleanReceiptName(owner)}
	receipt := &ocpCleanReceipt{object: object, owner: owner, digest: digest, identities: string(identities), originalUIDs: current}
	err = c.Get(ctx, key, object)
	if apierrors.IsNotFound(err) {
		object.ObjectMeta = metav1.ObjectMeta{Namespace: ns, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "l8k-clean"}}
		return receipt, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read cleanup verification receipt: %w", err)
	}
	if err := validateOCPCleanReceipt(object, owner); err != nil {
		return nil, false, err
	}
	if object.Data["desired-digest"] != digest {
		return nil, false, fmt.Errorf("cleanup receipt %s covers different retained intent; finish its cleanup with the original bundle before changing deployment", key)
	}
	var previous map[string]types.UID
	if err := json.Unmarshal([]byte(object.Data["target-uids"]), &previous); err != nil {
		return nil, false, fmt.Errorf("invalid cleanup receipt target identities: %w", err)
	}
	receipt.originalUIDs = previous
	receipt.ancestryCheckpoint = object.Data["workload-checkpoint"]
	receipt.ancestryUID = types.UID(object.Data["workload-checkpoint-uid"])
	for identity, uid := range current {
		prior, present := previous[identity]
		if !present || uid != "" && uid != prior {
			return nil, false, fmt.Errorf("cleanup receipt %s belongs to an earlier deployment incarnation; current target %s must not reuse its verification", key, identity)
		}
	}
	state := object.Data["state"]
	verified := state == "hardware-verified" || state == "resources-complete" || state == "complete"
	return receipt, verified, nil
}

func validateOCPCleanReceipt(object *corev1.ConfigMap, owner string) error {
	state := object.Data["state"]
	validState := state == "cleanup-active" || state == "hardware-verified" || state == "resources-complete" || state == "complete" || state == "resources-unverified" || state == "complete-unverified"
	if object.Labels["app.kubernetes.io/managed-by"] != "l8k-clean" || object.Data["owner"] != owner || object.Data["desired-digest"] == "" || object.Data["target-uids"] == "" || !validState || len(object.OwnerReferences) > 0 {
		return fmt.Errorf("unexpected cleanup verification receipt %s/%s; refusing to reuse or overwrite it", object.Namespace, object.Name)
	}
	return nil
}

func (receipt *ocpCleanReceipt) save(ctx context.Context, c client.Client, state string) error {
	// Keep the original incarnation set through retries after roots disappear.
	identities := receipt.identities
	if receipt.object.Data != nil && receipt.object.Data["target-uids"] != "" {
		identities = receipt.object.Data["target-uids"]
	}
	receipt.object.Data = map[string]string{"owner": receipt.owner, "desired-digest": receipt.digest, "target-uids": identities, "state": state}
	if receipt.ancestryCheckpoint != "" {
		receipt.object.Data["workload-checkpoint"] = receipt.ancestryCheckpoint
		receipt.object.Data["workload-checkpoint-uid"] = string(receipt.ancestryUID)
	}
	var err error
	if receipt.object.ResourceVersion == "" {
		err = c.Create(ctx, receipt.object)
	} else {
		err = c.Update(ctx, receipt.object)
	}
	if err != nil {
		return fmt.Errorf("save %s cleanup verification receipt: %w", state, err)
	}
	return nil
}

// invalidateOCPCleanReceipt runs before a new non-dry-run deployment applies
// intent. A partial cleanup must finish first; a complete receipt is safely
// invalidated so even identical regenerated intent cannot reuse old hardware proof.
func invalidateOCPCleanReceipt(ctx context.Context, c client.Client, namespace, owner string) error {
	object := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: ocpCleanReceiptName(owner)}
	if err := c.Get(ctx, key, object); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect cleanup receipt before deployment: %w", err)
	}
	if err := validateOCPCleanReceipt(object, owner); err != nil {
		return err
	}
	if object.Data["state"] != "complete" && object.Data["state"] != "complete-unverified" {
		return fmt.Errorf("cleanup receipt %s records an interrupted cleanup; complete cleanup with its original retained bundle before deployment", key)
	}
	if object.UID == "" || object.ResourceVersion == "" {
		return fmt.Errorf("cleanup receipt %s has no UID/resource version; refusing invalidation", key)
	}
	if err := c.Delete(ctx, object, client.Preconditions{UID: &object.UID, ResourceVersion: &object.ResourceVersion}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("invalidate completed cleanup receipt: %w", err)
	}
	return nil
}

// completeOCPCleanReceipt is the coordinator's final step after shared settings
// and optional OLM readiness have finished. Resource verification remains durable
// while those stages are incomplete, and deployment cannot race their rollback.
func completeOCPCleanReceipt(ctx context.Context, c client.Client, namespace, owner string) error {
	object := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: ocpCleanReceiptName(owner)}
	if err := c.Get(ctx, key, object); err != nil {
		return fmt.Errorf("load cleanup receipt for final completion: %w", err)
	}
	if err := validateOCPCleanReceipt(object, owner); err != nil {
		return err
	}
	switch object.Data["state"] {
	case "resources-complete", "complete":
		object.Data["state"] = "complete"
	case "resources-unverified", "complete-unverified":
		object.Data["state"] = "complete-unverified"
	default:
		return fmt.Errorf("cleanup receipt %s has not completed resource teardown", key)
	}
	if err := c.Update(ctx, object); err != nil {
		return fmt.Errorf("complete cleanup verification receipt: %w", err)
	}
	return nil
}

// Once a root disappears, an initial namespace inventory cannot recover the
// ancestry of Pods whose DaemonSet or ReplicaSet was already collected. Only a
// prior verified phase with an original root UID and durable ancestry can bridge
// that gap. A waiver or an in-progress receipt is deliberately not such proof.
func requireOCPMissingRootProof(ctx context.Context, c client.Client, refs []cleanObjectRef, receipt *ocpCleanReceipt, verified bool) error {
	for _, ref := range refs {
		if !isCleanRootPolicy(ref.GVK) {
			continue
		}
		live, err := inspectOCPDeletionTarget(ctx, c, ref)
		if err != nil {
			return err
		}
		if live != nil {
			continue
		}
		identity := ocpCleanIdentity(ref.GVK, ref.Namespace, ref.Name)
		if !verified || receipt.originalUIDs[identity] == "" || receipt.ancestryCheckpoint == "" || receipt.ancestryUID == "" {
			return fmt.Errorf("selected %s is absent without a prior verified root UID and workload ancestry; no cleanup verification receipt can qualify that missing history; inspect affected hosts and use --skip-wait only to explicitly waive that evidence", cleanRefString(ref))
		}
		if receipt.object.Data["state"] == "hardware-verified" {
			checkpoint := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: receipt.object.Namespace, Name: receipt.ancestryCheckpoint}
			if err := c.Get(ctx, key, checkpoint); err != nil {
				return fmt.Errorf("selected %s is absent and its workload ancestry checkpoint is unavailable: %w", cleanRefString(ref), err)
			}
			if checkpoint.UID != receipt.ancestryUID || checkpoint.Labels["app.kubernetes.io/managed-by"] != "l8k-clean" || checkpoint.Data["_ancestry-recorded"] != "true" || len(checkpoint.OwnerReferences) > 0 {
				return fmt.Errorf("selected %s is absent and its recorded workload ancestry changed; default cleanup cannot verify the missing history", cleanRefString(ref))
			}
		}
	}
	return nil
}
