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

// Package v1alpha1 defines the FleetGenAIService API: a hub-side,
// placement-enabled wrapper around the sister project's GenAIService
// (https://github.com/danbruno101/kro-genaiops-demo). The wrapped spec is
// opaque to this controller — kro on each member expands it — which keeps the
// distributed object cloud/cluster-agnostic while placement stays
// platform-owned. See docs/proposals/KEP-kro-multicluster.md.
//
// +kubebuilder:object:generate=true
// +groupName=fleet.kro.run
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group/version of the fleet PoC types.
var GroupVersion = schema.GroupVersion{Group: "fleet.kro.run", Version: "v1alpha1"}

// Conventions between a PlacementDecision producer and this consumer, for
// what KEP-5313 does not carry (see docs/placement-decision-gaps.md).
const (
	// ParametersAnnotationPrefix + <ClusterProfile name> on a PlacementDecision
	// holds that member's per-member parameters as a JSON object of strings,
	// e.g. parameters.fleet.kro.run/eks-prod: '{"replicas":"4"}'. Set by the
	// producer; absent means every member gets the template as-is
	// (replication).
	ParametersAnnotationPrefix = "parameters.fleet.kro.run/"
	// DecisionReasonAnnotation on a PlacementDecision carries the
	// decision-level reason — why the decision is empty or partial. It is
	// what makes "nothing qualifies" distinguishable from "not decided yet".
	DecisionReasonAnnotation = "fleet.kro.run/decision-reason"

	// ReplicasParameter is the per-member parameter this PoC understands:
	// the replica count for that member's copy.
	ReplicasParameter = "replicas"
)

// Condition types and reasons on a FleetGenAIService.
const (
	ConditionPlaced = "Placed"
	ConditionReady  = "Ready"

	// ReasonPlaced: the placement resolved to members and every copy has
	// been handed to them.
	ReasonPlaced = "Placed"
	// ReasonDecisionPending: a decision is referenced but does not exist
	// yet (the producer has not decided). Not terminal.
	ReasonDecisionPending = "DecisionPending"
	// ReasonNoEligibleClusters: the decision exists and is empty, or the
	// selector matches nothing. Terminal and explicit: no fallback.
	ReasonNoEligibleClusters = "NoEligibleClusters"
	// ReasonInsufficientCapacity: the decision assigns fewer replicas than
	// requested. The decided slice is placed; nothing spills elsewhere.
	ReasonInsufficientCapacity = "InsufficientCapacity"
	// ReasonDecisionViolatesRequirements: the decision names a cluster that
	// is not in the fleet or fails spec.placement.requirements. Refused as
	// a whole: nothing is placed.
	ReasonDecisionViolatesRequirements = "DecisionViolatesRequirements"
	// ReasonInvalidPlacement: the placement block is malformed (no source,
	// or more than one).
	ReasonInvalidPlacement = "InvalidPlacement"
	// ReasonNotPlaced on Ready: nothing is placed, so nothing can be ready.
	ReasonNotPlaced = "NotPlaced"
)

// Placement sources, reported in status.placement.source.
const (
	PlacementSourceSelector = "ClusterSelector"
	PlacementSourceDecision = "PlacementDecision"
)

// FleetGenAIServiceSpec defines the desired state: which GenAIService to
// place, and where.
type FleetGenAIServiceSpec struct {
	// Template is the GenAIService to materialize on each selected member
	// cluster. The placed object gets the same namespace/name as this
	// FleetGenAIService.
	Template GenAIServiceTemplate `json:"template"`

	// Placement says where the template goes. Platform-owned: the
	// developer's template never names a cluster, a cloud or a region.
	Placement Placement `json:"placement"`
}

// GenAIServiceTemplate carries the member-side object to place.
type GenAIServiceTemplate struct {
	// Spec is the GenAIService spec, passed through verbatim to each
	// member — except for values a decision's per-member parameters
	// override (today: replicas). It is intentionally unvalidated here: its
	// schema is owned by the members' ResourceGraphDefinition (stock kro).
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec runtime.RawExtension `json:"spec"`
}

// Placement is the KEP's placement concept: exactly one SOURCE of the member
// set, which kro-fleet consumes and never computes. v1: an inline label
// selector over ClusterProfiles. v2: a PlacementDecision (KEP-5313) written
// by something else — a scheduler, a policy engine, a failover controller —
// referenced by name or discovered by placement key. Both resolve to the
// same internal value, an ordered member set with optional per-member
// parameters, so everything downstream is source-agnostic.
type Placement struct {
	// ClusterSelector selects ClusterProfile objects by label. An empty
	// selector matches no clusters (explicit opt-in, no accidental
	// fleet-wide blast). Mutually exclusive with decisionRef/placementKey.
	// +optional
	ClusterSelector *metav1.LabelSelector `json:"clusterSelector,omitempty"`

	// DecisionRef names a PlacementDecision in this namespace to consume.
	// The decision is read-only input; its producer owns it.
	// +optional
	DecisionRef *DecisionReference `json:"decisionRef,omitempty"`

	// PlacementKey discovers PlacementDecision slices in this namespace by
	// the multicluster.x-k8s.io/placement-key label (KEP-5313's recommended
	// discovery); slices are merged in multicluster.x-k8s.io/decision-index
	// order. Mutually exclusive with decisionRef.
	// +optional
	PlacementKey string `json:"placementKey,omitempty"`

	// Requirements are hard constraints every decided cluster must satisfy.
	// They are INPUT to the producer and a GUARD here: kro-fleet never
	// selects clusters with them, but it refuses — as a whole, placing
	// nothing — a decision that names a cluster violating them
	// (Placed=False/DecisionViolatesRequirements). There is no graceful
	// fallback for a hard constraint.
	// +optional
	Requirements *Requirements `json:"requirements,omitempty"`

	// Tolerance controls how partial readiness folds into the rolled-up
	// Ready condition.
	// +optional
	Tolerance *Tolerance `json:"tolerance,omitempty"`
}

