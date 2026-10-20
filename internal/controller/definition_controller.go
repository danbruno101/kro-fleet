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
	"fmt"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// DefaultMemberKroNamespace is where the stock kro Helm chart lands on a
// member; only used to read the kro version off its Deployment.
const DefaultMemberKroNamespace = "kro"

// kroVersionTTL bounds how often a member's kro Deployment is re-read.
const kroVersionTTL = 5 * time.Minute

// DefinitionReconciler distributes one hub FleetResourceGraphDefinition, as
// a plain kro ResourceGraphDefinition, to the registered, healthy members
// its distribution selects; reads each member kro's verdict back through
// the member watch; removes it on unplacement/deletion — but never from a
// member that still hosts hub-placed instances of its kind (the deletion
// guard); and folds the per-member facts into Distributed / Accepted /
// Deleting on the hub object. It is the distribution half of KREP-013; the
// hub never evaluates a graph.
//
// The per-member inventory for definitions is status.members[] itself (name,
// appliedHash, uid), not an AppliedManifestRecord: records belong to
// instances, and the record controller would settle a definition's record
// as an orphan of a FleetGenAIService that never existed.
type DefinitionReconciler struct {
	Manager            mcmanager.Manager
	FleetNamespace     string
	MemberKroNamespace string

	kroVersions sync.Map // member name -> kroVersionEntry
}

type kroVersionEntry struct {
	version string
	at      time.Time
}

// SetupWithManager wires the watches:
//   - FleetResourceGraphDefinition on the hub (local only);
//   - the distributed ResourceGraphDefinition on every engaged member
//     (provider clusters only), mapped back to the same-named hub object —
//     this is how member status reaches the hub, no polling (started lazily
//     once the member serves the kind, see LazyMemberKind);
//   - ClusterProfile on the hub, mapped to every definition, so new or
//     relabeled members receive (or lose) definitions;
//   - AppliedManifestRecord on the hub, mapped to the definitions of the
//     kinds it tracks, so a blocked removal proceeds as soon as the last
//     instance is gone.
func (r *DefinitionReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.Manager = mgr
	if r.MemberKroNamespace == "" {
		r.MemberKroNamespace = DefaultMemberKroNamespace
	}

	memberObj := &unstructured.Unstructured{}
	memberObj.SetGroupVersionKind(ResourceGraphDefinitionGVK)

	c, err := mcbuilder.ControllerManagedBy(mgr).
		Named("fleetresourcegraphdefinition").
		For(&fleetv1alpha1.FleetResourceGraphDefinition{},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{}, r.allDefinitionsHandler,
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(&fleetv1alpha1.AppliedManifestRecord{}, r.recordHandler,
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Build(r)
	if err != nil {
		return err
	}
	// A member without kro (yet) must still engage, so its status can say
	// so; the watch starts once the member serves the kind.
	return c.MultiClusterWatch(LazyMemberKind(memberObj, r.memberDefinitionHandler))
}

// memberDefinitionHandler maps a distributed ResourceGraphDefinition event
// on a member to the same-named hub definition.
func (r *DefinitionReconciler) memberDefinitionHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(_ context.Context, obj client.Object) []mcreconcile.Request {
			placedBy := obj.GetLabels()[PlacedByLabel]
			if placedBy == "" {
				return nil
			}
			return []mcreconcile.Request{{
				Request: reconcile.Request{NamespacedName: types.NamespacedName{Name: placedBy}},
			}}
		})
}

// allDefinitionsHandler re-enqueues every definition on an inventory change.
func (r *DefinitionReconciler) allDefinitionsHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(ctx context.Context, _ client.Object) []mcreconcile.Request {
			return r.enqueueDefinitions(ctx, func(*fleetv1alpha1.FleetResourceGraphDefinition) bool { return true })
		})
}

// recordHandler enqueues the definitions whose kind an inventory record
// tracks: the deletion guard depends on these records.
func (r *DefinitionReconciler) recordHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(ctx context.Context, obj client.Object) []mcreconcile.Request {
			rec, ok := obj.(*fleetv1alpha1.AppliedManifestRecord)
			if !ok {
				return nil
			}
			return r.enqueueDefinitions(ctx, func(d *fleetv1alpha1.FleetResourceGraphDefinition) bool {
				return manifestsOfKind(rec, DefinitionKind(d)) > 0
			})
		})
}

