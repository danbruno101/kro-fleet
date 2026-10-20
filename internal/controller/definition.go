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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// Definition distribution, the pure half: what a member receives, what the
// hub reads back from it, which members are targeted, and how per-member
// facts fold into the wrapper's conditions. No I/O here.

// ResourceGraphDefinitionGVK is the member-side object a
// FleetResourceGraphDefinition distributes: stock kro's own API, unchanged.
var ResourceGraphDefinitionGVK = schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "ResourceGraphDefinition"}

// DefinitionSpecHash hashes spec.resourceGraphDefinition exactly as a
// member receives it (DefinitionSpecMap). Members carry it as a label, so
// "which revision does this member hold" is read back, never assumed.
// Short enough for a label value.
func DefinitionSpecHash(spec *krov1alpha1.ResourceGraphDefinitionSpec) (string, error) {
	m, err := DefinitionSpecMap(spec)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("failed to hash resourceGraphDefinition: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32], nil
}

// DefinitionSpecMap turns kro's typed spec back into the plain object a
// user would have applied. kro's RawExtension fields (schema.types, a
// resource's template or externalRef) marshal as JSON null when unset,
// which kro's own CRD rejects; those keys are what the author never wrote,
// so they are dropped — nothing else is touched.
func DefinitionSpecMap(spec *krov1alpha1.ResourceGraphDefinitionSpec) (map[string]interface{}, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to render resourceGraphDefinition: %w", err)
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("resourceGraphDefinition is not an object: %w", err)
	}
	dropNulls(m)
	return m, nil
}

// dropNulls removes JSON null values from a decoded object, recursively.
func dropNulls(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, e := range t {
			if e == nil {
				delete(t, k)
				continue
			}
			dropNulls(e)
		}
	case []interface{}:
		for _, e := range t {
			dropNulls(e)
		}
	}
}

// RenderDefinition builds the plain ResourceGraphDefinition a member
// receives: named after the wrapper, spec copied verbatim, and only the
// ownership label the controller already uses for pruning plus the spec
// hash on the metadata. Nothing fleet-specific inside the spec — a member
// gets exactly what a user would have applied by hand.
func RenderDefinition(frgd *fleetv1alpha1.FleetResourceGraphDefinition, hash string) (*unstructured.Unstructured, error) {
	spec, err := DefinitionSpecMap(&frgd.Spec.ResourceGraphDefinition)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ResourceGraphDefinitionGVK)
	obj.SetName(frgd.Name)
	obj.SetLabels(map[string]string{PlacedByLabel: frgd.Name, fleetv1alpha1.SpecHashLabel: hash})
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		return nil, err
	}
	return obj, nil
}

// DefinitionKind returns the instance GVK a definition serves (group
// defaults to kro.run, as kro does).
func DefinitionKind(frgd *fleetv1alpha1.FleetResourceGraphDefinition) schema.GroupVersionKind {
	s := frgd.Spec.ResourceGraphDefinition.Schema
	if s == nil {
		return schema.GroupVersionKind{}
	}
	group := s.Group
	if group == "" {
		group = "kro.run"
	}
	return schema.GroupVersionKind{Group: group, Version: s.APIVersion, Kind: s.Kind}
}

// ObservedDefinition is what the hub reads off a member's copy. Every field
// but UID/Hash/Generation is the member kro's own report.
type ObservedDefinition struct {
	UID        types.UID
	Hash       string
	Generation int64
	State      string
	Ready      string
	Reason     string
	Message    string
	// ReadyObservedGeneration is the generation the member kro's Ready
	// condition was computed for (0 when it does not say).
	ReadyObservedGeneration int64
}

