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
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

const (
	// Finalizer drives cross-cluster GC: the hub object is only released
	// once every tracked member copy is deleted (or its member is gone).
	Finalizer = "fleet.kro.run/cleanup"
	// FieldOwner is the server-side-apply field manager on members.
	FieldOwner = "kro-fleet-placement"
	// PlacedByLabel marks objects this controller applied to members; the
	// inventory only ever deletes objects carrying it.
	PlacedByLabel = "fleet.kro.run/placed-by"

	// requeueInterval is the periodic resync: cheap belt-and-braces for
	// member reachability transitions the watches cannot deliver.
	requeueInterval = 30 * time.Second
	// notEngagedRetry is used while a tracked member is temporarily
	// unreachable (registered but not engaged).
	notEngagedRetry = 15 * time.Second
)

// GenAIServiceGVK is the member-side object the fleet places. Its schema is
// owned by stock kro on the members (the sister repo's GenAIService RGD).
var GenAIServiceGVK = schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "GenAIService"}

// FleetReconciler places one hub FleetGenAIService onto every member matching
// its placement, keeps a per-(instance, member) AppliedManifestRecord as the
// applied-manifest inventory, GCs on unplacement/deletion via the finalizer
// using exactly that inventory, and aggregates per-member readiness back onto
// the hub object.
type FleetReconciler struct {
	Manager        mcmanager.Manager
	FleetNamespace string
}

// SetupWithManager wires the watches:
//   - FleetGenAIService on the hub (local cluster only — members never carry
//     this CRD);
//   - the placed GenAIService on every engaged member (provider clusters
//     only), mapped back to the same-named hub object;
//   - ClusterProfile on the hub, mapped to all FleetGenAIServices so
//     inventory changes re-resolve placement.
func (r *FleetReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.Manager = mgr

	memberObj := &unstructured.Unstructured{}
	memberObj.SetGroupVersionKind(GenAIServiceGVK)

	return mcbuilder.ControllerManagedBy(mgr).
		Named("fleetgenaiservice").
		For(&fleetv1alpha1.FleetGenAIService{},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(memberObj, r.memberObjectHandler,
			mcbuilder.WithEngageWithLocalCluster(false),
			mcbuilder.WithEngageWithProviderClusters(true)).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{}, r.clusterProfileHandler,
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Complete(r)
}

// memberObjectHandler maps a placed GenAIService event on a member to the
// same-named FleetGenAIService on the hub (ClusterName "" = local cluster).
func (r *FleetReconciler) memberObjectHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(_ context.Context, obj client.Object) []mcreconcile.Request {
			if obj.GetLabels()[PlacedByLabel] == "" {
				return nil
			}
			return []mcreconcile.Request{{
				Request: reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      obj.GetName(),
				}},
			}}
		})
}

// clusterProfileHandler re-enqueues every FleetGenAIService when the
// ClusterProfile inventory changes (add/remove/relabel members).
func (r *FleetReconciler) clusterProfileHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(ctx context.Context, _ client.Object) []mcreconcile.Request {
			list := &fleetv1alpha1.FleetGenAIServiceList{}
			if err := r.Manager.GetLocalManager().GetClient().List(ctx, list); err != nil {
				ctrllog.FromContext(ctx).Error(err, "failed to list FleetGenAIServices for ClusterProfile event")
				return nil
			}
			reqs := make([]mcreconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, mcreconcile.Request{
					Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])},
				})
			}
			return reqs
		})
}

