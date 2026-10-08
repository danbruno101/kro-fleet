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

// Package scheduler is the DEMO-ONLY reference placement producer: it exists
// so the demo has something writing PlacementDecisions. It reads
// ClusterProfile.status.properties on the hub and writes one KEP-5313
// PlacementDecision per FleetGenAIService that asks for one, carrying the
// per-member split as the parameters annotation. It is deliberately small
// and swappable — OCM, Karmada or Kueue would replace it without any change
// to kro-fleet. Nothing in this package is part of kro-fleet proper.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/danbruno101/kro-fleet/internal/controller"
	"github.com/danbruno101/kro-fleet/internal/demo/properties"
)

const (
	// SchedulerName is written to PlacementDecision.schedulerName.
	SchedulerName = "fleet-demo-scheduler"
	// PolicyAnnotation on a FleetGenAIService selects the policy (default
	// spread); "manual" opts the instance out of this scheduler entirely.
	PolicyAnnotation = "scheduler.example.com/policy"
	// FieldOwner is the SSA field manager on decisions.
	FieldOwner = "fleet-demo-scheduler"
)

// Scheduler writes decisions for instances that consume one.
type Scheduler struct {
	Manager        mcmanager.Manager
	FleetNamespace string
	// CapacityProperty is the ClusterProfile property holding free
	// accelerators (default gpus-free.example.com). One replica = one unit.
	CapacityProperty string
	// TotalProperty is the ClusterProfile property holding the cluster's
	// total accelerators (default gpus-total.example.com); see accountCapacity.
	TotalProperty string
}

// SetupWithManager watches instances, the inventory (properties, health)
// and our own decisions, all on the hub.
func (s *Scheduler) SetupWithManager(mgr mcmanager.Manager) error {
	s.Manager = mgr
	if s.CapacityProperty == "" {
		s.CapacityProperty = properties.GPUsFree
	}
	if s.TotalProperty == "" {
		s.TotalProperty = properties.GPUsTotal
	}
	local := []mcbuilder.ForOption{mcbuilder.WithEngageWithLocalCluster(true), mcbuilder.WithEngageWithProviderClusters(false)}
	localW := []mcbuilder.WatchesOption{mcbuilder.WithEngageWithLocalCluster(true), mcbuilder.WithEngageWithProviderClusters(false)}
	return mcbuilder.ControllerManagedBy(mgr).
		Named("demo-scheduler").
		For(&fleetv1alpha1.FleetGenAIService{}, local...).
		Watches(&clusterinventoryv1alpha1.ClusterProfile{}, s.allInstancesHandler, localW...).
		// Any decision of ours changes every instance's accounting (readLedger),
		// so a decision event re-evaluates all of them, not just its own.
		Watches(&clusterinventoryv1alpha1.PlacementDecision{}, s.allInstancesHandler, localW...).
		Complete(s)
}

func (s *Scheduler) allInstancesHandler(_ multicluster.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
	return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation[client.Object, mcreconcile.Request](
		func(ctx context.Context, _ client.Object) []mcreconcile.Request {
			list := &fleetv1alpha1.FleetGenAIServiceList{}
			if err := s.Manager.GetLocalManager().GetClient().List(ctx, list); err != nil {
				return nil
			}
			reqs := make([]mcreconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, mcreconcile.Request{Request: reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}})
			}
			return reqs
		})
}

// Reconcile computes (or recomputes) the decision for one instance.
func (s *Scheduler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("instance", req.NamespacedName)
	hub := s.Manager.GetLocalManager().GetClient()

	fgs := &fleetv1alpha1.FleetGenAIService{}
	if err := hub.Get(ctx, req.NamespacedName, fgs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err) // its decision goes with it (ownerReference)
	}
	if !fgs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	pl := fgs.Spec.Placement
	if pl.DecisionRef == nil && pl.PlacementKey == "" {
		return ctrl.Result{}, nil // not ours: an inline selector needs no scheduler
	}
	decisionName := fgs.Name
	if pl.DecisionRef != nil {
		decisionName = pl.DecisionRef.Name
	}
	placementKey := pl.PlacementKey
	if placementKey == "" {
		placementKey = fgs.Name
	}
	policy := fgs.Annotations[PolicyAnnotation]
	if policy == "" {
		policy = PolicySpread
	}
	if policy == PolicyManual {
		return ctrl.Result{}, nil // another producer owns this instance's decision
	}
	requested := requestedReplicas(fgs)

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hub.List(ctx, profiles, client.InNamespace(s.FleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}

	// What this scheduler has already assigned, per cluster: to this instance
	// and to every other one. Capacity is sized from that ledger, not from
	// the advertised free count alone (see accountCapacity).
	led, err := s.readLedger(ctx, hub, types.NamespacedName{Namespace: fgs.Namespace, Name: decisionName})
	if err != nil {
		return ctrl.Result{}, err
	}

	cands, excluded := s.candidates(profiles.Items, pl.Requirements, led)
	assignment, err := Assign(policy, requested, cands)
	if err != nil {
		log.Error(err, "invalid policy; writing an empty decision")
		assignment = Assignment{}
	}

	pd := s.render(fgs, decisionName, placementKey, policy, requested, cands, assignment, excluded, err)
	if err := hub.Patch(ctx, pd, client.Apply, client.ForceOwnership, client.FieldOwner(FieldOwner)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to write PlacementDecision: %w", err)
	}
	log.Info("decided", "policy", policy, "requested", requested, "assigned", assignment.Total(), "split", assignment)
	return ctrl.Result{}, nil
}

