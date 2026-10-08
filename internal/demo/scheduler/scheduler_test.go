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

package scheduler

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

func TestAccountCapacity(t *testing.T) {
	cases := []struct {
		name                string
		total, free         int64
		hasTotal            bool
		own, others, expect int64
	}{
		{"idle cluster", 4, 4, true, 0, 0, 4},
		{"own replicas landed: free excludes them, the ledger credits them back", 4, 2, true, 2, 0, 4},
		// The ratchet: a new decision assigned 4 but only 2 pods have landed, so
		// free still says 2. Advertised free + own would be 6; the ledger holds 4.
		{"own replicas assigned but not landed yet", 4, 2, true, 4, 0, 4},
		{"own replicas all landed", 4, 0, true, 4, 0, 4},
		{"over-assigned by an earlier mistake: capped at the total", 4, 0, true, 8, 0, 4},
		{"another instance's assignment is reserved before its pods land", 4, 4, true, 0, 4, 0},
		{"another instance's assignment, landed", 4, 0, true, 0, 4, 0},
		{"shared: others hold 1 (landed), room for 3", 4, 3, true, 0, 1, 3},
		{"consumption nobody here placed is external", 4, 1, true, 0, 0, 1},
		{"external plus own, landed", 4, 0, true, 2, 0, 2},
		{"no total advertised: free + own, the only formula left", 0, 2, false, 2, 0, 4},
		{"never negative", 4, 0, true, 0, 6, 0},
	}
	for _, c := range cases {
		if got := accountCapacity(c.total, c.free, c.hasTotal, c.own, c.others); got != c.expect {
			t.Errorf("%s: accountCapacity(total=%d free=%d hasTotal=%v own=%d others=%d) = %d, want %d",
				c.name, c.total, c.free, c.hasTotal, c.own, c.others, got, c.expect)
		}
	}
}

// The cheapest-first consolidation of the demo: 8 replicas spread 3/3/2 over
// three clusters with 4 accelerators each, then re-decided cheapest-first.
// The re-decision must be 4/4/0 and stay 4/4/0 while the pods move, whatever
// the advertised free counts say in the meantime.
func TestCheapestFirstDoesNotRatchet(t *testing.T) {
	type cluster struct{ free, own int64 }
	steps := []map[string]cluster{
		{"eks": {2, 2}, "gke": {1, 3}, "aks": {1, 3}}, // spread 3/3/2 has landed
		{"eks": {2, 4}, "gke": {1, 4}, "aks": {1, 0}}, // re-decided 4/4/0; nothing has moved yet
		{"eks": {0, 4}, "gke": {0, 4}, "aks": {4, 0}}, // everything landed
	}
	cost := map[string]float64{"eks": 1, "gke": 2, "aks": 3}
	for i, st := range steps {
		var cands []Candidate
		for _, n := range []string{"aks", "eks", "gke"} {
			cands = append(cands, Candidate{Name: n, Capacity: accountCapacity(4, st[n].free, true, st[n].own, 0), CostTier: cost[n]})
		}
		a, err := Assign(PolicyCheapestFirst, 8, cands)
		if err != nil {
			t.Fatal(err)
		}
		if a["eks"] != 4 || a["gke"] != 4 || a["aks"] != 0 {
			t.Errorf("step %d: cheapest-first assigned %v, want eks 4 / gke 4 / aks 0", i, a)
		}
	}
}

func TestAssignedReplicas(t *testing.T) {
	pd := &clusterinventoryv1alpha1.PlacementDecision{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"parameters.fleet.kro.run/a": `{"replicas":"4"}`,
		"parameters.fleet.kro.run/b": `{"replicas":2}`,
		"parameters.fleet.kro.run/c": `not json`,
	}}}
	for cluster, want := range map[string]int64{"a": 4, "b": 2, "c": 0, "d": 0} {
		if got := assignedReplicas(pd, cluster); got != want {
			t.Errorf("assignedReplicas(%s) = %d, want %d", cluster, got, want)
		}
	}
}
