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
	"fmt"
	"math"
	"sort"
)

// Policies the demo scheduler understands. Deliberately trivial: the point
// is that kro-fleet converges on whatever decision comes out, not the
// quality of the decision. OCM, Karmada or Kueue would replace all of this.
const (
	PolicySpread        = "spread"         // balance across eligible clusters
	PolicyCheapestFirst = "cheapest-first" // fill the cheapest cost tier first
	PolicySpotFirst     = "spot-first"     // fill spot capacity first, then cheapest
	PolicyBinPack       = "bin-pack"       // fewest clusters: fill the largest first
	// PolicyManual tells this scheduler to leave the instance alone: someone
	// else (a person, another producer) writes its decision.
	PolicyManual = "manual"
)

// Candidate is an eligible cluster as the policy sees it.
type Candidate struct {
	Name     string
	Capacity int64   // replicas this cluster can take for this workload
	CostTier float64 // lower is cheaper; +Inf when unknown
	Spot     bool
}

// Assignment maps cluster name to replicas (only clusters with > 0).
type Assignment map[string]int64

// Total sums the assigned replicas.
func (a Assignment) Total() int64 {
	var t int64
	for _, n := range a {
		t += n
	}
	return t
}

// Assign divides `requested` replicas across candidates under a policy. It
// never assigns more than a candidate's capacity, never invents capacity,
// and is deterministic (ties broken by name).
func Assign(policy string, requested int64, cands []Candidate) (Assignment, error) {
	out := Assignment{}
	if requested <= 0 || len(cands) == 0 {
		return out, nil
	}
	sorted := append([]Candidate(nil), cands...)
	switch policy {
	case "", PolicySpread:
		return spread(requested, sorted), nil
	case PolicyCheapestFirst:
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].CostTier != sorted[j].CostTier {
				return sorted[i].CostTier < sorted[j].CostTier
			}
			return sorted[i].Name < sorted[j].Name
		})
	case PolicySpotFirst:
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Spot != sorted[j].Spot {
				return sorted[i].Spot
			}
			if sorted[i].CostTier != sorted[j].CostTier {
				return sorted[i].CostTier < sorted[j].CostTier
			}
			return sorted[i].Name < sorted[j].Name
		})
	case PolicyBinPack:
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Capacity != sorted[j].Capacity {
				return sorted[i].Capacity > sorted[j].Capacity
			}
			return sorted[i].Name < sorted[j].Name
		})
	default:
		return nil, fmt.Errorf("unknown policy %q (want %s, %s, %s or %s)", policy, PolicySpread, PolicyCheapestFirst, PolicySpotFirst, PolicyBinPack)
	}
	// Fill in order.
	remaining := requested
	for _, c := range sorted {
		if remaining == 0 {
			break
		}
		n := min(c.Capacity, remaining)
		if n > 0 {
			out[c.Name] = n
			remaining -= n
		}
	}
	return out, nil
}

// spread hands out one replica at a time to the candidate with the fewest
// replicas assigned so far that still has capacity (ties by name), which
// balances the assignment across eligible clusters.
func spread(requested int64, cands []Candidate) Assignment {
	out := Assignment{}
	remaining := map[string]int64{}
	for _, c := range cands {
		remaining[c.Name] = c.Capacity
	}
	for i := int64(0); i < requested; i++ {
		best := ""
		var bestAssigned int64 = math.MaxInt64
		for _, c := range cands {
			if remaining[c.Name] <= 0 {
				continue
			}
			if out[c.Name] < bestAssigned || (out[c.Name] == bestAssigned && c.Name < best) {
				best, bestAssigned = c.Name, out[c.Name]
			}
		}
		if best == "" {
			break
		}
		out[best]++
		remaining[best]--
	}
	return out
}
