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
	"strings"
	"testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

func definition(name, kind string) *fleetv1alpha1.FleetResourceGraphDefinition {
	return &fleetv1alpha1.FleetResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: fleetv1alpha1.FleetResourceGraphDefinitionSpec{
			ResourceGraphDefinition: krov1alpha1.ResourceGraphDefinitionSpec{
				Schema: &krov1alpha1.Schema{
					Kind: kind, APIVersion: "v1alpha1", Group: "kro.run",
					Spec: runtime.RawExtension{Raw: []byte(`{"name":"string"}`)},
				},
				Resources: []*krov1alpha1.Resource{{
					ID:       "cm",
					Template: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"${schema.spec.name}"}}`)},
				}},
			},
		},
	}
}

func TestDefinitionSpecHashIsStableAndSensitive(t *testing.T) {
	a := definition("echo", "Echo")
	b := definition("echo", "Echo")
	ha, err := DefinitionSpecHash(&a.Spec.ResourceGraphDefinition)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := DefinitionSpecHash(&b.Spec.ResourceGraphDefinition)
	if ha != hb {
		t.Fatalf("same spec, different hashes: %s vs %s", ha, hb)
	}
	if len(ha) != 32 {
		t.Fatalf("hash must fit a label value, got %d chars", len(ha))
	}
	b.Spec.ResourceGraphDefinition.Resources[0].Template.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x"}}`)
	if hb2, _ := DefinitionSpecHash(&b.Spec.ResourceGraphDefinition); hb2 == ha {
		t.Fatal("a changed template must change the hash")
	}
	// Distribution settings are NOT part of what members receive.
	b = definition("echo", "Echo")
	b.Spec.Distribution.Enabled = ptr.To(false)
	if hb3, _ := DefinitionSpecHash(&b.Spec.ResourceGraphDefinition); hb3 != ha {
		t.Fatal("distribution settings must not change the spec hash")
	}
}

func TestRenderDefinitionIsVerbatimAndPlain(t *testing.T) {
	d := definition("echo.kro.run", "Echo")
	obj, err := RenderDefinition(d, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetKind() != "ResourceGraphDefinition" || obj.GetAPIVersion() != "kro.run/v1alpha1" {
		t.Fatalf("wrong GVK: %s", obj.GroupVersionKind())
	}
	if obj.GetName() != "echo.kro.run" || obj.GetNamespace() != "" {
		t.Fatalf("member copy must be cluster-scoped and named after the wrapper, got %s/%s", obj.GetNamespace(), obj.GetName())
	}
	if obj.GetLabels()[PlacedByLabel] != "echo.kro.run" || obj.GetLabels()[fleetv1alpha1.SpecHashLabel] != "abc" {
		t.Fatalf("missing ownership / hash labels: %v", obj.GetLabels())
	}
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "schema", "kind")
	if kind != "Echo" {
		t.Fatalf("spec not copied: kind=%q", kind)
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	for k := range spec {
		if strings.Contains(k, "fleet") || k == "distribution" || k == "placementDefaults" {
			t.Fatalf("fleet-specific key %q leaked into the member spec", k)
		}
	}
	tmpl, _, _ := unstructured.NestedMap(obj.Object, "spec", "resources")
	_ = tmpl
	res, _, _ := unstructured.NestedSlice(obj.Object, "spec", "resources")
	if len(res) != 1 {
		t.Fatalf("resources not copied verbatim: %v", res)
	}
	// kro's unset RawExtension fields must not arrive as JSON null: a member
	// kro rejects `types: null` / `externalRef: null`.
	if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "schema", "types"); found {
		t.Fatal("unset schema.types must be omitted, not null")
	}
	if r0, ok := res[0].(map[string]interface{}); !ok || r0["externalRef"] != nil {
		t.Fatalf("unset externalRef must be omitted, got %v", res[0])
	} else if _, has := r0["externalRef"]; has {
		t.Fatal("unset externalRef key must be absent")
	}
}

func TestObserveDefinition(t *testing.T) {
	mk := func(state string, conds ...map[string]interface{}) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "echo", "uid": "u1", "generation": int64(2),
				"labels": map[string]interface{}{fleetv1alpha1.SpecHashLabel: "h1"}},
			"status": map[string]interface{}{"state": state},
		}}
		var cs []interface{}
		for _, c := range conds {
			cs = append(cs, c)
		}
		if cs != nil {
			_ = unstructured.SetNestedSlice(o.Object, cs, "status", "conditions")
		}
		return o
	}
	t.Run("active with Ready", func(t *testing.T) {
		o := ObserveDefinition(mk("Active", map[string]interface{}{"type": "Ready", "status": "True", "reason": "Ready", "observedGeneration": int64(2)}))
		if o.State != "Active" || o.Ready != "True" || o.Hash != "h1" || o.UID != "u1" || !o.Fresh() {
			t.Fatalf("unexpected observation: %+v", o)
		}
	})
	t.Run("rejected graph surfaces the member's own reason", func(t *testing.T) {
		o := ObserveDefinition(mk("Inactive",
			map[string]interface{}{"type": "GraphAccepted", "status": "False", "reason": "InvalidResourceGraph", "message": "no matches for kind SkewMarker"},
			map[string]interface{}{"type": "Ready", "status": "False"},
		))
		if o.State != "Inactive" || o.Ready != "False" {
			t.Fatalf("unexpected: %+v", o)
		}
		if o.Reason != "GraphAccepted/InvalidResourceGraph" || o.Message != "no matches for kind SkewMarker" {
			t.Fatalf("reason/message not taken verbatim from the failing condition: %+v", o)
		}
	})
	t.Run("stale Ready is not fresh", func(t *testing.T) {
		o := ObserveDefinition(mk("Active", map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": int64(1)}))
		if o.Fresh() {
			t.Fatal("Ready computed for generation 1 must not count for generation 2")
		}
	})
	t.Run("nil", func(t *testing.T) {
		if o := ObserveDefinition(nil); o.State != "" || o.Ready != "" {
			t.Fatalf("nil object must observe nothing: %+v", o)
		}
	})
}

func TestDefinitionTargets(t *testing.T) {
	withProps := func(name string, props map[string]string) clusterinventoryv1alpha1.ClusterProfile {
		p := profile(name, nil)
		for k, v := range props {
			p.Status.Properties = append(p.Status.Properties, clusterinventoryv1alpha1.Property{Name: k, Value: v})
		}
		return p
	}
	profiles := []clusterinventoryv1alpha1.ClusterProfile{
		withProps("m3", map[string]string{"cloud.example.com": "eks"}),
		withProps("m1", map[string]string{"cloud.example.com": "gke"}),
		withProps("m2", map[string]string{"cloud.example.com": "aks"}),
	}
	d := definition("echo", "Echo")
	if got := DefinitionTargets(d, profiles); strings.Join(got, ",") != "m1,m2,m3" {
		t.Fatalf("no selector must target every member, sorted; got %v", got)
	}
	d.Spec.Distribution.MemberSelector = &fleetv1alpha1.Requirements{MatchProperties: map[string]string{"cloud.example.com": "aks"}}
	if got := DefinitionTargets(d, profiles); strings.Join(got, ",") != "m2" {
		t.Fatalf("selector must narrow to matching members; got %v", got)
	}
	d.Spec.Distribution.Enabled = ptr.To(false)
	if got := DefinitionTargets(d, profiles); got != nil {
		t.Fatalf("enabled=false must target nobody; got %v", got)
	}
}

func member(name string, applied bool, state, ready, reason string) fleetv1alpha1.MemberDefinitionStatus {
	return fleetv1alpha1.MemberDefinitionStatus{Name: name, Applied: applied, State: state, Ready: ready, Reason: reason}
}

func TestRollupDefinition(t *testing.T) {
	active := func(n string) fleetv1alpha1.MemberDefinitionStatus { return member(n, true, "Active", "True", "") }

	t.Run("all accepted", func(t *testing.T) {
		d, a, del, s := RollupDefinition(DefinitionRollup{Enabled: true, Targets: []string{"m1", "m2"}, Members: []fleetv1alpha1.MemberDefinitionStatus{active("m1"), active("m2")}})
		if d.Status != metav1.ConditionTrue || a.Status != metav1.ConditionTrue || del.Status != metav1.ConditionFalse || del.Reason != fleetv1alpha1.ReasonNoRemovalPending {
			t.Fatalf("unexpected: %+v %+v %+v", d, a, del)
		}
		if s.Members != "2/2" || s.Accepted != 2 || s.Distributed != 2 || s.Targeted != 2 {
			t.Fatalf("summary: %+v", s)
		}
	})
	t.Run("one member rejects", func(t *testing.T) {
		rej := member("m3", true, "Inactive", "False", "GraphAccepted/InvalidResourceGraph")
		rej.Message = "no matches for kind SkewMarker"
		d, a, _, s := RollupDefinition(DefinitionRollup{Enabled: true, Targets: []string{"m1", "m2", "m3"}, Members: []fleetv1alpha1.MemberDefinitionStatus{active("m1"), active("m2"), rej}})
		if d.Status != metav1.ConditionTrue {
			t.Fatalf("distributed is about the hash, not acceptance: %+v", d)
		}
		if a.Status != metav1.ConditionFalse || a.Reason != fleetv1alpha1.ReasonNotAccepted || !strings.Contains(a.Message, "m3: GraphAccepted/InvalidResourceGraph: no matches for kind SkewMarker") {
			t.Fatalf("accepted must name the failing member with its own reason: %+v", a)
		}
		if s.Members != "2/3" {
			t.Fatalf("summary: %+v", s)
		}
	})
	t.Run("pending member", func(t *testing.T) {
		d, a, _, _ := RollupDefinition(DefinitionRollup{Enabled: true, Targets: []string{"m1", "m2"}, Members: []fleetv1alpha1.MemberDefinitionStatus{active("m1"), member("m2", false, "", "", fleetv1alpha1.ReasonNotDistributed)}})
		if d.Status != metav1.ConditionFalse || d.Reason != fleetv1alpha1.ReasonDistributing || !strings.Contains(d.Message, "pending on m2") {
			t.Fatalf("unexpected: %+v", d)
		}
		if a.Status != metav1.ConditionFalse {
			t.Fatalf("unexpected: %+v", a)
		}
	})
	t.Run("unhealthy member does not count against Distributed but does against Accepted", func(t *testing.T) {
		d, a, _, s := RollupDefinition(DefinitionRollup{Enabled: true, Targets: []string{"m1", "m2"}, Members: []fleetv1alpha1.MemberDefinitionStatus{active("m1"), member("m2", false, "", "", fleetv1alpha1.ReasonMemberNotEngaged)}})
		if d.Status != metav1.ConditionTrue || !strings.Contains(d.Message, "not engaged: m2") {
			t.Fatalf("unexpected: %+v", d)
		}
		if a.Status != metav1.ConditionFalse || !strings.Contains(a.Message, "m2: MemberNotEngaged") || s.Members != "1/2" {
			t.Fatalf("unexpected: %+v %+v", a, s)
		}
	})
	t.Run("nobody engaged is not distributed", func(t *testing.T) {
		d, _, _, _ := RollupDefinition(DefinitionRollup{Enabled: true, Targets: []string{"m1"}, Members: []fleetv1alpha1.MemberDefinitionStatus{member("m1", false, "", "", fleetv1alpha1.ReasonMemberNotEngaged)}})
		if d.Status == metav1.ConditionTrue {
			t.Fatalf("with no member reachable, Distributed must not be vacuously True: %+v", d)
		}
	})
	t.Run("disabled and no match are explicit, never vacuously true", func(t *testing.T) {
		d, a, _, s := RollupDefinition(DefinitionRollup{Enabled: false})
		if d.Status != metav1.ConditionFalse || d.Reason != fleetv1alpha1.ReasonDistributionDisabled || a.Reason != fleetv1alpha1.ReasonDistributionDisabled || s.Members != "0/0" {
			t.Fatalf("unexpected: %+v %+v %+v", d, a, s)
		}
		d, a, _, _ = RollupDefinition(DefinitionRollup{Enabled: true})
		if d.Reason != fleetv1alpha1.ReasonNoMatchingMembers || a.Reason != fleetv1alpha1.ReasonNoMatchingMembers {
			t.Fatalf("unexpected: %+v %+v", d, a)
		}
	})
	t.Run("deletion guard", func(t *testing.T) {
		_, _, del, _ := RollupDefinition(DefinitionRollup{Enabled: true, Blocked: []BlockedMember{{Name: "m1", HubPlaced: 2}, {Name: "m2", HubPlaced: 1, MemberLocal: 2}, {Name: "m3", MemberLocal: 1}}, Removing: []string{"m4"}})
		if del.Status != metav1.ConditionFalse || del.Reason != fleetv1alpha1.ReasonBlockedByInstances {
			t.Fatalf("blocked must win: %+v", del)
		}
		// The message tells hub-placed instances (from records) apart from
		// member-local ones (found on the member, in no record).
		want := "m1 (2 hub-placed, 0 member-local instance(s)), m2 (1 hub-placed, 2 member-local instance(s)), m3 (0 hub-placed, 1 member-local instance(s))"
		if !strings.Contains(del.Message, want) {
			t.Fatalf("blocked message must name members with both counts:\n got %q\nwant %q", del.Message, want)
		}
		_, _, del, _ = RollupDefinition(DefinitionRollup{Enabled: true, Removing: []string{"m3"}})
		if del.Status != metav1.ConditionTrue || del.Reason != fleetv1alpha1.ReasonRemoving {
			t.Fatalf("unexpected: %+v", del)
		}
	})
}

func TestDefinitionGate(t *testing.T) {
	gvk := GenAIServiceGVK
	if ok, why := DefinitionGate(nil, gvk, "m1"); ok || !strings.Contains(why, "no FleetResourceGraphDefinition for kind GenAIService") {
		t.Fatalf("no definition must close the gate: %v %q", ok, why)
	}
	d := definition("genaiservice.kro.run", "GenAIService")
	d.Status.SpecHash = "h2"
	accepted := member("m1", true, "Active", "True", "")
	accepted.AppliedHash, accepted.AcceptedHash = "h2", "h2"
	// Still on h1 (the apply of h2 has not landed), its kro serving h1.
	staleActive := fleetv1alpha1.MemberDefinitionStatus{Name: "m2", Applied: false, AppliedHash: "h1", AcceptedHash: "h1", State: "Active", Ready: "True"}
	// Holds h2, its kro evaluated it and refused; h1 was accepted before.
	rejected := member("m3", true, "Inactive", "False", "GraphAccepted/InvalidResourceGraph")
	rejected.Message = "no matches for kind SkewMarker"
	rejected.AppliedHash, rejected.AcceptedHash = "h2", "h1"
	// Holds h2, its kro has not reported anything yet: never Active.
	neverActive := fleetv1alpha1.MemberDefinitionStatus{Name: "m5", Applied: true, AppliedHash: "h2"}
	// Holds h2, its kro has not evaluated that generation yet (Unknown).
	pending := member("m6", true, "Active", "Unknown", fleetv1alpha1.ReasonReconciling)
	pending.Message = "member kro has evaluated generation 1, copy is at generation 2"
	pending.AppliedHash, pending.AcceptedHash = "h2", "h1"
	// Serving h1 but on its way out (deselected): not a placement target.
	removing := fleetv1alpha1.MemberDefinitionStatus{Name: "m7", Applied: false, AppliedHash: "h1", AcceptedHash: "h1", State: "Active", Ready: "True", Reason: fleetv1alpha1.ReasonNotDistributed, Message: "pending removal: the copy is still terminating on the member"}
	// Serving h1; the hub could not write h2 to it: pending for a reason.
	applyFailed := fleetv1alpha1.MemberDefinitionStatus{Name: "m8", Applied: false, AppliedHash: "h1", AcceptedHash: "h1", State: "Active", Ready: "True", Reason: fleetv1alpha1.ReasonApplyFailed, Message: "admission webhook denied the request"}
	d.Status.Members = []fleetv1alpha1.MemberDefinitionStatus{accepted, staleActive, rejected, neverActive, pending, removing, applyFailed}

	t.Run("accepted member passes without a note", func(t *testing.T) {
		if ok, note := DefinitionGate(d, gvk, "m1"); !ok || note != "" {
			t.Fatalf("accepted member must pass cleanly: %v %q", ok, note)
		}
	})
	t.Run("stale-Active passes: serving beats current", func(t *testing.T) {
		ok, note := DefinitionGate(d, gvk, "m2")
		if !ok || note != "serving revision h1 of definition genaiservice.kro.run; revision h2 pending" {
			t.Fatalf("a member serving an older revision must pass with the pending note: %v %q", ok, note)
		}
	})
	t.Run("explicit rejection of the new revision blocks", func(t *testing.T) {
		ok, why := DefinitionGate(d, gvk, "m3")
		if ok || !strings.Contains(why, "GraphAccepted/InvalidResourceGraph: no matches for kind SkewMarker") || !strings.Contains(why, "last accepted revision h1") {
			t.Fatalf("rejecting member must block with its own reason and the revision it last accepted: %v %q", ok, why)
		}
	})
	t.Run("never-Active blocks", func(t *testing.T) {
		if ok, why := DefinitionGate(d, gvk, "m5"); ok || !strings.Contains(why, "has not reported it Active yet") {
			t.Fatalf("a member whose kro never reported Active must block: %v %q", ok, why)
		}
	})
	t.Run("Unknown after an update passes with the pending message", func(t *testing.T) {
		ok, note := DefinitionGate(d, gvk, "m6")
		// Reconciling is the expected reason for a pending revision: the
		// note stays in its short form.
		if !ok || note != "serving revision h1 of definition genaiservice.kro.run; revision h2 pending" {
			t.Fatalf("an unevaluated new revision must not close the gate, and Reconciling adds nothing to the note: %v %q", ok, note)
		}
	})
	t.Run("pending for another reason carries the member's reason and message", func(t *testing.T) {
		ok, note := DefinitionGate(d, gvk, "m8")
		if !ok || note != "serving revision h1 of definition genaiservice.kro.run; revision h2 pending: ApplyFailed: admission webhook denied the request" {
			t.Fatalf("a serving member whose new revision failed to apply must pass, with why: %v %q", ok, note)
		}
	})
	t.Run("serving member with an empty reason keeps the short note", func(t *testing.T) {
		if ok, note := DefinitionGate(d, gvk, "m2"); !ok || note != "serving revision h1 of definition genaiservice.kro.run; revision h2 pending" {
			t.Fatalf("no reason means nothing to append: %v %q", ok, note)
		}
	})
	t.Run("pending removal blocks even while serving", func(t *testing.T) {
		if ok, why := DefinitionGate(d, gvk, "m7"); ok || !strings.Contains(why, "not distributed to this member: pending removal") {
			t.Fatalf("a member on its way out is not a target: %v %q", ok, why)
		}
	})
	t.Run("untargeted member blocks", func(t *testing.T) {
		if ok, why := DefinitionGate(d, gvk, "m4"); ok || !strings.Contains(why, "not distributed to this member") {
			t.Fatalf("untargeted member must not pass: %v %q", ok, why)
		}
	})
}

func TestMemberDefinitionStatusPredicates(t *testing.T) {
	cases := []struct {
		name                        string
		m                           fleetv1alpha1.MemberDefinitionStatus
		accepted, rejected, serving bool
	}{
		{"current and Active", fleetv1alpha1.MemberDefinitionStatus{Applied: true, AppliedHash: "h2", AcceptedHash: "h2", State: "Active", Ready: "True"}, true, false, true},
		{"older revision still Active", fleetv1alpha1.MemberDefinitionStatus{Applied: false, AppliedHash: "h1", AcceptedHash: "h1", State: "Active", Ready: "True"}, false, false, true},
		{"new revision not yet evaluated", fleetv1alpha1.MemberDefinitionStatus{Applied: true, AppliedHash: "h2", AcceptedHash: "h1", State: "Active", Ready: "Unknown"}, false, false, true},
		{"new revision rejected", fleetv1alpha1.MemberDefinitionStatus{Applied: true, AppliedHash: "h2", AcceptedHash: "h1", State: "Inactive", Ready: "False"}, false, true, false},
		{"never Active", fleetv1alpha1.MemberDefinitionStatus{Applied: true, AppliedHash: "h2"}, false, false, false},
		{"no copy", fleetv1alpha1.MemberDefinitionStatus{AcceptedHash: "h1"}, false, false, false},
		{"not engaged, served before", fleetv1alpha1.MemberDefinitionStatus{Applied: false, AppliedHash: "h1", AcceptedHash: "h1", Reason: fleetv1alpha1.ReasonMemberNotEngaged}, false, false, true},
	}
	for _, c := range cases {
		if got := c.m.Accepted(); got != c.accepted {
			t.Errorf("%s: Accepted() = %v, want %v", c.name, got, c.accepted)
		}
		if got := c.m.Rejected(); got != c.rejected {
			t.Errorf("%s: Rejected() = %v, want %v", c.name, got, c.rejected)
		}
		if got := c.m.Serving(); got != c.serving {
			t.Errorf("%s: Serving() = %v, want %v", c.name, got, c.serving)
		}
	}
}

func TestManifestsOfKindAndDefinitionKind(t *testing.T) {
	rec := &fleetv1alpha1.AppliedManifestRecord{Spec: fleetv1alpha1.AppliedManifestRecordSpec{Manifests: []fleetv1alpha1.ManifestIdentifier{
		{Group: "kro.run", Version: "v1alpha1", Kind: "GenAIService", Namespace: "fleet-demo", Name: "a"},
		{Version: "v1", Kind: "ConfigMap", Namespace: "fleet-demo", Name: "b"},
	}}}
	d := definition("genaiservice.kro.run", "GenAIService")
	if DefinitionKind(d).Kind != "GenAIService" || DefinitionKind(d).Group != "kro.run" {
		t.Fatalf("unexpected kind: %v", DefinitionKind(d))
	}
	d.Spec.ResourceGraphDefinition.Schema.Group = ""
	if DefinitionKind(d).Group != "kro.run" {
		t.Fatal("group must default to kro.run like kro does")
	}
	if manifestsOfKind(rec, DefinitionKind(d)) != 1 {
		t.Fatal("expected exactly the GenAIService manifest to count")
	}
	if manifestsOfKind(rec, DefinitionKind(definition("x", "Other"))) != 0 {
		t.Fatal("other kinds must not count")
	}
}

func TestMemberLocalInstances(t *testing.T) {
	inst := func(ns, name string) unstructured.Unstructured {
		u := unstructured.Unstructured{}
		u.SetGroupVersionKind(GenAIServiceGVK)
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	found := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{inst("fleet-demo", "placed"), inst("fleet-demo", "local"), inst("other", "placed")}}
	tracked := map[types.NamespacedName]bool{{Namespace: "fleet-demo", Name: "placed"}: true}
	// Only the instances no record tracks are member-local; the same name
	// in another namespace is a different instance.
	if n := MemberLocalInstances(found, tracked); n != 2 {
		t.Fatalf("expected 2 member-local instances, got %d", n)
	}
	if n := MemberLocalInstances(nil, tracked); n != 0 {
		t.Fatalf("no list means no instances, got %d", n)
	}
	if n := MemberLocalInstances(&unstructured.UnstructuredList{}, nil); n != 0 {
		t.Fatalf("an empty list means no instances, got %d", n)
	}
}
