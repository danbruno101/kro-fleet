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

// Phase 0 validation harness (see CLAUDE.md "Build phases").
//
// Proves the PoC foundation end to end with a TRIVIAL object before any
// FleetGenAIService code is written:
//
//   - the hub cluster carries ClusterProfile objects (cluster-inventory-api,
//     KEP-4322) for each member, plus a labeled kubeconfig Secret per member
//     (the provider's "Secret" kubeconfig strategy);
//   - a multicluster-runtime manager wired with the official
//     cluster-inventory-api provider engages every healthy ClusterProfile;
//   - a ConfigMap in the hub's fleet-demo namespace labeled
//     fleet.kro.run/place=true is server-side-applied into every engaged
//     member, and re-converged when the hub copy changes or a member copy
//     drifts.
//
// Run it locally against kind:
//
//	go run ./hack/phase0 --hub-kubeconfig $KUBECONFIG --hub-context kind-kro-fleet-hub
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	clusterinventoryapi "sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api"
	"sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api/kubeconfigstrategy"
)

const (
	// consumerName identifies this controller to the provider's Secret
	// kubeconfig strategy (label x-k8s.io/cluster-inventory-consumer).
	consumerName = "kro-fleet"
	// fleetNamespace is where ClusterProfiles (and their kubeconfig
	// Secrets) live on the hub.
	fleetNamespace = "fleet-system"
	// demoNamespace holds the trivial object we propagate.
	demoNamespace = "fleet-demo"
	// placeLabel marks hub ConfigMaps that should be placed onto members.
	placeLabel = "fleet.kro.run/place"
	// fieldOwner is the server-side-apply field manager on members.
	fieldOwner = "kro-fleet-phase0"
)

func main() {
	var kubeconfig, hubContext string
	flag.StringVar(&kubeconfig, "hub-kubeconfig", os.Getenv("KUBECONFIG"), "path to the kubeconfig with hub access")
	flag.StringVar(&hubContext, "hub-context", "kind-kro-fleet-hub", "kubeconfig context of the hub cluster")
	flag.Parse()

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrllog.Log.WithName("phase0")
	ctx := signals.SetupSignalHandler()

	if err := run(ctx, kubeconfig, hubContext); err != nil {
		log.Error(err, "phase 0 harness failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, kubeconfig, hubContext string) error {
	hubCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: hubContext},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to load hub kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))

	provider, err := clusterinventoryapi.New(clusterinventoryapi.Options{
		KubeconfigStrategyOption: kubeconfigstrategy.Option{
			Secret: &kubeconfigstrategy.SecretStrategyOption{ConsumerName: consumerName},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create cluster-inventory-api provider: %w", err)
	}

	mgr, err := mcmanager.New(hubCfg, provider, mcmanager.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("failed to create multicluster manager: %w", err)
	}

	if err := provider.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up provider with manager: %w", err)
	}

	r := &configMapPlacer{mgr: mgr}
	// Engage the local (hub) cluster too: hub ConfigMap events drive
	// placement; member events re-converge drifted copies.
	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("phase0-configmap-placer").
		For(&corev1.ConfigMap{}, mcbuilder.WithEngageWithLocalCluster(true)).
		Complete(r); err != nil {
		return fmt.Errorf("failed to build controller: %w", err)
	}

	return mgr.Start(ctx)
}

// configMapPlacer copies hub ConfigMaps labeled fleet.kro.run/place=true from
// the hub's fleet-demo namespace into the same namespace on every engaged
// member cluster, whichever cluster the triggering event came from.
type configMapPlacer struct {
	mgr mcmanager.Manager
}

func (r *configMapPlacer) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("cluster", req.ClusterName, "configmap", req.NamespacedName)

	if req.Namespace != demoNamespace {
		return ctrl.Result{}, nil
	}

	hubClient := r.mgr.GetLocalManager().GetClient()

	src := &corev1.ConfigMap{}
	if err := hubClient.Get(ctx, req.NamespacedName, src); err != nil {
		if apierrors.IsNotFound(err) {
			// Phase 0 proves placement + convergence only; GC of
			// deleted hub objects is phase 1 (finalizer +
			// applied-manifest tracking).
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get hub ConfigMap: %w", err)
	}
	if src.Labels[placeLabel] != "true" {
		return ctrl.Result{}, nil
	}

	profiles := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := hubClient.List(ctx, profiles, client.InNamespace(fleetNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ClusterProfiles: %w", err)
	}

	for i := range profiles.Items {
		clp := &profiles.Items[i]
		clusterName := multicluster.ClusterName(fmt.Sprintf("%s/%s", clp.Namespace, clp.Name))
		cl, err := r.mgr.GetCluster(ctx, clusterName)
		if err != nil {
			// Not engaged (yet) — e.g. unhealthy or missing kubeconfig.
			log.Info("member not engaged, skipping", "member", clusterName, "reason", err.Error())
			continue
		}

		ns := &corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: src.Namespace},
		}
		if err := cl.GetClient().Patch(ctx, ns, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to ensure namespace on %s: %w", clusterName, err)
		}

		dst := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      src.Name,
				Namespace: src.Namespace,
				Labels:    src.Labels,
			},
			Data: src.Data,
		}
		if err := cl.GetClient().Patch(ctx, dst, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to apply ConfigMap to %s: %w", clusterName, err)
		}
		log.Info("placed ConfigMap onto member", "member", clusterName)
	}

	return ctrl.Result{}, nil
}
