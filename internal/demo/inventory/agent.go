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

// Package inventory is the DEMO-ONLY stand-in for a cluster manager's
// property duties (KEP-4322 "Properties"): it mirrors each member's About
// API ClusterProperty objects into that member's ClusterProfile
// status.properties on the hub, name and value as-is, and it publishes the
// one computed property the capacity story needs — free accelerators, from
// node allocatable minus pod requests — as a ClusterProperty on the member
// (so the member advertises it like any other) before mirroring it.
//
// It does not own ControlPlaneHealthy (the scripts assert it; the demo flips
// it to simulate an outage). Everything here is replaceable by a real
// cluster manager (OCM, KubeFleet, GKE Fleet) that populates
// status.properties; kro-fleet and the demo scheduler only read the hub.
package inventory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	"github.com/danbruno101/kro-fleet/internal/demo/properties"
)

// ClusterPropertyGVK is the About API kind (storage version v1beta1).
var ClusterPropertyGVK = schema.GroupVersionKind{Group: "about.k8s.io", Version: "v1beta1", Kind: "ClusterProperty"}

const (
	// FieldOwner is the SSA field manager for computed ClusterProperties.
	FieldOwner = "fleet-demo-inventory-agent"
	// ManagedLabel marks ClusterProperties this agent computes (and owns).
	ManagedLabel = "fleet.kro.run/computed-by"

	resync = 20 * time.Second
)

// Agent mirrors member properties to the hub, per engaged member.
type Agent struct {
	Manager        mcmanager.Manager
	FleetNamespace string
	// AcceleratorResources are the extended resource names counted as
	// accelerators (the first one present on a node wins per cluster).
	AcceleratorResources []string
}

// SetupWithManager watches ClusterProperty and Node objects on every engaged
// member; each event re-syncs that member's whole property set.
func (a *Agent) SetupWithManager(mgr mcmanager.Manager) error {
	a.Manager = mgr
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(ClusterPropertyGVK)
	return mcbuilder.ControllerManagedBy(mgr).
		Named("inventory-agent").
		For(cp,
			mcbuilder.WithEngageWithLocalCluster(false),
			mcbuilder.WithEngageWithProviderClusters(true)).
		Watches(&corev1.Node{}, a.wholeClusterHandler,
			mcbuilder.WithEngageWithLocalCluster(false),
			mcbuilder.WithEngageWithProviderClusters(true)).
		Complete(a)
}

// wholeClusterHandler maps any member event to one request for that member.
func (a *Agent) wholeClusterHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(_ context.Context, _ client.Object) []mcreconcile.Request {
			return []mcreconcile.Request{{Request: reconcile.Request{NamespacedName: types.NamespacedName{Name: "inventory"}}}}
		})
}

