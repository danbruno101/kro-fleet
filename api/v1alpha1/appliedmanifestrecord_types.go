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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// InstanceLabel on an AppliedManifestRecord names the FleetGenAIService
	// it belongs to (same namespace).
	InstanceLabel = "fleet.kro.run/instance"
	// ClusterLabel on an AppliedManifestRecord names the member ClusterProfile.
	ClusterLabel = "fleet.kro.run/cluster"

	// RecordConditionApplied is True when every intended manifest is confirmed
	// applied on the member.
	RecordConditionApplied = "Applied"
	// RecordConditionOrphaned is True when the member was deregistered (its
	// ClusterProfile deleted) while the record still tracked objects there.
	// The record is kept as the ledger of what may be orphaned; it is
	// reconciled, and cleaned up, if the member registers again.
	RecordConditionOrphaned = "Orphaned"
)

// AppliedManifestRecordSpec is the intent half of the record.
type AppliedManifestRecordSpec struct {
	// InstanceRef names the FleetGenAIService this record belongs to (same
	// namespace as the record).
	InstanceRef LocalObjectReference `json:"instanceRef"`

	// Cluster is the member the manifests are applied to, as the
	// ClusterProfile's namespace/name on the hub.
	Cluster ClusterReference `json:"cluster"`

	// Manifests is the set the controller intends to apply on the member,
	// keyed by GVK/namespace/name. It is written BEFORE the member is
	// touched, so a crash between an apply and the status write can never
	// leave an object the inventory does not know about.
	// +optional
	Manifests []ManifestIdentifier `json:"manifests,omitempty"`
}

// LocalObjectReference names an object in the same namespace.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// ClusterReference identifies a member ClusterProfile on the hub.
type ClusterReference struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// ManifestIdentifier is the inventory key: one object on one member.
type ManifestIdentifier struct {
	// +optional
	Group   string `json:"group,omitempty"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// AppliedManifest is a manifest confirmed on the member.
type AppliedManifest struct {
	ManifestIdentifier `json:",inline"`

	// UID of the object on the member, used as a delete precondition so a
	// same-named object created by someone else is never removed.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// Parameters are the per-member values this copy was rendered with
	// (e.g. replicas), for inventory readability.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// AppliedManifestRecordStatus is the observed half of the record.
type AppliedManifestRecordStatus struct {
	// Applied lists the manifests confirmed on the member. Entries that are
	// no longer in spec.manifests are pruned from the member, then dropped.
	// +optional
	Applied []AppliedManifest `json:"applied,omitempty"`

	// Conditions: Applied, Orphaned.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=amr
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster.name`
// +kubebuilder:printcolumn:name="Intended",type=integer,JSONPath=`.spec.manifests[*]`,priority=1
// +kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type=="Applied")].status`
// +kubebuilder:printcolumn:name="Orphaned",type=string,JSONPath=`.status.conditions[?(@.type=="Orphaned")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AppliedManifestRecord is the hub-side applied-manifest inventory for one
// (FleetGenAIService, member) pair — conceptually OCM's AppliedManifestWork.
// Ownership cannot cross a cluster boundary, so this record is what makes
// unplacement, finalizer-driven teardown and drift pruning exact: the
// controller deletes precisely the objects recorded here, and nothing else.
// There is exactly one record per placement; it has no ownerReference on
// purpose — its lifecycle is explicit, and a record whose member vanished
// before cleanup survives as the ledger of possibly orphaned objects.
type AppliedManifestRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AppliedManifestRecordSpec `json:"spec"`
	// +optional
	Status AppliedManifestRecordStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppliedManifestRecordList contains a list of AppliedManifestRecord.
type AppliedManifestRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppliedManifestRecord `json:"items"`
}