func (r *DefinitionReconciler) enqueueDefinitions(ctx context.Context, match func(*fleetv1alpha1.FleetResourceGraphDefinition) bool) []mcreconcile.Request {
	list := &fleetv1alpha1.FleetResourceGraphDefinitionList{}
	if err := r.Manager.GetLocalManager().GetClient().List(ctx, list); err != nil {
		ctrllog.FromContext(ctx).Error(err, "failed to list FleetResourceGraphDefinitions")
		return nil
	}
	var reqs []mcreconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, mcreconcile.Request{
				Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])},
			})
		}
	}
	return reqs
}

// Reconcile funnels every event (hub definition, member copy, inventory,
// record) into one converge pass for the named definition.
func (r *DefinitionReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("fleetresourcegraphdefinition", req.Name)
	hub := r.Manager.GetLocalManager().GetClient()

	frgd := &fleetv1alpha1.FleetResourceGraphDefinition{}
	if err := hub.Get(ctx, req.NamespacedName, frgd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !frgd.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, frgd)
	}

	if !controllerutil.ContainsFinalizer(frgd, Finalizer) {
		controllerutil.AddFinalizer(frgd, Finalizer)
		if err := hub.Update(ctx, frgd); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	hash, err := DefinitionSpecHash(&frgd.Spec.ResourceGraphDefinition)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired, err := RenderDefinition(frgd, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hub.List(ctx, profiles, client.InNamespace(r.FleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}
	profileMap := map[string]*clusterinventoryv1alpha1.ClusterProfile{}
	for i := range profiles.Items {
		profileMap[profiles.Items[i].Name] = &profiles.Items[i]
	}
	targets := DefinitionTargets(frgd, profiles.Items)
	targetSet := map[string]bool{}
	for _, t := range targets {
		targetSet[t] = true
	}
	previous := map[string]fleetv1alpha1.MemberDefinitionStatus{}
	for _, m := range frgd.Status.Members {
		previous[m.Name] = m
	}

	var members []fleetv1alpha1.MemberDefinitionStatus
	var blocked []BlockedMember
	var removing []string
	retrySoon := false

	// Unplace: members the status still lists that are no longer targeted
	// (disabled, deselected, or deregistered), subject to the guard.
	for _, prev := range frgd.Status.Members {
		if targetSet[prev.Name] {
			continue
		}
		st, b, pending, gone, err := r.removeFromMember(ctx, frgd, prev, profileMap[prev.Name])
		if err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case b != nil:
			blocked = append(blocked, *b)
			members = append(members, st)
		case gone:
			log.Info("definition removed from member", "member", prev.Name)
		default:
			retrySoon = true
			removing = append(removing, prev.Name)
			st.Reason, st.Message = fleetv1alpha1.ReasonNotDistributed, "pending removal: "+pending
			members = append(members, st)
		}
	}

	// Distribute to every targeted member and read its verdict back.
	for _, name := range targets {
		st := r.distributeToMember(ctx, frgd, desired, hash, name, profileMap[name], previous[name])
		if !st.Applied || st.Reason == fleetv1alpha1.ReasonMemberNotEngaged || st.Reason == fleetv1alpha1.ReasonApplyFailed {
			retrySoon = true
		}
		members = append(members, st)
	}

	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	distributed, accepted, deleting, summary := RollupDefinition(DefinitionRollup{
		Enabled: frgd.Spec.Distribution.IsEnabled(), Targets: targets, Members: members, Blocked: blocked, Removing: removing,
	})
	frgd.Status.Members = members
	frgd.Status.Summary = summary
	frgd.Status.SpecHash = hash
	frgd.Status.ObservedGeneration = frgd.Generation
	for _, c := range []metav1.Condition{distributed, accepted, deleting} {
		c.ObservedGeneration = frgd.Generation
		meta.SetStatusCondition(&frgd.Status.Conditions, c)
	}
	if err := hub.Status().Update(ctx, frgd); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if retrySoon {
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// finalize removes the definition from every member it is on, honoring the
// guard, then releases the finalizer. A member blocked by instances keeps
// the wrapper in Terminating with Deleting=False/BlockedByInstances until
// the instances are gone (the record watch re-triggers this).
func (r *DefinitionReconciler) finalize(ctx context.Context, frgd *fleetv1alpha1.FleetResourceGraphDefinition) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	hub := r.Manager.GetLocalManager().GetClient()

	if !controllerutil.ContainsFinalizer(frgd, Finalizer) {
		return ctrl.Result{}, nil
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hub.List(ctx, profiles, client.InNamespace(r.FleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}
	profileMap := map[string]*clusterinventoryv1alpha1.ClusterProfile{}
	for i := range profiles.Items {
		profileMap[profiles.Items[i].Name] = &profiles.Items[i]
	}

	var remaining []fleetv1alpha1.MemberDefinitionStatus
	var blocked []BlockedMember
	var removing []string
	for _, prev := range frgd.Status.Members {
		st, b, pending, gone, err := r.removeFromMember(ctx, frgd, prev, profileMap[prev.Name])
		if err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case b != nil:
			blocked = append(blocked, *b)
			remaining = append(remaining, st)
		case gone:
			log.Info("definition removed from member", "member", prev.Name)
		default:
			removing = append(removing, prev.Name)
			st.Reason, st.Message = fleetv1alpha1.ReasonNotDistributed, "pending removal: "+pending
			remaining = append(remaining, st)
		}
	}

	if len(remaining) > 0 {
		sort.Slice(remaining, func(i, j int) bool { return remaining[i].Name < remaining[j].Name })
		_, _, deleting, summary := RollupDefinition(DefinitionRollup{Enabled: false, Members: remaining, Blocked: blocked, Removing: removing})
		frgd.Status.Members = remaining
		frgd.Status.Summary = summary
		deleting.ObservedGeneration = frgd.Generation
		meta.SetStatusCondition(&frgd.Status.Conditions, deleting)
		if err := hub.Status().Update(ctx, frgd); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update status during finalize: %w", err)
		}
		log.Info("finalization pending", "reason", deleting.Reason, "why", deleting.Message)
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}

	controllerutil.RemoveFinalizer(frgd, Finalizer)
	if err := hub.Update(ctx, frgd); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}
	log.Info("finalized: definition removed from every member")
	return ctrl.Result{}, nil
}

// distributeToMember server-side-applies the definition into one healthy,
// engaged member and reads the member kro's verdict off the apply response
// (the freshest view of the copy). Everything reported is observed.
func (r *DefinitionReconciler) distributeToMember(ctx context.Context, frgd *fleetv1alpha1.FleetResourceGraphDefinition, desired *unstructured.Unstructured, hash, member string, profile *clusterinventoryv1alpha1.ClusterProfile, prev fleetv1alpha1.MemberDefinitionStatus) fleetv1alpha1.MemberDefinitionStatus {
	log := ctrllog.FromContext(ctx)
	st := fleetv1alpha1.MemberDefinitionStatus{Name: member, AppliedHash: prev.AppliedHash, UID: prev.UID, KroVersion: prev.KroVersion, LastTransitionTime: prev.LastTransitionTime}

	st.Applied = prev.AppliedHash == hash // as last observed, until the member answers again
	if !MemberHealthy(profile) {
		st.Reason, st.Message = fleetv1alpha1.ReasonMemberNotEngaged, "member not engaged (cluster manager reports ControlPlaneHealthy is not True)"
		return st
	}
	cl, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(r.FleetNamespace+"/"+member))
	if err != nil {
		st.Reason, st.Message = fleetv1alpha1.ReasonMemberNotEngaged, "member not engaged (unhealthy or missing credentials)"
		return st
	}

	obj := desired.DeepCopy()
	if err := cl.GetClient().Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
		log.Error(err, "failed to apply ResourceGraphDefinition to member", "member", member)
		st.Applied = false
		st.Reason, st.Message = fleetv1alpha1.ReasonApplyFailed, err.Error()
		return st
	}

	o := ObserveDefinition(obj)
	st.UID = o.UID
	st.AppliedHash = o.Hash
	st.Applied = o.Hash == hash
	st.State, st.Ready, st.Reason, st.Message = o.State, o.Ready, o.Reason, o.Message
	if !o.Fresh() {
		// kro has not evaluated this generation yet: its Ready is about an
		// older revision. Say so rather than letting a stale True count.
		st.Ready = string(metav1.ConditionUnknown)
		st.Reason = "Reconciling"
		st.Message = fmt.Sprintf("member kro has evaluated generation %d, copy is at generation %d", o.ReadyObservedGeneration, o.Generation)
	}
	st.KroVersion = r.kroVersion(ctx, cl, member)
	if prev.State != st.State || prev.Ready != st.Ready || prev.LastTransitionTime.IsZero() {
		st.LastTransitionTime = metav1.Now()
	}
	return st
}