// ObserveDefinition reads a member's ResourceGraphDefinition as kro reports
// it: status.state verbatim, the Ready condition's status/reason/message
// verbatim; when Ready carries no reason, the first condition that is not
// True (e.g. GraphAccepted=False/InvalidResourceGraph) supplies reason and
// message, so a rejected graph surfaces the member's own words.
func ObserveDefinition(obj *unstructured.Unstructured) ObservedDefinition {
	o := ObservedDefinition{}
	if obj == nil {
		return o
	}
	o.UID = obj.GetUID()
	o.Hash = obj.GetLabels()[fleetv1alpha1.SpecHashLabel]
	o.Generation = obj.GetGeneration()
	o.State, _, _ = unstructured.NestedString(obj.Object, "status", "state")
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var fallback map[string]interface{}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		if t == string(krov1alpha1.InstanceConditionTypeReady) {
			o.Ready = status
			o.Reason, _ = cond["reason"].(string)
			o.Message, _ = cond["message"].(string)
			switch g := cond["observedGeneration"].(type) {
			case int64:
				o.ReadyObservedGeneration = g
			case float64:
				o.ReadyObservedGeneration = int64(g)
			}
			continue
		}
		if fallback == nil && status != string(metav1.ConditionTrue) {
			fallback = cond
		}
	}
	if o.Reason == "" && fallback != nil {
		o.Reason, _ = fallback["reason"].(string)
		o.Message, _ = fallback["message"].(string)
		if t, _ := fallback["type"].(string); t != "" && o.Reason != "" {
			o.Reason = t + "/" + o.Reason
		}
	}
	return o
}

// Fresh reports whether the member kro has evaluated the copy's current
// generation (or does not say). A stale Ready after a spec update must not
// count as acceptance of the new revision.
func (o ObservedDefinition) Fresh() bool {
	return o.ReadyObservedGeneration == 0 || o.ReadyObservedGeneration >= o.Generation
}

// Accepted reports whether the member kro says it serves the revision the
// copy is at: Active, Ready True, and that verdict is about the copy's own
// generation.
func (o ObservedDefinition) Accepted() bool {
	return o.Fresh() && o.State == string(krov1alpha1.ResourceGraphDefinitionStateActive) && o.Ready == string(metav1.ConditionTrue)
}

// DefinitionTargets resolves which registered members a definition goes to:
// none when distribution is disabled, else every member whose ClusterProfile
// properties satisfy memberSelector (absent = all). Sorted.
func DefinitionTargets(frgd *fleetv1alpha1.FleetResourceGraphDefinition, profiles []clusterinventoryv1alpha1.ClusterProfile) []string {
	if !frgd.Spec.Distribution.IsEnabled() {
		return nil
	}
	var out []string
	for i := range profiles {
		if checkRequirements(&profiles[i], frgd.Spec.Distribution.MemberSelector) != "" {
			continue
		}
		out = append(out, profiles[i].Name)
	}
	sort.Strings(out)
	return out
}

// BlockedMember is one member whose definition cannot be removed yet, with
// the instances of its kind that block it split by who created them. The
// guard reflects the member's reality, not the hub's ledger: stock kro
// accepts an instance created directly on the member, and removing the
// definition would delete the generated CRD and that instance with it.
type BlockedMember struct {
	Name string
	// HubPlaced is the number of instances the hub placed there: the
	// AppliedManifestRecords for the member that track the kind.
	HubPlaced int
	// MemberLocal is the number of instances found on the member that no
	// record tracks — created there directly, invisible to the hub's ledger.
	MemberLocal int
}

// String renders the member for the Deleting condition message, e.g.
// "member-2 (1 hub-placed, 2 member-local instance(s))".
func (b BlockedMember) String() string {
	return fmt.Sprintf("%s (%d hub-placed, %d member-local instance(s))", b.Name, b.HubPlaced, b.MemberLocal)
}

// MemberLocalInstances counts the instances on a member that no hub record
// tracks: everything listed there of the definition's kind whose
// namespace/name is not among the tracked manifests of that kind.
func MemberLocalInstances(found *unstructured.UnstructuredList, tracked map[types.NamespacedName]bool) int {
	if found == nil {
		return 0
	}
	n := 0
	for i := range found.Items {
		key := types.NamespacedName{Namespace: found.Items[i].GetNamespace(), Name: found.Items[i].GetName()}
		if !tracked[key] {
			n++
		}
	}
	return n
}

