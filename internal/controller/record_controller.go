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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
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

// RecordReconciler settles orphaned AppliedManifestRecords: records whose
// FleetGenAIService is gone but whose member could not be cleaned at the
// time (deregistered, or unreachable). Once the member is engaged again it
// deletes exactly the recorded objects and then the record. Records of live
// instances are driven by FleetReconciler and ignored here.
type RecordReconciler struct {
	Manager        mcmanager.Manager
	FleetNamespace string
	inventory      *FleetReconciler // shares the record helpers
}

// SetupWithManager watches records on the hub, and ClusterProfiles so a
// member registering again re-triggers its orphaned records.
func (r *RecordReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.Manager = mgr
	r.inventory = &FleetReconciler{Manager: mgr, FleetNamespace: r.FleetNamespace}

	return mcbuilder.ControllerManagedBy(mgr).
		Named("appliedmanifestrecord").
		For(&fleetv1alpha1.AppliedManifestRecord{},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{}, r.clusterProfileHandler,
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Complete(r)
}

// clusterProfileHandler enqueues every record tracking the member whose
// ClusterProfile changed.
func (r *RecordReconciler) clusterProfileHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(ctx context.Context, obj client.Object) []mcreconcile.Request {
			list := &fleetv1alpha1.AppliedManifestRecordList{}
			if err := r.Manager.GetLocalManager().GetClient().List(ctx, list, client.MatchingLabels{fleetv1alpha1.ClusterLabel: obj.GetName()}); err != nil {
				ctrllog.FromContext(ctx).Error(err, "failed to list AppliedManifestRecords for ClusterProfile event")
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

// Reconcile handles one record.
func (r *RecordReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("record", req.NamespacedName)
	hub := r.Manager.GetLocalManager().GetClient()

	rec := &fleetv1alpha1.AppliedManifestRecord{}
	if err := hub.Get(ctx, req.NamespacedName, rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Live instance (present and not deleting): FleetReconciler owns the record.
	fgs := &fleetv1alpha1.FleetGenAIService{}
	err := hub.Get(ctx, types.NamespacedName{Namespace: rec.Namespace, Name: rec.Spec.InstanceRef.Name}, fgs)
	switch {
	case err == nil:
		return ctrl.Result{}, nil // deletion is driven by the instance's finalizer
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("failed to get FleetGenAIService: %w", err)
	}

	// Orphan: the instance is gone. Clean the member when it is reachable.
	member := rec.Spec.Cluster.Name
	cl, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(r.FleetNamespace+"/"+member))
	if err != nil {
		profile := &clusterinventoryv1alpha1.ClusterProfile{}
		perr := hub.Get(ctx, types.NamespacedName{Namespace: r.FleetNamespace, Name: member}, profile)
		if apierrors.IsNotFound(perr) {
			msg := fmt.Sprintf("member %s was deregistered before cleanup; %d tracked object(s) may be orphaned there", member, len(trackedManifests(rec)))
			log.Info("orphaned record: member deregistered", "member", member)
			// Re-enqueued by the ClusterProfile watch when the member returns.
			return ctrl.Result{}, r.inventory.markOrphaned(ctx, rec, true, msg)
		}
		log.Info("orphaned record: member registered but not engaged, retrying", "member", member)
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}
	if err := r.inventory.markOrphaned(ctx, rec, false, ""); err != nil {
		return ctrl.Result{}, err
	}

	gone, err := deleteManifests(ctx, cl.GetClient(), rec.Spec.InstanceRef.Name, trackedManifests(rec))
	if err != nil {
		return ctrl.Result{}, err
	}
	done, err := r.inventory.dropFromRecord(ctx, rec, gone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: notEngagedRetry}, nil
	}
	if err := hub.Delete(ctx, rec); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete settled record: %w", err)
	}
	log.Info("orphaned record settled: member cleaned", "member", member)
	return ctrl.Result{}, nil
}