// removeFromMember takes the definition off one member unless hub-placed
// instances of its kind still live there. It returns the member's status
// entry to keep (when not gone), the blocker if any, why removal is still
// pending otherwise, and gone=true when the member is settled.
func (r *DefinitionReconciler) removeFromMember(ctx context.Context, frgd *fleetv1alpha1.FleetResourceGraphDefinition, prev fleetv1alpha1.MemberDefinitionStatus, profile *clusterinventoryv1alpha1.ClusterProfile) (fleetv1alpha1.MemberDefinitionStatus, *BlockedMember, string, bool, error) {
	log := ctrllog.FromContext(ctx)
	member := prev.Name
	st := prev
	st.Applied = false

	n, err := r.blockingInstances(ctx, frgd, member)
	if err != nil {
		return st, nil, "", false, err
	}
	if n > 0 {
		// Keep observing the copy while it is held, so the entry stays true.
		if MemberHealthy(profile) {
			if cl, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(r.FleetNamespace+"/"+member)); err == nil {
				copy := &unstructured.Unstructured{}
				copy.SetGroupVersionKind(ResourceGraphDefinitionGVK)
				if err := cl.GetClient().Get(ctx, types.NamespacedName{Name: frgd.Name}, copy); err == nil {
					o := ObserveDefinition(copy)
					st.State, st.Ready, st.Reason, st.Message = o.State, o.Ready, o.Reason, o.Message
					st.AppliedHash, st.UID = o.Hash, o.UID
					st.Applied = o.Hash == frgd.Status.SpecHash
				}
			}
		}
		return st, &BlockedMember{Name: member, Instances: n}, "", false, nil
	}

	if profile == nil {
		// Deregistered before removal: nothing can be done from here. There
		// is no orphan ledger for definitions (a copy left on a member that
		// never returns is harmless on its own); surfaced in the log only.
		log.Info("member deregistered before the definition was removed; its copy may remain there", "member", member)
		return st, nil, "", true, nil
	}
	if !MemberHealthy(profile) {
		return st, nil, "member reported unhealthy by its cluster manager", false, nil
	}
	cl, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(r.FleetNamespace+"/"+member))
	if err != nil {
		return st, nil, "member not engaged (credentials or connectivity)", false, nil
	}
	tracked := []fleetv1alpha1.AppliedManifest{{
		ManifestIdentifier: fleetv1alpha1.ManifestIdentifier{Group: ResourceGraphDefinitionGVK.Group, Version: ResourceGraphDefinitionGVK.Version, Kind: ResourceGraphDefinitionGVK.Kind, Name: frgd.Name},
		UID:                prev.UID,
	}}
	gone, err := deleteManifests(ctx, cl.GetClient(), frgd.Name, tracked)
	if err != nil {
		return st, nil, "", false, err
	}
	if len(gone) == 0 {
		return st, nil, "the copy is still terminating on the member (kro unwinding the definition)", false, nil
	}
	return st, nil, "", true, nil
}

