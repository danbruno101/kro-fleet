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
//
// Member credentials come from ClusterProfile.status.accessProviders
// (KEP-4322 / KEP-5339): the cluster-inventory-api provider's AccessProvider
// strategy pairs each profile's access provider with an exec credential
// plugin declared in an access-providers file (sigs.k8s.io/cluster-inventory-api
// pkg/access format). Without a file, a built-in default is used that
// resolves the "kubeconfig-secretreader" provider through the upstream
// kubeconfig-secretreader plugin, which reads each member's kubeconfig
// Secret from the hub — the kind fleet's setup. Cloud fleets pass a file
// mapping their providers (e.g. eks/gke/aks) to the cloud CLIs' exec plugins.
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
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
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

// defaultProviderName is the access provider name the built-in default
// resolves; ClusterProfiles registered by scripts/setup-fleet.sh advertise it.
const defaultProviderName = "kubeconfig-secretreader"

func main() {
	var hubKubeconfig, hubContext, fleetNamespace, accessProvidersFile, secretReaderPlugin string
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "", "path to a kubeconfig with hub access (default: $KUBECONFIG, then ~/.kube/config, then in-cluster)")
	flag.StringVar(&hubContext, "hub-context", "", "kubeconfig context of the hub cluster (current context if empty)")
	flag.StringVar(&fleetNamespace, "fleet-namespace", "fleet-system", "hub namespace holding ClusterProfiles (and, for the default provider, the members' kubeconfig Secrets)")
	flag.StringVar(&accessProvidersFile, "access-providers-file", "", "cluster-inventory-api access providers file (pkg/access JSON) mapping ClusterProfile access provider names to exec credential plugins; empty = built-in kubeconfig-secretreader default")
	flag.StringVar(&secretReaderPlugin, "kubeconfig-secretreader-plugin", "kubeconfig-secretreader-plugin", "command (looked up on PATH) or path of the upstream kubeconfig-secretreader exec plugin used by the built-in default provider")
	flag.Parse()

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrllog.Log.WithName("fleet-controller")

	if err := run(hubKubeconfig, hubContext, fleetNamespace, accessProvidersFile, secretReaderPlugin); err != nil {
		log.Error(err, "fleet controller failed")
		os.Exit(1)
	}
}

func run(hubKubeconfig, hubContext, fleetNamespace, accessProvidersFile, secretReaderPlugin string) error {
	ctx := signals.SetupSignalHandler()
	log := ctrllog.Log.WithName("fleet-controller")

	// Standard loading order: --hub-kubeconfig, else $KUBECONFIG, else
	// ~/.kube/config, else in-cluster (when running on the hub itself).
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = hubKubeconfig
	hubClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{CurrentContext: hubContext},
	)
	inCluster := false
	hubCfg, err := hubClientConfig.ClientConfig()
	if err != nil {
		if hubKubeconfig == "" && hubContext == "" {
			var inClusterErr error
			if hubCfg, inClusterErr = rest.InClusterConfig(); inClusterErr != nil {
				return fmt.Errorf("failed to load hub config from kubeconfig (%v) or in-cluster (%v)", err, inClusterErr)
			}
			inCluster = true
		} else {
			return fmt.Errorf("failed to load hub config: %w", err)
		}
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))
	utilruntime.Must(fleetv1alpha1.AddToScheme(scheme))

	var accessCfg *access.Config
	if accessProvidersFile != "" {
		if accessCfg, err = access.NewFromFile(accessProvidersFile); err != nil {
			return fmt.Errorf("failed to load access providers file: %w", err)
		}
		log.Info("using access providers file", "path", accessProvidersFile, "providers", len(accessCfg.Providers))
	} else {
		// The plugin runs as a child process and needs its own hub access.
		// In-cluster it finds the service account on its own; as a host
		// process it gets a minified copy of the hub context (0600 temp
		// file, removed on exit) so it never depends on the caller's
		// current-context.
		var env []clientcmdapi.ExecEnvVar
		if !inCluster {
			path, cleanup, err := writeHubKubeconfig(hubClientConfig, hubContext)
			if err != nil {
				return err
			}
			defer cleanup()
			env = append(env, clientcmdapi.ExecEnvVar{Name: "KUBECONFIG", Value: path})
		}
		accessCfg = defaultAccessConfig(secretReaderPlugin, env)
		log.Info("using built-in access provider", "provider", defaultProviderName, "plugin", secretReaderPlugin)
	}

	provider, err := clusterinventoryapi.New(clusterinventoryapi.Options{
		KubeconfigStrategyOption: kubeconfigstrategy.Option{
			AccessProvider: &kubeconfigstrategy.AccessProviderOption{Provider: accessCfg},
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
	// Orphaned inventory records (instance gone, member unreachable at the
	// time) are settled separately, when their member is engaged again.
	rr := &controller.RecordReconciler{FleetNamespace: fleetNamespace}
	if err := rr.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up record controller: %w", err)
	}

	return mgr.Start(ctx)
}

// defaultAccessConfig declares the built-in provider: the upstream
// kubeconfig-secretreader exec plugin, which receives the Secret coordinates
// from the ClusterProfile's cluster extension
// (client.authentication.k8s.io/exec: {name, key, namespace}) and answers
// with the credentials found in that kubeconfig.
func defaultAccessConfig(plugin string, env []clientcmdapi.ExecEnvVar) *access.Config {
	return access.New([]access.Provider{{
		Name: defaultProviderName,
		ExecConfig: &clientcmdapi.ExecConfig{
			APIVersion:         "client.authentication.k8s.io/v1",
			Command:            plugin,
			Env:                env,
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			ProvideClusterInfo: true,
		},
	}})
}

// writeHubKubeconfig writes a minified, flattened copy of the hub context to
// a private temp file for the exec plugin and returns its path plus a
// cleanup function.
func writeHubKubeconfig(cc clientcmd.ClientConfig, hubContext string) (string, func(), error) {
	raw, err := cc.RawConfig()
	if err != nil {
		return "", nil, fmt.Errorf("failed to read hub kubeconfig: %w", err)
	}
	if hubContext != "" {
		raw.CurrentContext = hubContext
	}
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return "", nil, fmt.Errorf("failed to minify hub kubeconfig: %w", err)
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return "", nil, fmt.Errorf("failed to flatten hub kubeconfig: %w", err)
	}
	f, err := os.CreateTemp("", "kro-fleet-hub-*.kubeconfig")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp kubeconfig: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	if err := clientcmd.WriteToFile(raw, path); err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("failed to write hub kubeconfig for the exec plugin: %w", err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}
