/*
Copyright 2026 The kro-fleet Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// The applied-manifest inventory: one AppliedManifestRecord per
// (FleetGenAIService, member), keyed by GVK/namespace/name. Everything the
// controller writes to a member is recorded here BEFORE the write (intent),
// confirmed after it (status.applied), and removed from the member exactly
// by this record on unplacement, pruning and finalization.

// RecordName derives the record name for an (instance, member) pair. Names
// are DNS subdomains (<= 253 chars); longer pairs are truncated and suffixed
// with a hash so they stay unique.
func RecordName(instance, cluster string) string {
	name := instance + "-" + cluster
	if len(name) <= validation.DNS1123SubdomainMaxLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:10]
	return name[:validation.DNS1123SubdomainMaxLength-len(suffix)-1] + "-" + suffix
}

// identifierOf returns the inventory key of an object.
func identifierOf(obj *unstructured.Unstructured) fleetv1alpha1.ManifestIdentifier {
	gvk := obj.GroupVersionKind()
	return fleetv1alpha1.ManifestIdentifier{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		Namespace: obj.GetNamespace(), Name: obj.GetName(),
	}
}

// gvkOf returns the GroupVersionKind of an inventory key.
func gvkOf(id fleetv1alpha1.ManifestIdentifier) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: id.Group, Version: id.Version, Kind: id.Kind}
}

// sameManifestSet reports whether two identifier lists hold the same keys.
func sameManifestSet(a, b []fleetv1alpha1.ManifestIdentifier) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[fleetv1alpha1.ManifestIdentifier]bool{}
	for _, id := range a {
		set[id] = true
	}
	for _, id := range b {
		if !set[id] {
			return false
		}
	}
	return true
}

// listRecords returns the records of one FleetGenAIService, keyed by member
// ClusterProfile name.
func (r *FleetReconciler) listRecords(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService) (map[string]*fleetv1alpha1.AppliedManifestRecord, error) {
	hub := r.Manager.GetLocalManager().GetClient()
	list := &fleetv1alpha1.AppliedManifestRecordList{}
	if err := hub.List(ctx, list, client.InNamespace(fgs.Namespace), client.MatchingLabels{fleetv1alpha1.InstanceLabel: fgs.Name}); err != nil {
		return nil, fmt.Errorf("failed to list AppliedManifestRecords: %w", err)
	}
	out := map[string]*fleetv1alpha1.AppliedManifestRecord{}
	for i := range list.Items {
		rec := &list.Items[i]
		out[rec.Spec.Cluster.Name] = rec
	}
	return out, nil
}

// ensureRecord creates (or updates the intent of) the record for one member
// so that the intended manifest set is persisted before any member write.
func (r *FleetReconciler) ensureRecord(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService, existing *fleetv1alpha1.AppliedManifestRecord, member string, intended []fleetv1alpha1.ManifestIdentifier) (*fleetv1alpha1.AppliedManifestRecord, error) {
	hub := r.Manager.GetLocalManager().GetClient()
	if existing == nil {
		rec := &fleetv1alpha1.AppliedManifestRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:      RecordName(fgs.Name, member),
				Namespace: fgs.Namespace,
				Labels: map[string]string{
					fleetv1alpha1.InstanceLabel: fgs.Name,
					fleetv1alpha1.ClusterLabel:  member,
				},
			},
			Spec: fleetv1alpha1.AppliedManifestRecordSpec{
				InstanceRef: fleetv1alpha1.LocalObjectReference{Name: fgs.Name},
				Cluster:     fleetv1alpha1.ClusterReference{Namespace: r.FleetNamespace, Name: member},
				Manifests:   intended,
			},
		}
		if err := hub.Create(ctx, rec); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("failed to record placement intent for %s: %w", member, err)
			}
			if err := hub.Get(ctx, client.ObjectKeyFromObject(rec), rec); err != nil {
				return nil, err
			}
			return r.ensureRecord(ctx, fgs, rec, member, intended)
		}
		return rec, nil
	}
	if sameManifestSet(existing.Spec.Manifests, intended) {
		return existing, nil
	}
	existing.Spec.Manifests = intended
	if err := hub.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("failed to update placement intent for %s: %w", member, err)
	}
	return existing, nil
}

// confirmApplied records a manifest as applied on the member (status.applied),
// replacing any stale entry with the same key.
func (r *FleetReconciler) confirmApplied(ctx context.Context, rec *fleetv1alpha1.AppliedManifestRecord, applied []fleetv1alpha1.AppliedManifest) error {
	hub := r.Manager.GetLocalManager().GetClient()
	changed := false
	for _, a := range applied {
		found := false
		for i := range rec.Status.Applied {
			if rec.Status.Applied[i].ManifestIdentifier == a.ManifestIdentifier {
				found = true
				if rec.Status.Applied[i].UID != a.UID || !sameParams(rec.Status.Applied[i].Parameters, a.Parameters) {
					rec.Status.Applied[i] = a
					changed = true
				}
			}
		}
		if !found {
			rec.Status.Applied = append(rec.Status.Applied, a)
			changed = true
		}
	}
	sort.Slice(rec.Status.Applied, func(i, j int) bool {
		return manifestKey(rec.Status.Applied[i].ManifestIdentifier) < manifestKey(rec.Status.Applied[j].ManifestIdentifier)
	})
	allApplied := len(rec.Status.Applied) >= len(rec.Spec.Manifests)
	cond := metav1.Condition{Type: fleetv1alpha1.RecordConditionApplied, Status: metav1.ConditionFalse, Reason: "Applying",
		Message: fmt.Sprintf("%d/%d intended manifests confirmed", len(rec.Status.Applied), len(rec.Spec.Manifests))}
	if allApplied {
		cond.Status, cond.Reason = metav1.ConditionTrue, "AllApplied"
	}
	if meta.SetStatusCondition(&rec.Status.Conditions, cond) {
		changed = true
	}
	if !changed {
		return nil
	}
	if err := hub.Status().Update(ctx, rec); err != nil {
		return fmt.Errorf("failed to confirm applied manifests: %w", err)
	}
	return nil
}

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func manifestKey(id fleetv1alpha1.ManifestIdentifier) string {
	return id.Group + "/" + id.Version + "/" + id.Kind + "/" + id.Namespace + "/" + id.Name
}

// trackedManifests is the union of intent and confirmed entries: everything
// that might exist on the member because of this record.
func trackedManifests(rec *fleetv1alpha1.AppliedManifestRecord) []fleetv1alpha1.AppliedManifest {
	var out []fleetv1alpha1.AppliedManifest
	seen := map[fleetv1alpha1.ManifestIdentifier]bool{}
	for _, a := range rec.Status.Applied {
		seen[a.ManifestIdentifier] = true
		out = append(out, a)
	}
	for _, id := range rec.Spec.Manifests {
		if !seen[id] {
			out = append(out, fleetv1alpha1.AppliedManifest{ManifestIdentifier: id})
		}
	}
	return out
}

// deleteManifests removes the given objects from a member. It only deletes
// objects that carry this instance's placed-by label (and the recorded UID
// when known), so a same-named object someone else created is left alone.
// Returns the entries that are confirmed gone.
func deleteManifests(ctx context.Context, cl client.Client, instance string, manifests []fleetv1alpha1.AppliedManifest) ([]fleetv1alpha1.ManifestIdentifier, error) {
	log := ctrllog.FromContext(ctx)
	var gone []fleetv1alpha1.ManifestIdentifier
	for _, m := range manifests {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvkOf(m.ManifestIdentifier))
		err := cl.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Name}, obj)
		switch {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			// Absent, or the CRD never existed on this member: nothing to do.
			gone = append(gone, m.ManifestIdentifier)
			continue
		case err != nil:
			return gone, fmt.Errorf("failed to read %s %s/%s: %w", m.Kind, m.Namespace, m.Name, err)
		}
		if obj.GetLabels()[PlacedByLabel] != instance {
			log.Info("not deleting: object is not placed by this instance", "kind", m.Kind, "name", m.Name, "placedBy", obj.GetLabels()[PlacedByLabel])
			gone = append(gone, m.ManifestIdentifier)
			continue
		}
		if m.UID != "" && obj.GetUID() != m.UID {
			log.Info("not deleting: object UID differs from the recorded one", "kind", m.Kind, "name", m.Name)
			gone = append(gone, m.ManifestIdentifier)
			continue
		}
		if !obj.GetDeletionTimestamp().IsZero() {
			continue // deletion in progress; confirm on a later pass
		}
		uid := obj.GetUID()
		if err := cl.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsConflict(err) { // UID precondition failed: replaced by someone else
				gone = append(gone, m.ManifestIdentifier)
				continue
			}
			return gone, fmt.Errorf("failed to delete %s %s/%s: %w", m.Kind, m.Namespace, m.Name, err)
		}
	}
	return gone, nil
}

// dropFromRecord removes confirmed-gone entries from a record's status and
// persists it. Returns true when nothing tracked remains.
func (r *FleetReconciler) dropFromRecord(ctx context.Context, rec *fleetv1alpha1.AppliedManifestRecord, gone []fleetv1alpha1.ManifestIdentifier) (bool, error) {
	hub := r.Manager.GetLocalManager().GetClient()
	goneSet := map[fleetv1alpha1.ManifestIdentifier]bool{}
	for _, id := range gone {
		goneSet[id] = true
	}
	kept := rec.Status.Applied[:0]
	changed := false
	for _, a := range rec.Status.Applied {
		if goneSet[a.ManifestIdentifier] {
			changed = true
			continue
		}
		kept = append(kept, a)
	}
	rec.Status.Applied = kept
	if changed {
		if err := hub.Status().Update(ctx, rec); err != nil {
			return false, fmt.Errorf("failed to update record after deletion: %w", err)
		}
	}
	for _, m := range trackedManifests(rec) {
		if !goneSet[m.ManifestIdentifier] {
			return false, nil
		}
	}
	return true, nil
}

// markOrphaned flags a record whose member was deregistered before cleanup.
func (r *FleetReconciler) markOrphaned(ctx context.Context, rec *fleetv1alpha1.AppliedManifestRecord, orphaned bool, msg string) error {
	hub := r.Manager.GetLocalManager().GetClient()
	cond := metav1.Condition{Type: fleetv1alpha1.RecordConditionOrphaned, Status: metav1.ConditionFalse, Reason: "MemberRegistered", Message: "member ClusterProfile present"}
	if orphaned {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "MemberDeregistered", msg
	}
	if !meta.SetStatusCondition(&rec.Status.Conditions, cond) {
		return nil
	}
	return hub.Status().Update(ctx, rec)
}
