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
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// ResolvePlacement returns the names of the ClusterProfiles matched by the
// placement selector, sorted for determinism.
//
// An empty selector (no matchLabels, no matchExpressions) matches NO clusters:
// fleet placement is an explicit opt-in, and "select everything by default"
// would be an accidental fleet-wide blast radius. This deliberately diverges
// from the usual "empty selector selects all" Kubernetes convention.
func ResolvePlacement(sel metav1.LabelSelector, profiles []clusterinventoryv1alpha1.ClusterProfile) ([]string, error) {
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return nil, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return nil, fmt.Errorf("invalid clusterSelector: %w", err)
	}
	var matched []string
	for i := range profiles {
		if selector.Matches(labels.Set(profiles[i].Labels)) {
			matched = append(matched, profiles[i].Name)
		}
	}
	sort.Strings(matched)
	return matched, nil
}

// MemberHealthy is the health gate on a member: its ClusterProfile must exist
// and carry ControlPlaneHealthy=True from its cluster manager. The
// cluster-inventory-api provider (v0.25.2) checks this condition only
// before engaging a member and keeps an already-engaged member engaged when
// it later turns unhealthy, so the controller enforces it on every pass:
// an unhealthy member is neither written to nor cleaned up until it
// recovers (or is deregistered, which turns its records into orphans).
func MemberHealthy(profile *clusterinventoryv1alpha1.ClusterProfile) bool {
	if profile == nil {
		return false
	}
	return meta.IsStatusConditionTrue(profile.Status.Conditions, clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy)
}

// MemberReady decides whether a placed GenAIService (a kro instance, read as
// unstructured from a member) has been expanded: kro marks expanded
// instances with status.state=ACTIVE and/or a true Ready / InstanceSynced
// condition (see docs/KEP-GAP.md). Replica readiness is judged separately
// from status.readyReplicas (MemberReadyReplicas).
func MemberReady(obj *unstructured.Unstructured) (bool, string) {
	if obj == nil {
		return false, "placed object not found"
	}
	if state, _, _ := unstructured.NestedString(obj.Object, "status", "state"); state != "" {
		if state == "ACTIVE" {
			return true, "instance state ACTIVE"
		}
		return false, fmt.Sprintf("instance state %s", state)
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		if t != "Ready" && t != "InstanceSynced" {
			continue
		}
		if status, _ := cond["status"].(string); status == "True" {
			return true, fmt.Sprintf("condition %s=True", t)
		}
		reason, _ := cond["reason"].(string)
		return false, fmt.Sprintf("condition %s not True (%s)", t, reason)
	}
	return false, "no readiness signal in status yet"
}

// MemberReadyReplicas reads the ready replica count a member reports for the
// placed object (the sister RGD surfaces status.readyReplicas). The second
// value is false when the object does not report one.
func MemberReadyReplicas(obj *unstructured.Unstructured) (int32, bool) {
	if obj == nil {
		return 0, false
	}
	n, found, err := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	if err != nil || !found {
		return 0, false
	}
	return int32(n), true
}

// MemberEndpoint reads the externally reachable endpoint a member reports
// for the placed object (status.externalEndpoint on the fleet RGD), or "".
func MemberEndpoint(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	s, _, _ := unstructured.NestedString(obj.Object, "status", "externalEndpoint")
	return s
}

// RollupInput is what the fleet-level conditions are computed from.
type RollupInput struct {
	// Placed is the Placed condition already computed from the resolved
	// placement; when it is not True and nothing is placed, Ready follows.
	Placed metav1.Condition

	// PlacedClusters / ReadyClusters count members.
	PlacedClusters, ReadyClusters int
	// RequestedReplicas is the template's total; AssignedReplicas what the
	// decision handed out; ReadyReplicas what members report.
	RequestedReplicas, AssignedReplicas, ReadyReplicas int32
	// Divided is true when per-member replicas parameters are in play, i.e.
	// the template is divided across members rather than replicated.
	Divided bool

	Tolerance *fleetv1alpha1.Tolerance
}