// DefinitionRollup is what the wrapper-level conditions are computed from.
type DefinitionRollup struct {
	Enabled  bool
	Targets  []string
	Members  []fleetv1alpha1.MemberDefinitionStatus
	Blocked  []BlockedMember
	Removing []string // members being removed, nothing blocking
}

// RollupDefinition computes Distributed, Accepted and Deleting plus the
// summary (see the condition docs on the API type for their meaning).
func RollupDefinition(in DefinitionRollup) (distributed, accepted, deleting metav1.Condition, summary fleetv1alpha1.DefinitionSummary) {
	byName := map[string]fleetv1alpha1.MemberDefinitionStatus{}
	for _, m := range in.Members {
		byName[m.Name] = m
	}
	summary.Targeted = int32(len(in.Targets))
	var notApplied, notEngaged, notAccepted []string
	for _, name := range in.Targets {
		m := byName[name]
		switch {
		case m.Applied:
			summary.Distributed++
		case m.Reason == fleetv1alpha1.ReasonMemberNotEngaged:
			// Distributed is about the members that CAN be written to; an
			// unhealthy member is neither written to nor counted against it.
			notEngaged = append(notEngaged, name)
		default:
			notApplied = append(notApplied, name)
		}
		if m.Accepted() {
			summary.Accepted++
		} else {
			reason := m.Reason
			if reason == "" {
				reason = fleetv1alpha1.ReasonNotDistributed
			}
			msg := fmt.Sprintf("%s: %s", name, reason)
			if m.Message != "" {
				msg += ": " + m.Message
			}
			notAccepted = append(notAccepted, msg)
		}
	}
	summary.Members = fmt.Sprintf("%d/%d", summary.Accepted, summary.Targeted)

	distributed = metav1.Condition{Type: fleetv1alpha1.ConditionDistributed, Status: metav1.ConditionFalse}
	accepted = metav1.Condition{Type: fleetv1alpha1.ConditionAccepted, Status: metav1.ConditionFalse}
	switch {
	case !in.Enabled:
		distributed.Reason, distributed.Message = fleetv1alpha1.ReasonDistributionDisabled, "distribution.enabled is false; the definition is on no member"
		accepted.Reason, accepted.Message = fleetv1alpha1.ReasonDistributionDisabled, distributed.Message
	case len(in.Targets) == 0:
		distributed.Reason, distributed.Message = fleetv1alpha1.ReasonNoMatchingMembers, "memberSelector matches no registered member"
		accepted.Reason, accepted.Message = fleetv1alpha1.ReasonNoMatchingMembers, distributed.Message
	default:
		if len(notApplied) == 0 && summary.Distributed > 0 {
			distributed.Status, distributed.Reason = metav1.ConditionTrue, fleetv1alpha1.ReasonDistributed
			distributed.Message = fmt.Sprintf("current revision on %d/%d targeted member(s)", summary.Distributed, summary.Targeted)
		} else {
			distributed.Reason = fleetv1alpha1.ReasonDistributing
			distributed.Message = fmt.Sprintf("current revision on %d/%d targeted member(s); pending on %s", summary.Distributed, summary.Targeted, strings.Join(notApplied, ", "))
		}
		if len(notEngaged) > 0 {
			distributed.Message += "; not engaged: " + strings.Join(notEngaged, ", ")
		}
		if len(notAccepted) == 0 {
			accepted.Status, accepted.Reason = metav1.ConditionTrue, fleetv1alpha1.ReasonAccepted
			accepted.Message = fmt.Sprintf("Active on %d/%d targeted member(s)", summary.Accepted, summary.Targeted)
		} else {
			accepted.Reason = fleetv1alpha1.ReasonNotAccepted
			accepted.Message = fmt.Sprintf("Active on %d/%d targeted member(s); %s", summary.Accepted, summary.Targeted, notAccepted[0])
		}
	}

	deleting = metav1.Condition{Type: fleetv1alpha1.ConditionDeleting, Status: metav1.ConditionFalse, Reason: fleetv1alpha1.ReasonNoRemovalPending, Message: "no member is pending removal"}
	switch {
	case len(in.Blocked) > 0:
		parts := make([]string, 0, len(in.Blocked))
		for _, b := range in.Blocked {
			parts = append(parts, b.String())
		}
		deleting.Reason = fleetv1alpha1.ReasonBlockedByInstances
		deleting.Message = "removal refused while instances of this kind exist on: " + strings.Join(parts, ", ")
	case len(in.Removing) > 0:
		deleting.Status, deleting.Reason = metav1.ConditionTrue, fleetv1alpha1.ReasonRemoving
		deleting.Message = "removing from " + strings.Join(in.Removing, ", ")
	}
	return distributed, accepted, deleting, summary
}