// ledger is this scheduler's own accelerator accounting per cluster: the
// replicas its decisions assign there, split between the instance being
// decided (own) and all the others (others).
type ledger struct {
	own    map[string]int64
	others map[string]int64
}

// readLedger reads this scheduler's decisions back from the hub (all
// namespaces: capacity is per cluster, not per namespace) and sums what each
// assigns to each cluster. Decisions by other producers reserve nothing
// here: their consumption shows up through the advertised free count.
func (s *Scheduler) readLedger(ctx context.Context, hub client.Client, mine types.NamespacedName) (ledger, error) {
	l := ledger{own: map[string]int64{}, others: map[string]int64{}}
	list := &clusterinventoryv1alpha1.PlacementDecisionList{}
	if err := hub.List(ctx, list); err != nil {
		return l, fmt.Errorf("failed to list PlacementDecisions: %w", err)
	}
	for i := range list.Items {
		pd := &list.Items[i]
		if pd.SchedulerName != SchedulerName || !pd.DeletionTimestamp.IsZero() {
			continue
		}
		into := l.others
		if pd.Namespace == mine.Namespace && pd.Name == mine.Name {
			into = l.own
		}
		for _, d := range pd.Decisions {
			into[d.ClusterProfileRef.Name] += assignedReplicas(pd, d.ClusterProfileRef.Name)
		}
	}
	return l, nil
}

// assignedReplicas reads the replicas a decision assigns to one cluster from
// its parameters annotation (0 when the annotation is absent or malformed).
func assignedReplicas(pd *clusterinventoryv1alpha1.PlacementDecision, cluster string) int64 {
	raw, ok := pd.Annotations[fleetv1alpha1.ParametersAnnotationPrefix+cluster]
	if !ok {
		return 0
	}
	var p map[string]interface{}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return 0
	}
	return toInt64(p[fleetv1alpha1.ReplicasParameter])
}

// accountCapacity sizes one cluster for the instance being decided.
//
// The scheduler accounts the way a quota system does: a replica is reserved
// from the moment a decision assigns it, whether or not its pod has landed
// yet, and the advertised free count only reveals consumption this scheduler
// did not place (other tenants). The obvious formula, advertised free plus
// this instance's own assignment, ratchets: until the pods of a new decision
// are scheduled, free still reflects the old consumption, so every
// re-evaluation finds more room than exists and cheapest-first piles every
// replica onto the cheapest cluster.
//
//	external = max(0, total - free - (own + others))   consumption nobody here placed
//	capacity = total - others - external
//
// A cluster that advertises no total leaves only the ratchet-prone formula.
func accountCapacity(total, free int64, hasTotal bool, own, others int64) int64 {
	if !hasTotal {
		return free + own
	}
	external := total - free - (own + others)
	if external < 0 {
		external = 0
	}
	if c := total - others - external; c > 0 {
		return c
	}
	return 0
}