// Reconcile funnels every event (hub object, member copy, inventory) into one
// converge pass for the named FleetGenAIService.
func (r *FleetReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("fleetgenaiservice", req.NamespacedName)
	hub := r.Manager.GetLocalManager().GetClient()

	fgs := &fleetv1alpha1.FleetGenAIService{}
	if err := hub.Get(ctx, req.NamespacedName, fgs); err != nil {
		// Not found: deletion already completed via the finalizer path.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !fgs.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, fgs)
	}

	if !controllerutil.ContainsFinalizer(fgs, Finalizer) {
		controllerutil.AddFinalizer(fgs, Finalizer)
		if err := hub.Update(ctx, fgs); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hub.List(ctx, profiles, client.InNamespace(r.FleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}
	matched, err := ResolvePlacement(fgs.Spec.Placement.ClusterSelector, profiles.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	matchedSet := toSet(matched)
	profileSet := map[string]bool{}
	for i := range profiles.Items {
		profileSet[profiles.Items[i].Name] = true
	}

	records, err := r.listRecords(ctx, fgs)
	if err != nil {
		return ctrl.Result{}, err
	}

	blocked := false // a tracked member is unreachable, retry sooner
	var statuses []fleetv1alpha1.ClusterStatus

	// Unplace members that stopped matching: delete exactly what their
	// record tracks, then the record.
	for member, rec := range records {
		if matchedSet[member] {
			continue
		}
		gone, err := r.unplace(ctx, fgs, rec, profileSet[member])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			blocked = true
			statuses = append(statuses, fleetv1alpha1.ClusterStatus{
				Name: member, Ready: false, Message: "pending removal: member not reachable",
			})
		} else {
			log.Info("unplaced from member", "member", member)
		}
	}

	// Place / converge on matching members and collect readiness.
	desired, err := r.renderMemberObject(fgs)
	if err != nil {
		return ctrl.Result{}, err
	}
	intended := []fleetv1alpha1.ManifestIdentifier{identifierOf(desired)}
	readyCount := 0
	for _, member := range matched {
		// Intent log first: the record names what is about to be applied
		// BEFORE the member is touched, so a crash between apply and the
		// confirmation can never orphan a copy the inventory doesn't know.
		rec, err := r.ensureRecord(ctx, fgs, records[member], member, intended)
		if err != nil {
			return ctrl.Result{}, err
		}
		st, err := r.applyToMember(ctx, fgs, rec, member, desired.DeepCopy())
		if err != nil {
			return ctrl.Result{}, err
		}
		if st.Ready {
			readyCount++
		}
		statuses = append(statuses, st)
	}

	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	fgs.Status.Clusters = statuses
	fgs.Status.Summary = fleetv1alpha1.Summary{Placed: int32(len(matched)), Ready: int32(readyCount)}
	fgs.Status.ObservedGeneration = fgs.Generation
	cond := RollupReady(len(matched), readyCount, fgs.Spec.Placement.Tolerance)
	cond.ObservedGeneration = fgs.Generation
	meta.SetStatusCondition(&fgs.Status.Conditions, cond)
	if err := hub.Status().Update(ctx, fgs); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if blocked {
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// finalize unplaces every recorded member, then releases the finalizer.
// Members still registered but not engaged block deletion and retry; a
// member whose ClusterProfile is gone cannot be cleaned, so its record is
// kept and marked Orphaned (settled by RecordReconciler if the member
// returns) while the instance is released.
func (r *FleetReconciler) finalize(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	hub := r.Manager.GetLocalManager().GetClient()

	if !controllerutil.ContainsFinalizer(fgs, Finalizer) {
		return ctrl.Result{}, nil
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hub.List(ctx, profiles, client.InNamespace(r.FleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}
	profileSet := map[string]bool{}
	for i := range profiles.Items {
		profileSet[profiles.Items[i].Name] = true
	}

	records, err := r.listRecords(ctx, fgs)
	if err != nil {
		return ctrl.Result{}, err
	}

	var remaining []fleetv1alpha1.ClusterStatus
	for member, rec := range records {
		gone, err := r.unplace(ctx, fgs, rec, profileSet[member])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			remaining = append(remaining, fleetv1alpha1.ClusterStatus{
				Name: member, Ready: false, Message: "pending removal: member not reachable",
			})
		}
	}

	if len(remaining) > 0 {
		sort.Slice(remaining, func(i, j int) bool { return remaining[i].Name < remaining[j].Name })
		fgs.Status.Clusters = remaining
		if err := hub.Status().Update(ctx, fgs); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update status during finalize: %w", err)
		}
		log.Info("finalization blocked on unreachable members", "count", len(remaining))
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}

	controllerutil.RemoveFinalizer(fgs, Finalizer)
	if err := hub.Update(ctx, fgs); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	log.Info("finalized: all member copies removed")
	return ctrl.Result{}, nil
}

// applyToMember SSAs the namespace + the desired object into one member,
// confirms it in the record, prunes anything the record tracks that is no
// longer intended, and returns the member's status entry.
func (r *FleetReconciler) applyToMember(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService, rec *fleetv1alpha1.AppliedManifestRecord, member string, desired *unstructured.Unstructured) (fleetv1alpha1.ClusterStatus, error) {
	log := ctrllog.FromContext(ctx)
	cl, err := r.Manager.GetCluster(ctx, r.clusterName(member))
	if err != nil {
		return fleetv1alpha1.ClusterStatus{Name: member, Ready: false, Message: "member not engaged (unhealthy or missing credentials)"}, nil
	}

	// The workload namespace is shared infrastructure on the member (other
	// instances and the member's own ClusterPlatform live there), so it is
	// ensured but deliberately not inventoried or deleted.
	ns := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: fgs.Namespace},
	}
	if err := cl.GetClient().Patch(ctx, ns, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
		return fleetv1alpha1.ClusterStatus{Name: member, Ready: false, Message: fmt.Sprintf("failed to ensure namespace: %v", err)}, nil
	}

	if err := cl.GetClient().Patch(ctx, desired, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
		log.Error(err, "failed to apply GenAIService to member", "member", member)
		return fleetv1alpha1.ClusterStatus{Name: member, Ready: false, Message: fmt.Sprintf("apply failed: %v", err)}, nil
	}
	applied := fleetv1alpha1.AppliedManifest{ManifestIdentifier: identifierOf(desired), UID: desired.GetUID()}
	if err := r.confirmApplied(ctx, rec, []fleetv1alpha1.AppliedManifest{applied}); err != nil {
		return fleetv1alpha1.ClusterStatus{}, err
	}

	// Prune: anything confirmed earlier that is no longer intended.
	var stale []fleetv1alpha1.AppliedManifest
	intended := map[fleetv1alpha1.ManifestIdentifier]bool{}
	for _, id := range rec.Spec.Manifests {
		intended[id] = true
	}
	for _, a := range rec.Status.Applied {
		if !intended[a.ManifestIdentifier] {
			stale = append(stale, a)
		}
	}
	if len(stale) > 0 {
		gone, err := deleteManifests(ctx, cl.GetClient(), fgs.Name, stale)
		if err != nil {
			return fleetv1alpha1.ClusterStatus{}, err
		}
		if _, err := r.dropFromRecord(ctx, rec, gone); err != nil {
			return fleetv1alpha1.ClusterStatus{}, err
		}
		log.Info("pruned stale objects from member", "member", member, "count", len(gone))
	}

	// Read back (cached) for readiness.
	placed := &unstructured.Unstructured{}
	placed.SetGroupVersionKind(GenAIServiceGVK)
	if err := cl.GetClient().Get(ctx, client.ObjectKeyFromObject(desired), placed); err != nil {
		return fleetv1alpha1.ClusterStatus{Name: member, Ready: false, Message: fmt.Sprintf("placed, readback failed: %v", err)}, nil
	}
	ready, msg := MemberReady(placed)
	return fleetv1alpha1.ClusterStatus{Name: member, Ready: ready, Message: msg}, nil
}

// unplace removes everything a record tracks from its member and then the
// record itself. Returns true when the placement is settled: all objects
// confirmed gone and the record deleted, or the member deregistered (the
// record is then kept, marked Orphaned, as the ledger of what may remain).
func (r *FleetReconciler) unplace(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService, rec *fleetv1alpha1.AppliedManifestRecord, profileExists bool) (bool, error) {
	log := ctrllog.FromContext(ctx)
	hub := r.Manager.GetLocalManager().GetClient()
	member := rec.Spec.Cluster.Name

	cl, err := r.Manager.GetCluster(ctx, r.clusterName(member))
	if err != nil {
		if !profileExists {
			// The member left the inventory; nothing can be done from here.
			// Keep the record as the orphan ledger and release the instance.
			msg := fmt.Sprintf("member %s was deregistered before cleanup; %d tracked object(s) may be orphaned there", member, len(trackedManifests(rec)))
			log.Info("member deregistered before cleanup; record kept as orphan ledger", "member", member)
			return true, r.markOrphaned(ctx, rec, true, msg)
		}
		return false, nil // registered but not engaged: retry later
	}
	if err := r.markOrphaned(ctx, rec, false, ""); err != nil {
		return false, err
	}

	gone, err := deleteManifests(ctx, cl.GetClient(), fgs.Name, trackedManifests(rec))
	if err != nil {
		return false, err
	}
	done, err := r.dropFromRecord(ctx, rec, gone)
	if err != nil || !done {
		return false, err
	}
	if err := hub.Delete(ctx, rec); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("failed to delete record for %s: %w", member, err)
	}
	return true, nil
}

// renderMemberObject builds the GenAIService to place from the template.
func (r *FleetReconciler) renderMemberObject(fgs *fleetv1alpha1.FleetGenAIService) (*unstructured.Unstructured, error) {
	spec := map[string]interface{}{}
	if len(fgs.Spec.Template.Spec.Raw) > 0 {
		if err := json.Unmarshal(fgs.Spec.Template.Spec.Raw, &spec); err != nil {
			return nil, fmt.Errorf("template.spec is not an object: %w", err)
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GenAIServiceGVK)
	obj.SetNamespace(fgs.Namespace)
	obj.SetName(fgs.Name)
	obj.SetLabels(map[string]string{PlacedByLabel: fgs.Name})
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		return nil, err
	}
	return obj, nil
}

func (r *FleetReconciler) clusterName(profileName string) multicluster.ClusterName {
	return multicluster.ClusterName(r.FleetNamespace + "/" + profileName)
}

func toSet(names []string) map[string]bool {
	s := map[string]bool{}
	for _, n := range names {
		s[n] = true
	}
	return s
}
