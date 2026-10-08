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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

const fleetNS = "fleet-system"

func profileWithProps(name string, props map[string]string) clusterinventoryv1alpha1.ClusterProfile {
	p := clusterinventoryv1alpha1.ClusterProfile{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fleetNS}}
	for k, v := range props {
		p.Status.Properties = append(p.Status.Properties, clusterinventoryv1alpha1.Property{Name: k, Value: v})
	}
	return p
}

func decision(name string, labels, annotations map[string]string, clusters ...string) clusterinventoryv1alpha1.PlacementDecision {
	pd := clusterinventoryv1alpha1.PlacementDecision{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet-demo", Labels: labels, Annotations: annotations}, SchedulerName: "demo"}
	for _, c := range clusters {
		pd.Decisions = append(pd.Decisions, clusterinventoryv1alpha1.ClusterDecision{
			ClusterProfileRef: clusterinventoryv1alpha1.ClusterProfileReference{Name: c, Namespace: fleetNS},
			Reason:            "chosen",
		})
	}
	return pd
}

func TestResolveDecisionMergesSlicesInIndexOrderWithParameters(t *testing.T) {
	profiles := []clusterinventoryv1alpha1.ClusterProfile{profileWithProps("eks", nil), profileWithProps("gke", nil), profileWithProps("aks", nil)}
	slices := []clusterinventoryv1alpha1.PlacementDecision{
		decision("d-1", map[string]string{clusterinventoryv1alpha1.DecisionIndexLabel: "1"},
			map[string]string{fleetv1alpha1.ParametersAnnotationPrefix + "aks": `{"replicas": 2}`}, "aks"),
		decision("d-0", map[string]string{clusterinventoryv1alpha1.DecisionIndexLabel: "0"},
			map[string]string{
				fleetv1alpha1.ParametersAnnotationPrefix + "eks": `{"replicas":"4"}`,
				fleetv1alpha1.ParametersAnnotationPrefix + "gke": `{"replicas":"2","spot":true}`,
				fleetv1alpha1.DecisionReasonAnnotation:           "cheapest first",
			}, "eks", "gke", "eks"),
	}
	got := ResolveDecision(slices, "fleet-demo", fleetNS, nil, profiles)
	if got.Pending || len(got.Violations) != 0 {
		t.Fatalf("unexpected pending/violations: %+v", got)
	}
	if !reflect.DeepEqual(got.Names(), []string{"eks", "gke", "aks"}) {
		t.Errorf("order must follow decision-index then slice order, got %v", got.Names())
	}
	if !reflect.DeepEqual(got.DecisionNames, []string{"d-0", "d-1"}) {
		t.Errorf("decision names = %v", got.DecisionNames)
	}
	if got.SchedulerName != "demo" || got.Reason != "cheapest first" {
		t.Errorf("provenance not carried: %+v", got)
	}
	want := map[string]map[string]string{
		"eks": {"replicas": "4"},
		"gke": {"replicas": "2", "spot": "true"},
		"aks": {"replicas": "2"},
	}
	for _, m := range got.Members {
		if !reflect.DeepEqual(m.Parameters, want[m.Name]) {
			t.Errorf("%s parameters = %v, want %v", m.Name, m.Parameters, want[m.Name])
		}
	}
	if !got.Divided() {
		t.Error("replicas parameters must mark the placement as divided")
	}
}

func TestResolveDecisionFailsClosed(t *testing.T) {
	profiles := []clusterinventoryv1alpha1.ClusterProfile{
		profileWithProps("gov", map[string]string{"compliance.example.com": "fedramp-high"}),
		profileWithProps("pub", map[string]string{"compliance.example.com": "none"}),
		profileWithProps("bare", nil),
	}
	req := &fleetv1alpha1.Requirements{MatchProperties: map[string]string{"compliance.example.com": "fedramp-high"}}

	t.Run("unknown cluster is a violation", func(t *testing.T) {
		got := ResolveDecision([]clusterinventoryv1alpha1.PlacementDecision{decision("d", nil, nil, "gov", "ghost")}, "fleet-demo", fleetNS, req, profiles)
		if len(got.Violations) != 1 || !strings.Contains(got.Violations[0], "ghost") {
			t.Errorf("expected a violation naming ghost, got %v", got.Violations)
		}
	})
	t.Run("non-compliant cluster is a violation, compliant one is kept", func(t *testing.T) {
		got := ResolveDecision([]clusterinventoryv1alpha1.PlacementDecision{decision("d", nil, nil, "gov", "pub", "bare")}, "fleet-demo", fleetNS, req, profiles)
		if len(got.Violations) != 2 {
			t.Errorf("expected 2 violations (pub, bare), got %v", got.Violations)
		}
		if !reflect.DeepEqual(got.Names(), []string{"gov"}) {
			t.Errorf("members = %v, want [gov]", got.Names())
		}
	})
	t.Run("reference outside the fleet namespace is a violation", func(t *testing.T) {
		pd := decision("d", nil, nil, "gov")
		pd.Decisions[0].ClusterProfileRef.Namespace = "elsewhere"
		got := ResolveDecision([]clusterinventoryv1alpha1.PlacementDecision{pd}, "fleet-demo", fleetNS, nil, profiles)
		if len(got.Violations) != 1 {
			t.Errorf("expected 1 violation, got %v", got.Violations)
		}
	})
	t.Run("unset reference namespace means the decision's namespace (KEP-5313)", func(t *testing.T) {
		pd := decision("d", nil, nil, "gov")
		pd.Decisions[0].ClusterProfileRef.Namespace = ""
		got := ResolveDecision([]clusterinventoryv1alpha1.PlacementDecision{pd}, fleetNS, fleetNS, nil, profiles)
		if len(got.Violations) != 0 || len(got.Members) != 1 {
			t.Errorf("a decision in the fleet namespace may omit the namespace, got %+v", got)
		}
	})
}

func TestClassifyPlacement(t *testing.T) {
	member := func(name string, replicas string) ResolvedMember {
		m := ResolvedMember{Name: name}
		if replicas != "" {
			m.Parameters = map[string]string{fleetv1alpha1.ReplicasParameter: replicas}
		}
		return m
	}
	tests := []struct {
		name      string
		p         ResolvedPlacement
		requested int32
		reason    string
		status    metav1.ConditionStatus
		members   int
	}{
		{"invalid block", ResolvedPlacement{Invalid: "bad"}, 8, fleetv1alpha1.ReasonInvalidPlacement, metav1.ConditionFalse, 0},
		{"pending decision", ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceDecision, Pending: true}, 8, fleetv1alpha1.ReasonDecisionPending, metav1.ConditionFalse, 0},
		{"violation refuses everything", ResolvedPlacement{Members: []ResolvedMember{member("gov", "4")}, Violations: []string{"pub: not compliant"}}, 8, fleetv1alpha1.ReasonDecisionViolatesRequirements, metav1.ConditionFalse, 0},
		{"empty decision is terminal", ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceDecision, Reason: "no cluster has free GPUs"}, 8, fleetv1alpha1.ReasonNoEligibleClusters, metav1.ConditionFalse, 0},
		{"empty selector match is terminal", ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceSelector}, 1, fleetv1alpha1.ReasonNoEligibleClusters, metav1.ConditionFalse, 0},
		{"partial decision places its slice", ResolvedPlacement{Members: []ResolvedMember{member("gov", "4")}, Reason: "compliant capacity is 4"}, 6, fleetv1alpha1.ReasonInsufficientCapacity, metav1.ConditionFalse, 1},
		{"full decision", ResolvedPlacement{Members: []ResolvedMember{member("eks", "4"), member("gke", "2"), member("aks", "2")}}, 8, fleetv1alpha1.ReasonPlaced, metav1.ConditionTrue, 3},
		{"replicated selector placement", ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceSelector, Members: []ResolvedMember{member("a", ""), member("b", "")}}, 1, fleetv1alpha1.ReasonPlaced, metav1.ConditionTrue, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, members := ClassifyPlacement(tt.p, tt.requested, "d")
			if cond.Reason != tt.reason || cond.Status != tt.status {
				t.Errorf("got %s/%s (%s), want %s/%s", cond.Status, cond.Reason, cond.Message, tt.status, tt.reason)
			}
			if len(members) != tt.members {
				t.Errorf("members = %d, want %d", len(members), tt.members)
			}
			if cond.Message == "" {
				t.Error("message must never be empty")
			}
		})
	}
}

func TestParametersForTolerantParsing(t *testing.T) {
	pd := decision("d", nil, map[string]string{
		fleetv1alpha1.ParametersAnnotationPrefix + "ok":     `{"replicas": 3, "spot": false, "tier": "cheap"}`,
		fleetv1alpha1.ParametersAnnotationPrefix + "broken": `not json`,
	}, "ok", "broken")
	got := parametersFor(&pd, "ok")
	want := map[string]string{"replicas": "3", "spot": "false", "tier": "cheap"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if parametersFor(&pd, "broken") != nil {
		t.Error("unparseable parameters must be ignored, not fail the decision")
	}
	if parametersFor(&pd, "absent") != nil {
		t.Error("absent parameters must be nil")
	}
	if replicasOf(map[string]string{"replicas": "x"}, 5) != 5 || replicasOf(nil, 5) != 5 || replicasOf(map[string]string{"replicas": "2"}, 5) != 2 {
		t.Error("replicasOf must fall back to the template value")
	}
}