// candidates filters the inventory to clusters this instance may use and
// sizes them. Excluded clusters are explained, for the decision reason.
func (s *Scheduler) candidates(profiles []clusterinventoryv1alpha1.ClusterProfile, req *fleetv1alpha1.Requirements, led ledger) ([]Candidate, []string) {
	var cands []Candidate
	var excluded []string
	for i := range profiles {
		p := &profiles[i]
		props := map[string]string{}
		for _, pr := range p.Status.Properties {
			props[pr.Name] = pr.Value
		}
		switch {
		case !controller.MemberHealthy(p):
			excluded = append(excluded, p.Name+": ControlPlaneHealthy is not True")
			continue
		case props[properties.Draining] == "true":
			excluded = append(excluded, p.Name+": draining")
			continue
		}
		if req != nil {
			violated := ""
			keys := make([]string, 0, len(req.MatchProperties))
			for k := range req.MatchProperties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if props[k] != req.MatchProperties[k] {
					violated = fmt.Sprintf("%s=%q does not satisfy %s=%s", k, props[k], k, req.MatchProperties[k])
					break
				}
			}
			if violated != "" {
				excluded = append(excluded, p.Name+": "+violated)
				continue
			}
		}
		free, _ := strconv.ParseInt(props[s.CapacityProperty], 10, 64)
		total, totalErr := strconv.ParseInt(props[s.TotalProperty], 10, 64)
		capacity := accountCapacity(total, free, totalErr == nil, led.own[p.Name], led.others[p.Name])
		if capacity <= 0 {
			excluded = append(excluded, fmt.Sprintf("%s: no capacity (%s=%q, %s=%q, %d reserved by other decisions)",
				p.Name, s.TotalProperty, props[s.TotalProperty], s.CapacityProperty, props[s.CapacityProperty], led.others[p.Name]))
			continue
		}
		cost := math.Inf(1)
		if v, err := strconv.ParseFloat(props[properties.CostTier], 64); err == nil {
			cost = v
		}
		cands = append(cands, Candidate{Name: p.Name, Capacity: capacity, CostTier: cost, Spot: props[properties.CapacityType] == "spot"})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Name < cands[j].Name })
	return cands, excluded
}

// render builds the PlacementDecision for an assignment.
func (s *Scheduler) render(fgs *fleetv1alpha1.FleetGenAIService, name, placementKey, policy string, requested int64, cands []Candidate, assignment Assignment, excluded []string, policyErr error) *clusterinventoryv1alpha1.PlacementDecision {
	pd := &clusterinventoryv1alpha1.PlacementDecision{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterinventoryv1alpha1.GroupVersion.String(), Kind: clusterinventoryv1alpha1.PlacementDecisionKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fgs.Namespace,
			Labels: map[string]string{
				clusterinventoryv1alpha1.PlacementKeyLabel:  placementKey,
				clusterinventoryv1alpha1.DecisionIndexLabel: "0",
			},
			Annotations: map[string]string{},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: fleetv1alpha1.GroupVersion.String(), Kind: "FleetGenAIService",
				Name: fgs.Name, UID: fgs.UID,
			}},
		},
		SchedulerName: SchedulerName,
		Decisions:     []clusterinventoryv1alpha1.ClusterDecision{},
	}
	capacityOf := map[string]int64{}
	for _, c := range cands {
		capacityOf[c.Name] = c.Capacity
	}
	names := make([]string, 0, len(assignment))
	for n := range assignment {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pd.Decisions = append(pd.Decisions, clusterinventoryv1alpha1.ClusterDecision{
			ClusterProfileRef: clusterinventoryv1alpha1.ClusterProfileReference{Namespace: s.FleetNamespace, Name: n},
			Reason:            fmt.Sprintf("%d of %d available (%s)", assignment[n], capacityOf[n], policy),
		})
		pd.Annotations[fleetv1alpha1.ParametersAnnotationPrefix+n] = fmt.Sprintf(`{"%s":"%d"}`, fleetv1alpha1.ReplicasParameter, assignment[n])
	}

	total := assignment.Total()
	var reason string
	switch {
	case policyErr != nil:
		reason = policyErr.Error()
	case total == 0:
		reason = fmt.Sprintf("no eligible cluster for %d replica(s): %s", requested, strings.Join(excluded, "; "))
	case total < requested:
		reason = fmt.Sprintf("requested %d, placeable %d across %s; shortfall %d not spilled to ineligible clusters (%s)", requested, total, strings.Join(names, ","), requested-total, strings.Join(excluded, "; "))
	default:
		reason = fmt.Sprintf("%d replicas across %d cluster(s) by %s", total, len(names), policy)
		if len(excluded) > 0 {
			reason += "; excluded: " + strings.Join(excluded, "; ")
		}
	}
	pd.Annotations[fleetv1alpha1.DecisionReasonAnnotation] = reason
	return pd
}

func requestedReplicas(fgs *fleetv1alpha1.FleetGenAIService) int64 {
	spec := map[string]interface{}{}
	if len(fgs.Spec.Template.Spec.Raw) > 0 {
		_ = json.Unmarshal(fgs.Spec.Template.Spec.Raw, &spec)
	}
	if n := toInt64(spec[fleetv1alpha1.ReplicasParameter]); n > 0 {
		return n
	}
	return 1
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}
