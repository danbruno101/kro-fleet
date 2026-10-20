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
		_, _, del, _ := RollupDefinition(DefinitionRollup{Enabled: true, Blocked: []BlockedMember{{Name: "m1", Instances: 2}, {Name: "m2", Instances: 1}}, Removing: []string{"m3"}})
		if del.Status != metav1.ConditionFalse || del.Reason != fleetv1alpha1.ReasonBlockedByInstances || !strings.Contains(del.Message, "m1 (2 instance(s)), m2 (1 instance(s))") {
			t.Fatalf("blocked must win and name members with counts: %+v", del)
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
	rej := member("m3", true, "Inactive", "False", "GraphAccepted/InvalidResourceGraph")
	rej.Message = "no matches for kind SkewMarker"
	d.Status.Members = []fleetv1alpha1.MemberDefinitionStatus{
		member("m1", true, "Active", "True", ""),
		{Name: "m2", Applied: false, AppliedHash: "h1", State: "Active", Ready: "True"},
		rej,
	}
	if ok, _ := DefinitionGate(d, gvk, "m1"); !ok {
		t.Fatal("accepted member must pass")
	}
	if ok, why := DefinitionGate(d, gvk, "m2"); ok || !strings.Contains(why, `holds revision "h1", hub is at "h2"`) {
		t.Fatalf("stale revision must not pass: %v %q", ok, why)
	}
	if ok, why := DefinitionGate(d, gvk, "m3"); ok || !strings.Contains(why, "GraphAccepted/InvalidResourceGraph: no matches for kind SkewMarker") {
		t.Fatalf("rejecting member must carry its own reason: %v %q", ok, why)
	}
	if ok, why := DefinitionGate(d, gvk, "m4"); ok || !strings.Contains(why, "not distributed to this member") {
		t.Fatalf("untargeted member must not pass: %v %q", ok, why)
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