// Reconcile re-syncs one member: compute accelerator properties, publish
// them on the member, mirror everything into its ClusterProfile.
func (a *Agent) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("member", req.ClusterName)
	if req.ClusterName == "" {
		return ctrl.Result{}, nil
	}
	cl, err := a.Manager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{RequeueAfter: resync}, nil // not engaged (yet)
	}
	memberName := string(req.ClusterName)
	if i := strings.LastIndex(memberName, "/"); i >= 0 {
		memberName = memberName[i+1:]
	}

	total, free, err := a.accelerators(ctx, cl.GetClient())
	if err != nil {
		return ctrl.Result{}, err
	}
	computed := map[string]string{
		properties.GPUsTotal: strconv.FormatInt(total, 10),
		properties.GPUsFree:  strconv.FormatInt(free, 10),
	}
	for name, value := range computed {
		if err := a.publish(ctx, cl.GetClient(), name, value); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Mirror: every ClusterProperty on the member, as-is (KEP-4322).
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: ClusterPropertyGVK.Group, Version: ClusterPropertyGVK.Version, Kind: "ClusterPropertyList"})
	if err := cl.GetClient().List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			log.Info("member has no ClusterProperty CRD; nothing to mirror")
			return ctrl.Result{RequeueAfter: resync}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProperties: %w", err)
	}
	observed := map[string]string{}
	for i := range list.Items {
		v, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "value")
		observed[list.Items[i].GetName()] = v
	}
	for name, value := range computed { // in case the member write has not landed in the cache yet
		observed[name] = value
	}

	hub := a.Manager.GetLocalManager().GetClient()
	profile := &clusterinventoryv1alpha1.ClusterProfile{}
	if err := hub.Get(ctx, types.NamespacedName{Namespace: a.FleetNamespace, Name: memberName}, profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if changed := mergeProperties(profile, observed); changed {
		if err := hub.Status().Update(ctx, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update ClusterProfile properties: %w", err)
		}
		log.Info("mirrored member properties", "count", len(observed), "gpusFree", free, "gpusTotal", total)
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// accelerators sums allocatable accelerators over nodes and subtracts the
// requests of non-terminal pods, for the first accelerator resource any
// node advertises.
func (a *Agent) accelerators(ctx context.Context, cl client.Client) (total, free int64, err error) {
	nodes := &corev1.NodeList{}
	if err := cl.List(ctx, nodes); err != nil {
		return 0, 0, fmt.Errorf("failed to list nodes: %w", err)
	}
	resource := ""
	for _, name := range a.AcceleratorResources {
		for i := range nodes.Items {
			if q, ok := nodes.Items[i].Status.Allocatable[corev1.ResourceName(name)]; ok && !q.IsZero() {
				resource = name
				break
			}
		}
		if resource != "" {
			break
		}
	}
	if resource == "" {
		return 0, 0, nil
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceName(resource)]; ok {
			total += q.Value()
		}
	}
	pods := &corev1.PodList{}
	if err := cl.List(ctx, pods); err != nil {
		return 0, 0, fmt.Errorf("failed to list pods: %w", err)
	}
	var used int64
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceName(resource)]; ok {
				used += q.Value()
			} else if q, ok := c.Resources.Limits[corev1.ResourceName(resource)]; ok {
				used += q.Value()
			}
		}
	}
	free = total - used
	if free < 0 {
		free = 0
	}
	return total, free, nil
}

// publish server-side-applies a computed ClusterProperty on the member.
func (a *Agent) publish(ctx context.Context, cl client.Client, name, value string) error {
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(ClusterPropertyGVK)
	cp.SetName(name)
	cp.SetLabels(map[string]string{ManagedLabel: FieldOwner})
	if err := unstructured.SetNestedField(cp.Object, value, "spec", "value"); err != nil {
		return err
	}
	if err := cl.Patch(ctx, cp, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil // member without the About API CRD: skip publishing
		}
		return fmt.Errorf("failed to publish ClusterProperty %s: %w", name, err)
	}
	return nil
}

// mergeProperties sets status.properties to the observed set, keeping each
// unchanged entry's lastObservedTime. Returns true when anything changed.
func mergeProperties(profile *clusterinventoryv1alpha1.ClusterProfile, observed map[string]string) bool {
	existing := map[string]clusterinventoryv1alpha1.Property{}
	for _, p := range profile.Status.Properties {
		existing[p.Name] = p
	}
	names := make([]string, 0, len(observed))
	for n := range observed {
		names = append(names, n)
	}
	sort.Strings(names)
	now := metav1.Now()
	var out []clusterinventoryv1alpha1.Property
	changed := len(existing) != len(observed)
	for _, n := range names {
		p, ok := existing[n]
		if ok && p.Value == observed[n] {
			out = append(out, p)
			continue
		}
		changed = true
		out = append(out, clusterinventoryv1alpha1.Property{Name: n, Value: observed[n], LastObservedTime: now})
	}
	if !changed {
		return false
	}
	profile.Status.Properties = out
	return true
}
