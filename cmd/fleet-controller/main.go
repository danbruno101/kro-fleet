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

// The hub-side fleet placement controller: watches FleetGenAIService objects
// on the hub, resolves placement against the ClusterProfile inventory
// (cluster-inventory-api, KEP-4322), and reconciles the wrapped GenAIService
// into each matching member via multicluster-runtime. Members run stock kro;
// this controller never expands the graph itself.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	clusterinventoryapi "sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api"
	"sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api/kubeconfigstrategy"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
	"github.com/danbruno101/kro-fleet/internal/controller"
)

func main() {
	var hubKubeconfig, hubContext, fleetNamespace, consumerName string
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", os.Getenv("KUBECONFIG"), "path to a kubeconfig with hub access (in-cluster config if empty)")
	flag.StringVar(&hubContext, "hub-context", "", "kubeconfig context of the hub cluster (current context if empty)")
	flag.StringVar(&fleetNamespace, "fleet-namespace", "fleet-system", "hub namespace holding ClusterProfiles and kubeconfig Secrets")
	flag.StringVar(&consumerName, "consumer-name", "kro-fleet", "cluster-inventory consumer name for the Secret kubeconfig strategy")
	flag.Parse()

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrllog.Log.WithName("fleet-controller")

	if err := run(hubKubeconfig, hubContext, fleetNamespace, consumerName); err != nil {
		log.Error(err, "fleet controller failed")
		os.Exit(1)
	}
}

func run(hubKubeconfig, hubContext, fleetNamespace, consumerName string) error {
	ctx := signals.SetupSignalHandler()

	var hubCfg *rest.Config
	var err error
	if hubKubeconfig == "" {
		hubCfg, err = rest.InClusterConfig()
	} else {
		hubCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: hubKubeconfig},
			&clientcmd.ConfigOverrides{CurrentContext: hubContext},
		).ClientConfig()
	}
	if err != nil {
		return fmt.Errorf("failed to load hub config: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))
	utilruntime.Must(fleetv1alpha1.AddToScheme(scheme))

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
		return fmt.Errorf("failed to set up provider: %w", err)
	}

	r := &controller.FleetReconciler{FleetNamespace: fleetNamespace}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up fleet controller: %w", err)
	}

	return mgr.Start(ctx)
}