// DefinitionGate decides whether a fleet instance may be placed on a member.
// Its question is "can this member's kro serve the kind", not "is this
// member on the latest revision": a member passes when the definition for
// the kind exists on the hub, targets the member, and the member's copy is
// Serving (reported Active at SOME revision and has not rejected the
// current one). A member serving an older revision while its kro evaluates
// the new one passes with a note — "serving revision <old>; revision
// <new> pending" — for the instance's status.clusters[] entry, so the
// rollout is visible without flapping Placed on every definition edit. A
// member that holds no copy, has never reported Active, explicitly
// rejected the current revision, or is pending removal is blocked, with
// why in the definition's (i.e. the member kro's) own words.
func DefinitionGate(def *fleetv1alpha1.FleetResourceGraphDefinition, gvk schema.GroupVersionKind, member string) (ok bool, note string) {
	if def == nil {
		return false, fmt.Sprintf("no FleetResourceGraphDefinition for kind %s on the hub", gvk.Kind)
	}
	for _, m := range def.Status.Members {
		if m.Name != member {
			continue
		}
		if m.Reason == fleetv1alpha1.ReasonNotDistributed {
			// Not written yet, or on its way out: not a serving member.
			return false, fmt.Sprintf("definition %s is not distributed to this member: %s", def.Name, m.Message)
		}
		if m.Accepted() {
			return true, ""
		}
		if m.Serving() {
			return true, fmt.Sprintf("serving revision %s of definition %s; revision %s pending", m.AcceptedHash, def.Name, def.Status.SpecHash)
		}
		why := fmt.Sprintf("definition %s is not Active on this member", def.Name)
		switch {
		case m.Reason != "":
			why += ": " + m.Reason
			if m.Message != "" {
				why += ": " + m.Message
			}
		case m.AppliedHash == "":
			why += ": the member holds no copy"
		case m.AcceptedHash == "":
			why += ": the member's kro has not reported it Active yet"
		case !m.Applied:
			why += fmt.Sprintf(": holds revision %q, hub is at %q", m.AppliedHash, def.Status.SpecHash)
		case m.State != "":
			why += ": state " + m.State
		}
		if m.Rejected() && m.AcceptedHash != "" {
			why += fmt.Sprintf("; last accepted revision %s", m.AcceptedHash)
		}
		return false, why
	}
	return false, fmt.Sprintf("definition %s is not distributed to this member (distribution disabled or memberSelector excludes it)", def.Name)
}

// manifestsOfKind counts how many of a record's tracked manifests are
// instances of the given kind.
func manifestsOfKind(rec *fleetv1alpha1.AppliedManifestRecord, gvk schema.GroupVersionKind) int {
	n := 0
	for _, m := range trackedManifests(rec) {
		if m.Kind == gvk.Kind && m.Group == gvk.Group {
			n++
		}
	}
	return n
}