// DecisionReference names a PlacementDecision in the same namespace.
type DecisionReference struct {
	Name string `json:"name"`
}

// Requirements are hard constraints over ClusterProfile status.properties.
type Requirements struct {
	// MatchProperties requires each named property (About API /
	// ClusterProfile.status.properties name) to be present on the member's
	// ClusterProfile with exactly this value.
	// +optional
	MatchProperties map[string]string `json:"matchProperties,omitempty"`
}

// Tolerance mirrors the KEP's spec.placement.tolerance.
type Tolerance struct {
	// MinReadyClusters is the number of placed clusters that must report
	// ready for Ready to be True when the template is replicated (no
	// per-member replica parameters). Unset means "all placed clusters".
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinReadyClusters *int32 `json:"minReadyClusters,omitempty"`

	// MinReadyReplicas is the number of ready replicas, summed across
	// members, required for Ready to be True when the template is divided
	// (the decision carries per-member replicas). Unset means "all requested
	// replicas" (the template's replicas).
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinReadyReplicas *int32 `json:"minReadyReplicas,omitempty"`
}

// FleetGenAIServiceStatus is the fleet view aggregated back onto the hub
// object.
type FleetGenAIServiceStatus struct {
	// ObservedGeneration is the spec generation last acted upon.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Placement records the resolved decision — where the copies were sent
	// and why — so it can be shown after the fact.
	// +optional
	Placement *PlacementStatus `json:"placement,omitempty"`

	// Clusters holds one readiness entry per member the object is placed on
	// (plus members pending removal). It is a view, not the inventory: what
	// was applied where is tracked per (instance, member) in
	// AppliedManifestRecord objects, which drive unplacement and teardown.
	// +optional
	Clusters []ClusterStatus `json:"clusters,omitempty"`

	// Summary counts placements and replicas for quick fleet-level reading.
	// +optional
	Summary Summary `json:"summary,omitempty"`

	// Conditions: Placed (did the placement resolve, and to what) and Ready
	// (per-member readiness folded through spec.placement.tolerance).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PlacementStatus is the provenance of the current placement.
type PlacementStatus struct {
	// Source is ClusterSelector or PlacementDecision.
	Source string `json:"source"`
	// DecisionNames lists the PlacementDecision slice(s) consumed.
	// +optional
	DecisionNames []string `json:"decisionNames,omitempty"`
	// SchedulerName is the decision's schedulerName, when set.
	// +optional
	SchedulerName string `json:"schedulerName,omitempty"`
	// Reason is the decision-level reason the producer gave (why the
	// decision is empty or partial), when set.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Clusters is the resolved member set, in decision order.
	// +optional
	Clusters []PlacedCluster `json:"clusters,omitempty"`
}

// PlacedCluster is one member of the resolved placement.
type PlacedCluster struct {
	// Name is the ClusterProfile name.
	Name string `json:"name"`
	// Reason is the producer's per-cluster reason, when set.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Parameters are the per-member parameters applied to this member's copy.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// ClusterStatus is the per-member slice of the fleet view.
type ClusterStatus struct {
	// Name is the ClusterProfile name (unique within the fleet namespace).
	Name string `json:"name"`

	// Ready reports whether the placed GenAIService is ready on this
	// member (kro instance state ACTIVE or a true Ready condition).
	Ready bool `json:"ready"`

	// AssignedReplicas is the replica count this member's copy was given.
	// +optional
	AssignedReplicas int32 `json:"assignedReplicas,omitempty"`

	// ReadyReplicas is the member's reported ready replica count.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint is the member's externally reachable endpoint for this
	// copy, when the member reports one (e.g. its cloud load balancer).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Message carries a human-oriented note (e.g. why not ready, or that
	// the member is unreachable).
	// +optional
	Message string `json:"message,omitempty"`
}

// Summary counts placements and replicas.
type Summary struct {
	// Placed is the number of members the object is currently applied to.
	Placed int32 `json:"placed"`
	// Ready is the number of those members reporting ready.
	Ready int32 `json:"ready"`
	// RequestedReplicas is the total the template asks for.
	// +optional
	RequestedReplicas int32 `json:"requestedReplicas,omitempty"`
	// AssignedReplicas is the sum of replicas the decision assigned.
	// +optional
	AssignedReplicas int32 `json:"assignedReplicas,omitempty"`
	// ReadyReplicas is the sum of ready replicas reported by members.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fgs
// +kubebuilder:printcolumn:name="Placed",type=integer,JSONPath=`.status.summary.placed`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.summary.ready`
// +kubebuilder:printcolumn:name="Replicas",type=string,JSONPath=`.status.summary.readyReplicas`
// +kubebuilder:printcolumn:name="Requested",type=string,JSONPath=`.status.summary.requestedReplicas`
// +kubebuilder:printcolumn:name="PlacedCond",type=string,JSONPath=`.status.conditions[?(@.type=="Placed")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FleetGenAIService is one GenAIService authored once on the hub and placed
// onto the members its placement resolves to.
type FleetGenAIService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec FleetGenAIServiceSpec `json:"spec"`
	// +optional
	Status FleetGenAIServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetGenAIServiceList contains a list of FleetGenAIService.
type FleetGenAIServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetGenAIService `json:"items"`
}
