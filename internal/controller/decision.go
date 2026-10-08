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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// ResolvedPlacement is the one internal value both placement sources
// produce: an ordered member set with optional per-member parameters, plus
// the facts the rollup needs to classify the outcome. kro-fleet consumes it
// and never computes it — this file reads decisions; it ranks nothing.
type ResolvedPlacement struct {
	Source        string
	DecisionNames []string
	SchedulerName string
	Reason        string // decision-level reason from the producer, if any
	Members       []ResolvedMember

	// Pending: a decision is referenced but does not exist yet.
	Pending bool
	// Invalid: the placement block itself is malformed.
	Invalid string
	// Violations: clusters the decision names that are unknown to the fleet
	// or fail spec.placement.requirements. Non-empty => refuse the whole
	// decision (fail closed).
	Violations []string
}

// ResolvedMember is one decided member.
type ResolvedMember struct {
	Name       string
	Reason     string
	Parameters map[string]string
}

// Names returns the member names in decision order.
func (p ResolvedPlacement) Names() []string {
	out := make([]string, 0, len(p.Members))
	for _, m := range p.Members {
		out = append(out, m.Name)
	}
	return out
}

// Divided reports whether any member carries a replicas parameter, i.e. the
// template is divided across members rather than replicated to each.
func (p ResolvedPlacement) Divided() bool {
	for _, m := range p.Members {
		if _, ok := m.Parameters[fleetv1alpha1.ReplicasParameter]; ok {
			return true
		}
	}
	return false
}

// resolvePlacement turns spec.placement into a ResolvedPlacement against the
// fleet inventory, reading the referenced PlacementDecision(s) when the
// source is a decision.
func (r *FleetReconciler) resolvePlacement(ctx context.Context, fgs *fleetv1alpha1.FleetGenAIService, profiles []clusterinventoryv1alpha1.ClusterProfile) (ResolvedPlacement, error) {
	pl := fgs.Spec.Placement
	sources := 0
	if pl.ClusterSelector != nil {
		sources++
	}
	if pl.DecisionRef != nil {
		sources++
	}
	if pl.PlacementKey != "" {
		sources++
	}
	switch {
	case sources == 0:
		return ResolvedPlacement{Invalid: "spec.placement needs exactly one of clusterSelector, decisionRef or placementKey"}, nil
	case sources > 1:
		return ResolvedPlacement{Invalid: "spec.placement must set only one of clusterSelector, decisionRef or placementKey"}, nil
	}

	if pl.ClusterSelector != nil {
		matched, err := ResolvePlacement(*pl.ClusterSelector, profiles)
		if err != nil {
			return ResolvedPlacement{}, err
		}
		res := ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceSelector}
		for _, name := range matched {
			res.Members = append(res.Members, ResolvedMember{Name: name, Reason: "matches clusterSelector"})
		}
		return res, nil
	}

	hub := r.Manager.GetLocalManager().GetClient()
	var slices []clusterinventoryv1alpha1.PlacementDecision
	if pl.DecisionRef != nil {
		pd := &clusterinventoryv1alpha1.PlacementDecision{}
		err := hub.Get(ctx, types.NamespacedName{Namespace: fgs.Namespace, Name: pl.DecisionRef.Name}, pd)
		switch {
		case apierrors.IsNotFound(err):
			return ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceDecision, Pending: true}, nil
		case err != nil:
			return ResolvedPlacement{}, fmt.Errorf("failed to get PlacementDecision %s: %w", pl.DecisionRef.Name, err)
		}
		slices = append(slices, *pd)
	} else {
		list := &clusterinventoryv1alpha1.PlacementDecisionList{}
		if err := hub.List(ctx, list, client.InNamespace(fgs.Namespace),
			client.MatchingLabels{clusterinventoryv1alpha1.PlacementKeyLabel: pl.PlacementKey}); err != nil {
			return ResolvedPlacement{}, fmt.Errorf("failed to list PlacementDecisions for placement-key %s: %w", pl.PlacementKey, err)
		}
		if len(list.Items) == 0 {
			return ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceDecision, Pending: true}, nil
		}
		slices = list.Items
	}
	return ResolveDecision(slices, fgs.Namespace, r.FleetNamespace, pl.Requirements, profiles), nil
}

