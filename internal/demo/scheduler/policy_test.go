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
	"math"
	"reflect"
	"testing"
)

func TestAssign(t *testing.T) {
	// The demo fleet: no single cluster has 8 free.
	fleet := []Candidate{
		{Name: "aks", Capacity: 4, CostTier: 3, Spot: true},
		{Name: "eks", Capacity: 4, CostTier: 1},
		{Name: "gke", Capacity: 4, CostTier: 2},
	}
	tests := []struct {
		name      string
		policy    string
		requested int64
		cands     []Candidate
		want      Assignment
	}{
		{"spread balances", PolicySpread, 8, fleet, Assignment{"aks": 3, "eks": 3, "gke": 2}},
		{"spread respects capacity", PolicySpread, 8, []Candidate{{Name: "a", Capacity: 2}, {Name: "b", Capacity: 10}}, Assignment{"a": 2, "b": 6}},
		{"cheapest first fills by cost tier", PolicyCheapestFirst, 8, fleet, Assignment{"eks": 4, "gke": 4}},
		{"spot first takes spot then cheapest", PolicySpotFirst, 8, fleet, Assignment{"aks": 4, "eks": 4}},
		{"bin-pack fills the largest first", PolicyBinPack, 5, []Candidate{{Name: "small", Capacity: 2}, {Name: "big", Capacity: 6}}, Assignment{"big": 5}},
		{"shortfall is never invented", PolicyCheapestFirst, 6, []Candidate{{Name: "gov", Capacity: 4, CostTier: math.Inf(1)}}, Assignment{"gov": 4}},
		{"no candidates", PolicySpread, 8, nil, Assignment{}},
		{"default policy is spread", "", 2, fleet, Assignment{"aks": 1, "eks": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Assign(tt.policy, tt.requested, tt.cands)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
			if got.Total() > tt.requested {
				t.Errorf("assigned more than requested: %d > %d", got.Total(), tt.requested)
			}
		})
	}
	if _, err := Assign("random", 1, fleet); err == nil {
		t.Error("unknown policy must be rejected")
	}
}
