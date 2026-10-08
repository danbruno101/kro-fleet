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

// Package hub builds the multicluster-runtime manager every kro-fleet
// process shares: hub access from the standard kubeconfig chain (or
// in-cluster), and member access through the cluster-inventory-api provider's
// AccessProvider strategy, i.e. ClusterProfile.status.accessProviders plus an
// exec credential plugin (KEP-4322 / KEP-5339).
//
// Without an access-providers file, a built-in default resolves the
// "kubeconfig-secretreader" provider through the upstream
// kubeconfig-secretreader plugin, which reads each member's kubeconfig
// Secret from the hub — the kind fleet's setup. Cloud fleets pass a file
// (sigs.k8s.io/cluster-inventory-api pkg/access JSON) mapping their
// providers (e.g. eks/gke/aks) to the cloud CLIs' exec plugins.
package hub

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
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	clusterinventoryapi "sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api"
	"sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api/kubeconfigstrategy"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// DefaultProviderName is the access provider name the built-in default
// resolves; ClusterProfiles registered by scripts/lib/fleet.sh advertise it.
const DefaultProviderName = "kubeconfig-secretreader"

// Options select the hub and how members are reached.
type Options struct {
	HubKubeconfig       string
	HubContext          string
	FleetNamespace      string
	AccessProvidersFile string
	SecretReaderPlugin  string
}

// BindFlags registers the shared flags on a FlagSet.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.HubKubeconfig, "hub-kubeconfig", "", "path to a kubeconfig with hub access (default: $KUBECONFIG, then ~/.kube/config, then in-cluster)")
	fs.StringVar(&o.HubContext, "hub-context", "", "kubeconfig context of the hub cluster (current context if empty)")
	fs.StringVar(&o.FleetNamespace, "fleet-namespace", "fleet-system", "hub namespace holding ClusterProfiles (and, for the default provider, the members' kubeconfig Secrets)")
	fs.StringVar(&o.AccessProvidersFile, "access-providers-file", "", "cluster-inventory-api access providers file (pkg/access JSON) mapping ClusterProfile access provider names to exec credential plugins; empty = built-in kubeconfig-secretreader default")
	fs.StringVar(&o.SecretReaderPlugin, "kubeconfig-secretreader-plugin", "kubeconfig-secretreader-plugin", "command (looked up on PATH) or path of the upstream kubeconfig-secretreader exec plugin used by the built-in default provider")
}

// Scheme returns the scheme shared by kro-fleet processes.
func Scheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))
	utilruntime.Must(fleetv1alpha1.AddToScheme(scheme))
	return scheme
}

// NewManager builds the multicluster manager with the cluster-inventory-api
// provider set up. The returned cleanup removes any temp file the built-in
// provider wrote and must be called after the manager stops.
func NewManager(o Options) (mcmanager.Manager, func(), error) {
	log := ctrllog.Log.WithName("hub")
	cleanup := func() {}

	// Standard loading order: --hub-kubeconfig, else $KUBECONFIG, else
	// ~/.kube/config, else in-cluster (when running on the hub itself).
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = o.HubKubeconfig
	hubClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{CurrentContext: o.HubContext},
	)
	inCluster := false
	hubCfg, err := hubClientConfig.ClientConfig()
	if err != nil {
		if o.HubKubeconfig == "" && o.HubContext == "" {
			var inClusterErr error
			if hubCfg, inClusterErr = rest.InClusterConfig(); inClusterErr != nil {
				return nil, cleanup, fmt.Errorf("failed to load hub config from kubeconfig (%v) or in-cluster (%v)", err, inClusterErr)
			}
			inCluster = true
		} else {
			return nil, cleanup, fmt.Errorf("failed to load hub config: %w", err)
		}
	}

	var accessCfg *access.Config
	if o.AccessProvidersFile != "" {
		if accessCfg, err = access.NewFromFile(o.AccessProvidersFile); err != nil {
			return nil, cleanup, fmt.Errorf("failed to load access providers file: %w", err)
		}
		log.Info("using access providers file", "path", o.AccessProvidersFile, "providers", len(accessCfg.Providers))
	} else {
		// The plugin runs as a child process and needs its own hub access.
		// In-cluster it finds the service account on its own; as a host
		// process it gets a minified copy of the hub context (0600 temp
		// file, removed on exit) so it never depends on the caller's
		// current-context.
		var env []clientcmdapi.ExecEnvVar
		if !inCluster {
			path, rm, err := writeHubKubeconfig(hubClientConfig, o.HubContext)
			if err != nil {
				return nil, cleanup, err
			}
			cleanup = rm
			env = append(env, clientcmdapi.ExecEnvVar{Name: "KUBECONFIG", Value: path})
		}
		accessCfg = defaultAccessConfig(o.SecretReaderPlugin, env)
		log.Info("using built-in access provider", "provider", DefaultProviderName, "plugin", o.SecretReaderPlugin)
	}

	provider, err := clusterinventoryapi.New(clusterinventoryapi.Options{
		KubeconfigStrategyOption: kubeconfigstrategy.Option{
			AccessProvider: &kubeconfigstrategy.AccessProviderOption{Provider: accessCfg},
		},
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("failed to create cluster-inventory-api provider: %w", err)
	}

	mgr, err := mcmanager.New(hubCfg, provider, mcmanager.Options{
		Scheme:  Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("failed to create multicluster manager: %w", err)
	}
	if err := provider.SetupWithManager(mgr); err != nil {
		return nil, cleanup, fmt.Errorf("failed to set up provider: %w", err)
	}
	return mgr, cleanup, nil
}

// defaultAccessConfig declares the built-in provider: the upstream
// kubeconfig-secretreader exec plugin, which receives the Secret coordinates
// from the ClusterProfile's cluster extension
// (client.authentication.k8s.io/exec: {name, key, namespace}) and answers
// with the credentials found in that kubeconfig.
func defaultAccessConfig(plugin string, env []clientcmdapi.ExecEnvVar) *access.Config {
	return access.New([]access.Provider{{
		Name: DefaultProviderName,
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
