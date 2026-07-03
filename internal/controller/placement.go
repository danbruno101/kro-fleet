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

// MemberReady decides whether a placed GenAIService (a kro instance, read as
// unstructured from a member) is ready. Heuristic for the PoC: kro marks
// expanded instances with status.state=ACTIVE and/or a true Ready /
// InstanceSynced condition (see docs/KEP-GAP.md).
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

// RollupReady computes the fleet-level Ready condition from per-member
// readiness and the placement tolerance.
func RollupReady(placed, ready int, tol *fleetv1alpha1.Tolerance) metav1.Condition {
	minReady := placed
	explicitMin := false
	if tol != nil && tol.MinReadyClusters != nil {
		minReady = int(*tol.MinReadyClusters)
		explicitMin = true
	}

	cond := metav1.Condition{
		Type:    "Ready",
		Message: fmt.Sprintf("%d/%d placed clusters ready (minimum %d)", ready, placed, minReady),
	}
	switch {
	case placed == 0 && !explicitMin:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoMatchingClusters"
		cond.Message = "placement selector matches no registered clusters"
	case ready >= minReady:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "MinReadyClustersMet"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "MinReadyClustersNotMet"
	}
	return cond
}
