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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

func profile(name string, lbls map[string]string) clusterinventoryv1alpha1.ClusterProfile {
	return clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet-system", Labels: lbls},
	}
}

func TestResolvePlacement(t *testing.T) {
	profiles := []clusterinventoryv1alpha1.ClusterProfile{
		profile("aks-prod", map[string]string{"tier": "prod", "cloud": "azure"}),
		profile("gke-prod", map[string]string{"tier": "prod", "cloud": "gcp"}),
		profile("gke-dev", map[string]string{"tier": "dev", "cloud": "gcp"}),
	}

	tests := []struct {
		name string
		sel  metav1.LabelSelector
		want []string
	}{
		{
			name: "empty selector matches nothing (explicit opt-in)",
			sel:  metav1.LabelSelector{},
			want: nil,
		},
		{
			name: "matchLabels selects and sorts",
			sel:  metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			want: []string{"aks-prod", "gke-prod"},
		},
		{
			name: "matchExpressions",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "cloud", Operator: metav1.LabelSelectorOpIn, Values: []string{"gcp"}},
			}},
			want: []string{"gke-dev", "gke-prod"},
		},
		{
			name: "no match",
			sel:  metav1.LabelSelector{MatchLabels: map[string]string{"tier": "staging"}},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePlacement(tt.sel, profiles)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolvePlacementInvalidSelector(t *testing.T) {
	sel := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "tier", Operator: "Bogus"},
	}}
	if _, err := ResolvePlacement(sel, nil); err == nil {
		t.Fatal("expected error for invalid selector operator")
	}
}

func TestMemberReady(t *testing.T) {
	obj := func(status map[string]interface{}) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if status != nil {
			u.Object["status"] = status
		}
		return u
	}

	tests := []struct {
		name  string
		obj   *unstructured.Unstructured
		ready bool
	}{
		{"nil object", nil, false},
		{"no status", obj(nil), false},
		{"state ACTIVE", obj(map[string]interface{}{"state": "ACTIVE"}), true},
		{"state FAILED", obj(map[string]interface{}{"state": "FAILED"}), false},
		{"Ready condition true", obj(map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}},
		}), true},
		{"InstanceSynced true", obj(map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "InstanceSynced", "status": "True"}},
		}), true},
		{"Ready condition false", obj(map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "False", "reason": "Expanding"}},
		}), false},
		{"state wins over conditions", obj(map[string]interface{}{
			"state":      "FAILED",
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}},
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, msg := MemberReady(tt.obj)
			if ready != tt.ready {
				t.Errorf("ready = %v (%s), want %v", ready, msg, tt.ready)
			}
			if msg == "" {
				t.Error("message must never be empty")
			}
		})
	}
}

func TestRollupReady(t *testing.T) {
	tol := func(clusters, replicas *int32) *fleetv1alpha1.Tolerance {
		return &fleetv1alpha1.Tolerance{MinReadyClusters: clusters, MinReadyReplicas: replicas}
	}
	placedTrue := metav1.Condition{Type: fleetv1alpha1.ConditionPlaced, Status: metav1.ConditionTrue, Reason: fleetv1alpha1.ReasonPlaced}
	refused := metav1.Condition{Type: fleetv1alpha1.ConditionPlaced, Status: metav1.ConditionFalse, Reason: fleetv1alpha1.ReasonNoEligibleClusters, Message: "nothing qualifies"}

	tests := []struct {
		name   string
		in     RollupInput
		status metav1.ConditionStatus
		reason string
	}{
		{"replicated: all ready, no tolerance", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 3}, metav1.ConditionTrue, "MinReadyClustersMet"},
		{"replicated: one lagging, no tolerance means all", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 2}, metav1.ConditionFalse, "MinReadyClustersNotMet"},
		{"replicated: one lagging, tolerated", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 2, Tolerance: tol(ptr.To[int32](2), nil)}, metav1.ConditionTrue, "MinReadyClustersMet"},
		{"replicated: below tolerance", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 1, Tolerance: tol(ptr.To[int32](2), nil)}, metav1.ConditionFalse, "MinReadyClustersNotMet"},
		{"nothing placed is never ready", RollupInput{Placed: refused}, metav1.ConditionFalse, fleetv1alpha1.ReasonNotPlaced},
		{"nothing placed, explicit zero tolerance is not a vacuous success", RollupInput{Placed: refused, Tolerance: tol(ptr.To[int32](0), nil)}, metav1.ConditionFalse, fleetv1alpha1.ReasonNotPlaced},
		{"divided: all replicas ready", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 3, RequestedReplicas: 8, AssignedReplicas: 8, ReadyReplicas: 8, Divided: true}, metav1.ConditionTrue, "MinReadyReplicasMet"},
		{"divided: short by one replica", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 2, RequestedReplicas: 8, AssignedReplicas: 8, ReadyReplicas: 7, Divided: true}, metav1.ConditionFalse, "MinReadyReplicasNotMet"},
		{"divided: tolerated shortfall", RollupInput{Placed: placedTrue, PlacedClusters: 3, ReadyClusters: 2, RequestedReplicas: 8, AssignedReplicas: 8, ReadyReplicas: 6, Divided: true, Tolerance: tol(nil, ptr.To[int32](6))}, metav1.ConditionTrue, "MinReadyReplicasMet"},
		{"replicated with explicit replica tolerance", RollupInput{Placed: placedTrue, PlacedClusters: 2, ReadyClusters: 2, RequestedReplicas: 4, ReadyReplicas: 3, Tolerance: tol(nil, ptr.To[int32](3))}, metav1.ConditionTrue, "MinReadyReplicasMet"},
		{"divided: zero ready is never ready even with zero minimum", RollupInput{Placed: placedTrue, PlacedClusters: 1, RequestedReplicas: 2, Divided: true, Tolerance: tol(nil, ptr.To[int32](0))}, metav1.ConditionFalse, "MinReadyReplicasNotMet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RollupReady(tt.in)
			if got.Status != tt.status || got.Reason != tt.reason {
				t.Errorf("got %s/%s (%s), want %s/%s", got.Status, got.Reason, got.Message, tt.status, tt.reason)
			}
			if got.Type != fleetv1alpha1.ConditionReady {
				t.Errorf("condition type = %q, want Ready", got.Type)
			}
		})
	}
}
