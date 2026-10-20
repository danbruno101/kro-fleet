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

package v1alpha1

import (
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Definition distribution adopts the distribution half of upstream kro's
// KREP-013 (kubernetes-sigs/kro#1223): a platform team authors a
// ResourceGraphDefinition ONCE on the hub and the fleet controller hands a
// plain copy to every member, where stock kro accepts it, generates the CRD
// and expands instances locally. The hub never evaluates a graph. The
// wrapper below is a PoC envelope around kro's own spec type — it exists
// only because the hub runs no kro; if KREP-013 ships hub-side RGDs it
// collapses into a label (`kro.run/distribute`) on a plain RGD. See
// docs/KEP-GAP.md.

const (
	// SpecHashLabel on a distributed ResourceGraphDefinition carries the hash
	// of the hub definition's spec it was rendered from, so the hub can read
	// back WHICH revision a member holds instead of assuming it.
	SpecHashLabel = "fleet.kro.run/spec-hash"

	// Condition types on a FleetResourceGraphDefinition.

	// ConditionDistributed is True when every targeted, healthy member holds
	// the current spec hash (status.members[].applied). It is False while an
	// engaged member still carries an older hash or its apply failed, when
	// distribution is disabled, or when the selector matches no member —
	// never vacuously True. A member that is not engaged (unhealthy per its
	// cluster manager, or without credentials) is neither written to nor
	// counted against it; it is named in the message.
	ConditionDistributed = "Distributed"
	// ConditionAccepted is True when every targeted member's own kro reports
	// the current revision Active (status.state Active, Ready=True). It is
	// False with the first failing member's name and kro-reported reason:
	// member-side validation is what gives per-cluster graph acceptance.
	ConditionAccepted = "Accepted"
	// ConditionDeleting reports the deletion guard. True (Removing) while
	// the definition is being removed from one or more members with nothing
	// blocking; False (BlockedByInstances) when removal is refused on members
	// that still host hub-placed instances of this kind — the message names
	// the blocking members and their instance counts; False
	// (NoRemovalPending) otherwise. On delete the finalizer holds the
	// wrapper in Terminating until every member is released.
	ConditionDeleting = "Deleting"

	// Reasons on the conditions above.
	ReasonDistributed         = "Distributed"
	ReasonDistributing        = "Distributing"
	ReasonDistributionDisabled = "DistributionDisabled"
	ReasonNoMatchingMembers   = "NoMatchingMembers"
	ReasonAccepted            = "Accepted"
	ReasonNotAccepted         = "NotAccepted"
	ReasonRemoving            = "Removing"
	ReasonBlockedByInstances  = "BlockedByInstances"
	ReasonNoRemovalPending    = "NoRemovalPending"

	// Reasons on status.members[] that are NOT the member's own: set only
	// when the member could not be observed at all.

	// ReasonMemberNotEngaged: the member is registered but not reachable
	// (unhealthy per its cluster manager, or no credentials). State and
	// Ready are left empty — unknown, not guessed.
	ReasonMemberNotEngaged = "MemberNotEngaged"
	// ReasonApplyFailed: the server-side apply on the member failed; the
	// message is the API error.
	ReasonApplyFailed = "ApplyFailed"
	// ReasonNotDistributed: the member is targeted but the controller has
	// not written to it yet, or it is pending removal.
	ReasonNotDistributed = "NotDistributed"

	// ReasonRGDNotReady on a FleetGenAIService's Placed condition and on a
	// status.clusters[] entry: the member is decided but the definition for
	// the instance's kind does not report it Active there, so nothing is
	// placed on it (and an earlier copy, if any, is held as is).
	ReasonRGDNotReady = "RGDNotReady"
)

// FleetResourceGraphDefinitionSpec is one kro ResourceGraphDefinition plus
// where it goes.
type FleetResourceGraphDefinitionSpec struct {
	// ResourceGraphDefinition is kro's own ResourceGraphDefinitionSpec,
	// embedded verbatim (imported from kro at the pinned version, never
	// copied). It is what each member receives, byte for byte: nothing
	// fleet-specific is ever added inside it.
	ResourceGraphDefinition krov1alpha1.ResourceGraphDefinitionSpec `json:"resourceGraphDefinition"`

	// Distribution says which members receive the definition.
	// +optional
	Distribution Distribution `json:"distribution,omitempty"`

	// PlacementDefaults are hard requirements a fleet instance of this kind
	// inherits when it sets none of its own (an instance's own
	// spec.placement.requirements always take precedence). Same shape as
	// the instance's requirements. Type and validation only for now: not
	// yet consumed by the instance controller (see docs/KEP-GAP.md).
	// +optional
	PlacementDefaults *Requirements `json:"placementDefaults,omitempty"`
}

// Distribution mirrors KREP-013's kro.run/distribute label, plus a member
// selector KREP-013 leaves as future work.
type Distribution struct {
	// Enabled distributes the definition to the selected members. Flipping
	// it to false is an unplacement from every member, subject to the
	// deletion guard.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// MemberSelector restricts distribution to members whose ClusterProfile
	// status.properties match — the same vocabulary as an instance's
	// spec.placement.requirements. Absent means every registered member.
	// A member that stops matching is unplaced, subject to the deletion
	// guard.
	// +optional
	MemberSelector *Requirements `json:"memberSelector,omitempty"`
}

// IsEnabled reads distribution.enabled with its default.
func (d Distribution) IsEnabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// FleetResourceGraphDefinitionStatus is the fleet view of one definition.
type FleetResourceGraphDefinitionStatus struct {
	// ObservedGeneration is the spec generation last acted upon.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SpecHash is the hash of spec.resourceGraphDefinition, the revision
	// members are expected to hold (compare status.members[].appliedHash).
	// +optional
	SpecHash string `json:"specHash,omitempty"`

	// Members holds one entry per targeted member (plus members pending
	// removal). Everything in an entry except applied / appliedHash / uid
	// is the member's own kro-reported state, read off the distributed
	// ResourceGraphDefinition — never inferred on the hub.
	// +optional
	Members []MemberDefinitionStatus `json:"members,omitempty"`

	// Summary counts members for quick reading.
	// +optional
	Summary DefinitionSummary `json:"summary,omitempty"`

	// Conditions: Distributed, Accepted, Deleting (see the condition
	// constants for what each means).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MemberDefinitionStatus is one member's view of the definition.
type MemberDefinitionStatus struct {
	// Name is the member ClusterProfile name.
	Name string `json:"name"`

	// AppliedHash is the spec hash read back from the member's copy
	// (its fleet.kro.run/spec-hash label), or empty when the member holds
	// no copy or could not be read.
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`

	// Applied is true when appliedHash equals status.specHash: the member
	// holds the current revision (as last observed, for a member that is
	// not engaged right now).
	Applied bool `json:"applied"`

	// UID of the member's copy, the delete precondition so a same-named
	// definition someone else created there is never removed.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// State is the member kro's status.state on the copy (Active /
	// Inactive), verbatim. Empty when unknown.
	// +optional
	State string `json:"state,omitempty"`

	// Ready is the status of the member kro's Ready condition on the copy
	// (True / False / Unknown), verbatim. Empty when unknown.
	// +optional
	Ready string `json:"ready,omitempty"`

	// Reason is the member kro's reason when the copy is not Active (its
	// Ready condition, else the first condition that is not True), or one
	// of the hub-side reasons when the member could not be observed
	// (MemberNotEngaged, ApplyFailed, NotDistributed).
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is the member kro's message for Reason, verbatim.
	// +optional
	Message string `json:"message,omitempty"`

	// KroVersion is the kro version the member runs, when obtainable
	// (the app.kubernetes.io/version label on its kro Deployment).
	// +optional
	KroVersion string `json:"kroVersion,omitempty"`

	// LastTransitionTime is when state or ready last changed for this
	// member, as observed by the hub.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// Accepted reports whether the member holds the current revision and its
// own kro says it is serving: applied, state Active, Ready True.
func (m MemberDefinitionStatus) Accepted() bool {
	return m.Applied && m.State == string(krov1alpha1.ResourceGraphDefinitionStateActive) && m.Ready == string(metav1.ConditionTrue)
}

// DefinitionSummary counts members.
type DefinitionSummary struct {
	// Targeted is the number of members the definition should be on.
	Targeted int32 `json:"targeted"`
	// Distributed is the number of those holding the current revision.
	Distributed int32 `json:"distributed"`
	// Accepted is the number of those whose kro reports it Active.
	Accepted int32 `json:"accepted"`
	// Members is "accepted/targeted", for kubectl get.
	// +optional
	Members string `json:"members,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=frgd
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.resourceGraphDefinition.schema.kind`
// +kubebuilder:printcolumn:name="Distributed",type=string,JSONPath=`.status.conditions[?(@.type=="Distributed")].status`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Members",type=string,JSONPath=`.status.summary.members`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FleetResourceGraphDefinition is one kro ResourceGraphDefinition authored
// once on the hub and distributed, as a plain ResourceGraphDefinition named
// after this object, to the members its distribution selects. Stock kro on
// each member accepts it (or not: per-cluster graph acceptance, by
// construction) and expands instances the fleet controller places there.
type FleetResourceGraphDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec FleetResourceGraphDefinitionSpec `json:"spec"`
	// +optional
	Status FleetResourceGraphDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetResourceGraphDefinitionList contains a list of FleetResourceGraphDefinition.
type FleetResourceGraphDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetResourceGraphDefinition `json:"items"`
}
