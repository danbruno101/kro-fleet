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

// FleetGenAIServiceSpec defines the desired state: which GenAIService to
// place, and where.
type FleetGenAIServiceSpec struct {
	// Template is the GenAIService to materialize on each selected member
	// cluster. The placed object gets the same namespace/name as this
	// FleetGenAIService.
	Template GenAIServiceTemplate `json:"template"`

	// Placement selects member clusters from the ClusterProfile inventory.
	Placement Placement `json:"placement"`
}

// GenAIServiceTemplate carries the member-side object to place.
type GenAIServiceTemplate struct {
	// Spec is the GenAIService spec, passed through verbatim to each
	// member. It is intentionally unvalidated here: its schema is owned by
	// the members' ResourceGraphDefinition (stock kro).
	// +kubebuilder:pruning:PreserveUnknownFields
	Spec runtime.RawExtension `json:"spec"`
}

// Placement is v1 of the KEP's placement concept: label-selector only.
type Placement struct {
	// ClusterSelector selects ClusterProfile objects by label. An empty
	// selector matches no clusters (explicit opt-in, no accidental
	// fleet-wide blast).
	ClusterSelector metav1.LabelSelector `json:"clusterSelector"`

	// Tolerance controls how partial failure folds into the rolled-up
	// Ready condition.
	// +optional
	Tolerance *Tolerance `json:"tolerance,omitempty"`
}

// Tolerance mirrors the KEP's spec.placement.tolerance.
type Tolerance struct {
	// MinReadyClusters is the number of placed clusters that must report
	// ready for the FleetGenAIService's Ready condition to be True.
	// Unset means "all placed clusters".
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinReadyClusters *int32 `json:"minReadyClusters,omitempty"`
}

// FleetGenAIServiceStatus is the fleet view aggregated back onto the hub
// object.
type FleetGenAIServiceStatus struct {
	// ObservedGeneration is the spec generation last acted upon.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Clusters holds one entry per member the object is (or was last)
	// placed on. This doubles as the PoC's applied-manifest inventory:
	// members listed here are exactly those the controller must clean up
	// on unplacement or deletion (see docs/KEP-GAP.md).
	// +optional
	Clusters []ClusterStatus `json:"clusters,omitempty"`

	// Summary counts placements for quick fleet-level reading.
	// +optional
	Summary Summary `json:"summary,omitempty"`

	// Conditions holds the rolled-up conditions; Ready is computed from
	// per-member readiness and spec.placement.tolerance.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterStatus is the per-member slice of the fleet view.
type ClusterStatus struct {
	// Name is the ClusterProfile name (unique within the fleet namespace).
	Name string `json:"name"`

	// Ready reports whether the placed GenAIService is ready on this
	// member (kro instance state ACTIVE or a true Ready condition).
	Ready bool `json:"ready"`

	// Message carries a human-oriented note (e.g. why not ready, or that
	// the member is unreachable).
	// +optional
	Message string `json:"message,omitempty"`
}

// Summary counts placements.
type Summary struct {
	// Placed is the number of members the object is currently applied to.
	Placed int32 `json:"placed"`
	// Ready is the number of those members reporting ready.
	Ready int32 `json:"ready"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fgs
// +kubebuilder:printcolumn:name="Placed",type=integer,JSONPath=`.status.summary.placed`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.summary.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FleetGenAIService is one GenAIService authored once on the hub and placed
// onto every member cluster matching the placement selector.
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
