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
	tol := func(n int32) *fleetv1alpha1.Tolerance {
		return &fleetv1alpha1.Tolerance{MinReadyClusters: ptr.To(n)}
	}

	tests := []struct {
		name   string
		placed int
		ready  int
		tol    *fleetv1alpha1.Tolerance
		status metav1.ConditionStatus
		reason string
	}{
		{"all ready, no tolerance", 3, 3, nil, metav1.ConditionTrue, "MinReadyClustersMet"},
		{"one lagging, no tolerance means all", 3, 2, nil, metav1.ConditionFalse, "MinReadyClustersNotMet"},
		{"one lagging, tolerated", 3, 2, tol(2), metav1.ConditionTrue, "MinReadyClustersMet"},
		{"below tolerance", 3, 1, tol(2), metav1.ConditionFalse, "MinReadyClustersNotMet"},
		{"zero placed, no tolerance", 0, 0, nil, metav1.ConditionFalse, "NoMatchingClusters"},
		{"zero placed but explicitly tolerated", 0, 0, tol(0), metav1.ConditionTrue, "MinReadyClustersMet"},
		{"nil tolerance struct field", 2, 2, &fleetv1alpha1.Tolerance{}, metav1.ConditionTrue, "MinReadyClustersMet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RollupReady(tt.placed, tt.ready, tt.tol)
			if got.Status != tt.status || got.Reason != tt.reason {
				t.Errorf("got %s/%s, want %s/%s", got.Status, got.Reason, tt.status, tt.reason)
			}
			if got.Type != "Ready" {
				t.Errorf("condition type = %q, want Ready", got.Type)
			}
		})
	}
}

func TestMergeIntent(t *testing.T) {
	existing := []fleetv1alpha1.ClusterStatus{
		{Name: "member-2", Ready: true, Message: "instance state ACTIVE"},
	}
	got := mergeIntent(existing, map[string]bool{"member-1": true, "member-2": true, "member-3": true})

	want := []fleetv1alpha1.ClusterStatus{
		{Name: "member-1", Ready: false, Message: "placing"},
		{Name: "member-2", Ready: true, Message: "instance state ACTIVE"}, // preserved, not reset
		{Name: "member-3", Ready: false, Message: "placing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