// RollupReady computes the fleet-level Ready condition.
//
// Replicated placement (every member runs the full template): Ready iff at
// least tolerance.minReadyClusters members are ready (default: all).
// Divided placement (the decision carries per-member replicas), or an
// explicit tolerance.minReadyReplicas: Ready iff the ready replicas summed
// across members reach the minimum (default: the requested total).
//
// Nothing placed is never Ready — not even with an explicit zero tolerance.
// A refused or pending placement is an explicit, separate state (the Placed
// condition), never a vacuous success.
func RollupReady(in RollupInput) metav1.Condition {
	cond := metav1.Condition{Type: fleetv1alpha1.ConditionReady}
	if in.PlacedClusters == 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = fleetv1alpha1.ReasonNotPlaced
		cond.Message = "nothing is placed"
		if in.Placed.Message != "" {
			cond.Message = in.Placed.Message
		}
		return cond
	}

	byReplicas := in.Divided || (in.Tolerance != nil && in.Tolerance.MinReadyReplicas != nil)
	if byReplicas {
		minReady := in.RequestedReplicas
		if in.Tolerance != nil && in.Tolerance.MinReadyReplicas != nil {
			minReady = *in.Tolerance.MinReadyReplicas
		}
		cond.Message = fmt.Sprintf("%d/%d replicas ready across %d/%d clusters (minimum %d)",
			in.ReadyReplicas, in.RequestedReplicas, in.ReadyClusters, in.PlacedClusters, minReady)
		if in.ReadyReplicas >= minReady && in.ReadyReplicas > 0 {
			cond.Status, cond.Reason = metav1.ConditionTrue, "MinReadyReplicasMet"
		} else {
			cond.Status, cond.Reason = metav1.ConditionFalse, "MinReadyReplicasNotMet"
		}
		return cond
	}

	minReady := in.PlacedClusters
	if in.Tolerance != nil && in.Tolerance.MinReadyClusters != nil {
		minReady = int(*in.Tolerance.MinReadyClusters)
	}
	cond.Message = fmt.Sprintf("%d/%d placed clusters ready (minimum %d)", in.ReadyClusters, in.PlacedClusters, minReady)
	if in.ReadyClusters >= minReady && in.ReadyClusters > 0 {
		cond.Status, cond.Reason = metav1.ConditionTrue, "MinReadyClustersMet"
	} else {
		cond.Status, cond.Reason = metav1.ConditionFalse, "MinReadyClustersNotMet"
	}
	return cond
}

// ClassifyPlacement turns a resolved placement into the Placed condition and
// the member set to act on. Fail closed: a decision with any violation
// places nothing; a partial decision (fewer replicas than requested) places
// exactly its slice and says so; an empty decision is terminal.
func ClassifyPlacement(p ResolvedPlacement, requestedReplicas int32, decisionLabel string) (metav1.Condition, []ResolvedMember) {
	cond := metav1.Condition{Type: fleetv1alpha1.ConditionPlaced, Status: metav1.ConditionFalse}
	switch {
	case p.Invalid != "":
		cond.Reason, cond.Message = fleetv1alpha1.ReasonInvalidPlacement, p.Invalid
		return cond, nil
	case p.Pending:
		cond.Reason = fleetv1alpha1.ReasonDecisionPending
		cond.Message = fmt.Sprintf("waiting for PlacementDecision %s", decisionLabel)
		return cond, nil
	case len(p.Violations) > 0:
		cond.Reason = fleetv1alpha1.ReasonDecisionViolatesRequirements
		cond.Message = "refused, nothing placed: " + strings.Join(p.Violations, "; ")
		return cond, nil
	case len(p.Members) == 0:
		cond.Reason = fleetv1alpha1.ReasonNoEligibleClusters
		switch {
		case p.Reason != "":
			cond.Message = p.Reason
		case p.Source == fleetv1alpha1.PlacementSourceSelector:
			cond.Message = "placement selector matches no registered clusters"
		default:
			cond.Message = fmt.Sprintf("PlacementDecision %s is empty: no cluster qualifies", decisionLabel)
		}
		return cond, nil
	}

	if p.Divided() && requestedReplicas > 0 {
		var assigned int32
		for _, m := range p.Members {
			assigned += replicasOf(m.Parameters, 0)
		}
		if assigned < requestedReplicas {
			cond.Reason = fleetv1alpha1.ReasonInsufficientCapacity
			cond.Message = fmt.Sprintf("decision assigns %d of %d requested replicas; the shortfall is not spilled elsewhere", assigned, requestedReplicas)
			if p.Reason != "" {
				cond.Message += ": " + p.Reason
			}
			return cond, p.Members
		}
	}
	cond.Status, cond.Reason = metav1.ConditionTrue, fleetv1alpha1.ReasonPlaced
	cond.Message = fmt.Sprintf("placed on %d cluster(s) via %s", len(p.Members), p.Source)
	return cond, p.Members
}
