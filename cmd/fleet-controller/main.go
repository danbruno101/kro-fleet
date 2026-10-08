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
// on the hub, consumes their placement — an inline selector or a
// PlacementDecision (KEP-5313) written by something else — against the
// ClusterProfile inventory (cluster-inventory-api, KEP-4322), and reconciles
// the wrapped GenAIService into each decided member via multicluster-runtime,
// keeping a per-(instance, member) applied-manifest inventory. Members run
// stock kro; this controller never expands the graph and never computes a
// placement.
//
// Member credentials come from ClusterProfile.status.accessProviders; see
// internal/hub for the provider wiring and its flags.
package main

import (
	"flag"
	"fmt"
	"os"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/danbruno101/kro-fleet/internal/controller"
	"github.com/danbruno101/kro-fleet/internal/hub"
)

func main() {
	var opts hub.Options
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrllog.Log.WithName("fleet-controller")

	if err := run(opts); err != nil {
		log.Error(err, "fleet controller failed")
		os.Exit(1)
	}
}

func run(opts hub.Options) error {
	ctx := signals.SetupSignalHandler()

	mgr, cleanup, err := hub.NewManager(opts)
	defer cleanup()
	if err != nil {
		return err
	}

	r := &controller.FleetReconciler{FleetNamespace: opts.FleetNamespace}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up fleet controller: %w", err)
	}
	// Orphaned inventory records (instance gone, member unreachable at the
	// time) are settled separately, when their member is engaged again.
	rr := &controller.RecordReconciler{FleetNamespace: opts.FleetNamespace}
	if err := rr.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to set up record controller: %w", err)
	}

	return mgr.Start(ctx)
}
