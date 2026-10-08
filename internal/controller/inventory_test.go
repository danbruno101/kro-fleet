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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

func TestRecordName(t *testing.T) {
	if got := RecordName("demo-llm", "kro-fleet-member-1"); got != "demo-llm-kro-fleet-member-1" {
		t.Errorf("short names must concatenate verbatim, got %q", got)
	}
	long := strings.Repeat("a", 200)
	got := RecordName(long, strings.Repeat("b", 200))
	if len(got) > validation.DNS1123SubdomainMaxLength {
		t.Errorf("record name exceeds the DNS subdomain limit: %d", len(got))
	}
	if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
		t.Errorf("record name is not a DNS subdomain: %v", errs)
	}
	if got == RecordName(long, strings.Repeat("c", 200)) {
		t.Error("truncated names for different members must differ")
	}
}

func TestSameManifestSet(t *testing.T) {
	a := fleetv1alpha1.ManifestIdentifier{Group: "kro.run", Version: "v1alpha1", Kind: "GenAIService", Namespace: "fleet-demo", Name: "demo-llm"}
	b := a
	b.Name = "other"
	if !sameManifestSet([]fleetv1alpha1.ManifestIdentifier{a, b}, []fleetv1alpha1.ManifestIdentifier{b, a}) {
		t.Error("order must not matter")
	}
	if sameManifestSet([]fleetv1alpha1.ManifestIdentifier{a}, []fleetv1alpha1.ManifestIdentifier{a, b}) {
		t.Error("different sizes must differ")
	}
	if sameManifestSet([]fleetv1alpha1.ManifestIdentifier{a}, []fleetv1alpha1.ManifestIdentifier{b}) {
		t.Error("different keys must differ")
	}
}

func TestTrackedManifestsIsUnionOfIntentAndConfirmed(t *testing.T) {
	id := func(name string) fleetv1alpha1.ManifestIdentifier {
		return fleetv1alpha1.ManifestIdentifier{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: name}
	}
	rec := &fleetv1alpha1.AppliedManifestRecord{
		Spec:   fleetv1alpha1.AppliedManifestRecordSpec{Manifests: []fleetv1alpha1.ManifestIdentifier{id("intended"), id("both")}},
		Status: fleetv1alpha1.AppliedManifestRecordStatus{Applied: []fleetv1alpha1.AppliedManifest{{ManifestIdentifier: id("both"), UID: "u1"}, {ManifestIdentifier: id("stale")}}},
	}
	got := trackedManifests(rec)
	if len(got) != 3 {
		t.Fatalf("expected 3 tracked manifests, got %d: %+v", len(got), got)
	}
	names := map[string]types.UID{}
	for _, m := range got {
		names[m.Name] = m.UID
	}
	if names["both"] != "u1" {
		t.Error("confirmed entries must keep their UID")
	}
	for _, n := range []string{"intended", "stale"} {
		if _, ok := names[n]; !ok {
			t.Errorf("missing %q", n)
		}
	}
}

// deleteManifests must delete only objects this instance placed, and must
// report everything else (absent, foreign, UID mismatch) as gone without
// touching it.
func TestDeleteManifestsOnlyDeletesOwnObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ours := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ours", Namespace: "ns", UID: "uid-ours", Labels: map[string]string{PlacedByLabel: "demo-llm"}}}
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "ns", UID: "uid-foreign"}}
	replaced := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "replaced", Namespace: "ns", UID: "uid-new", Labels: map[string]string{PlacedByLabel: "demo-llm"}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ours, foreign, replaced).Build()

	id := func(name string) fleetv1alpha1.ManifestIdentifier {
		return fleetv1alpha1.ManifestIdentifier{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: name}
	}
	manifests := []fleetv1alpha1.AppliedManifest{
		{ManifestIdentifier: id("ours"), UID: "uid-ours"},
		{ManifestIdentifier: id("foreign")},
		{ManifestIdentifier: id("replaced"), UID: "uid-old"},
		{ManifestIdentifier: id("absent")},
	}
	gone, err := deleteManifests(context.Background(), cl, "demo-llm", manifests)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	goneNames := map[string]bool{}
	for _, g := range gone {
		goneNames[g.Name] = true
	}
	// Our object was deleted this pass; it is confirmed gone on the next
	// pass (NotFound), so it must NOT be reported gone yet.
	if goneNames["ours"] {
		t.Error("a just-deleted object must be confirmed on a later pass, not reported gone immediately")
	}
	for _, n := range []string{"foreign", "replaced", "absent"} {
		if !goneNames[n] {
			t.Errorf("%q must be reported gone without being touched", n)
		}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvkOf(id("ours")))
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "ours"}, u); !apierrors.IsNotFound(err) {
		t.Errorf("our object should have been deleted, got err=%v", err)
	}
	for _, n := range []string{"foreign", "replaced"} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvkOf(id(n)))
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: n}, u); err != nil {
			t.Errorf("%q must not be deleted: %v", n, err)
		}
	}
	// Second pass: ours is now absent -> gone.
	gone, err = deleteManifests(context.Background(), cl, "demo-llm", manifests[:1])
	if err != nil || len(gone) != 1 || gone[0].Name != "ours" {
		t.Errorf("second pass must confirm the deletion, got gone=%v err=%v", gone, err)
	}
	_ = client.Object(nil)
}