// ResolveDecision merges PlacementDecision slices (decision-index order,
// then name) into a ResolvedPlacement, validating every cluster reference
// against the fleet inventory and the hard requirements. Pure: no I/O.
func ResolveDecision(slices []clusterinventoryv1alpha1.PlacementDecision, decisionNamespace, fleetNamespace string, req *fleetv1alpha1.Requirements, profiles []clusterinventoryv1alpha1.ClusterProfile) ResolvedPlacement {
	sort.SliceStable(slices, func(i, j int) bool {
		ii, ij := decisionIndex(slices[i]), decisionIndex(slices[j])
		if ii != ij {
			return ii < ij
		}
		return slices[i].Name < slices[j].Name
	})

	byName := map[string]*clusterinventoryv1alpha1.ClusterProfile{}
	for i := range profiles {
		byName[profiles[i].Name] = &profiles[i]
	}

	res := ResolvedPlacement{Source: fleetv1alpha1.PlacementSourceDecision}
	seen := map[string]bool{}
	for i := range slices {
		pd := &slices[i]
		res.DecisionNames = append(res.DecisionNames, pd.Name)
		if pd.SchedulerName != "" {
			res.SchedulerName = pd.SchedulerName
		}
		if reason := pd.Annotations[fleetv1alpha1.DecisionReasonAnnotation]; reason != "" {
			res.Reason = reason
		}
		for _, d := range pd.Decisions {
			name := d.ClusterProfileRef.Name
			ns := d.ClusterProfileRef.Namespace
			if ns == "" {
				ns = decisionNamespace // KEP-5313: unset means the decision's namespace
			}
			if ns != fleetNamespace {
				res.Violations = append(res.Violations, fmt.Sprintf("%s/%s: ClusterProfile is outside the fleet namespace %s", ns, name, fleetNamespace))
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			profile, ok := byName[name]
			if !ok {
				res.Violations = append(res.Violations, fmt.Sprintf("%s: no such ClusterProfile in the fleet", name))
				continue
			}
			if msg := checkRequirements(profile, req); msg != "" {
				res.Violations = append(res.Violations, fmt.Sprintf("%s: %s", name, msg))
				continue
			}
			res.Members = append(res.Members, ResolvedMember{
				Name:       name,
				Reason:     d.Reason,
				Parameters: parametersFor(pd, name),
			})
		}
	}
	return res
}

// decisionIndex reads the multicluster.x-k8s.io/decision-index label (slices
// without it sort last, by name).
func decisionIndex(pd clusterinventoryv1alpha1.PlacementDecision) int {
	v, ok := pd.Labels[clusterinventoryv1alpha1.DecisionIndexLabel]
	if !ok {
		return int(^uint(0) >> 1)
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return i
}

// parametersFor reads the producer's per-member parameters annotation for
// one cluster: parameters.fleet.kro.run/<cluster>: '{"replicas":"4"}'.
// Numbers and booleans are accepted and stringified.
func parametersFor(pd *clusterinventoryv1alpha1.PlacementDecision, cluster string) map[string]string {
	raw, ok := pd.Annotations[fleetv1alpha1.ParametersAnnotationPrefix+cluster]
	if !ok || raw == "" {
		return nil
	}
	var generic map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range generic {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatInt(int64(t), 10)
		case bool:
			out[k] = strconv.FormatBool(t)
		default:
			b, _ := json.Marshal(t)
			out[k] = string(b)
		}
	}
	return out
}

// checkRequirements returns why a profile fails the hard requirements, or "".
func checkRequirements(profile *clusterinventoryv1alpha1.ClusterProfile, req *fleetv1alpha1.Requirements) string {
	if req == nil || len(req.MatchProperties) == 0 {
		return ""
	}
	have := map[string]string{}
	for _, p := range profile.Status.Properties {
		have[p.Name] = p.Value
	}
	keys := make([]string, 0, len(req.MatchProperties))
	for k := range req.MatchProperties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := req.MatchProperties[k]
		got, ok := have[k]
		if !ok {
			return fmt.Sprintf("required property %s=%s is not advertised", k, want)
		}
		if got != want {
			return fmt.Sprintf("required property %s=%s, cluster advertises %q", k, want, got)
		}
	}
	return ""
}

// replicasOf reads a member's replicas parameter, or the template's.
func replicasOf(params map[string]string, templateReplicas int32) int32 {
	if v, ok := params[fleetv1alpha1.ReplicasParameter]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
			return int32(n)
		}
	}
	return templateReplicas
}