// blockingInstances counts the hub-placed instances of this definition's
// kind on one member: the AppliedManifestRecords for that member whose
// tracked manifests include the kind. Removing the definition there would
// make the member's kro drop the CRD and, with it, every instance.
func (r *DefinitionReconciler) blockingInstances(ctx context.Context, frgd *fleetv1alpha1.FleetResourceGraphDefinition, member string) (int, error) {
	gvk := DefinitionKind(frgd)
	if gvk.Kind == "" {
		return 0, nil
	}
	list := &fleetv1alpha1.AppliedManifestRecordList{}
	if err := r.Manager.GetLocalManager().GetClient().List(ctx, list, client.MatchingLabels{fleetv1alpha1.ClusterLabel: member}); err != nil {
		return 0, fmt.Errorf("failed to list AppliedManifestRecords for %s: %w", member, err)
	}
	n := 0
	for i := range list.Items {
		if manifestsOfKind(&list.Items[i], gvk) > 0 {
			n++
		}
	}
	return n, nil
}

// kroVersion reads the kro version a member runs off its kro Deployment
// (the chart's app.kubernetes.io/version label), uncached and at most once
// per TTL per member. Best effort: empty when not obtainable.
func (r *DefinitionReconciler) kroVersion(ctx context.Context, cl cluster.Cluster, member string) string {
	if v, ok := r.kroVersions.Load(member); ok {
		if e := v.(kroVersionEntry); time.Since(e.at) < kroVersionTTL {
			return e.version
		}
	}
	version := ""
	list := &appsv1.DeploymentList{}
	if err := cl.GetAPIReader().List(ctx, list, client.InNamespace(r.MemberKroNamespace), client.MatchingLabels{"app.kubernetes.io/name": "kro"}); err == nil {
		for _, d := range list.Items {
			if v := d.Labels["app.kubernetes.io/version"]; v != "" {
				version = v
				break
			}
		}
	}
	r.kroVersions.Store(member, kroVersionEntry{version: version, at: time.Now()})
	return version
}
