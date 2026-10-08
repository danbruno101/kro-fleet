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

// fleet-demo bundles the DEMO-ONLY components that surround kro-fleet so
// the capacity story has something producing properties and decisions:
//
//	fleet-demo inventory-agent  stand-in cluster manager: mirrors member
//	                            ClusterProperty objects (About API) into
//	                            ClusterProfile.status.properties, computing
//	                            free accelerators on the way
//	fleet-demo scheduler        reference placement producer: reads the hub
//	                            inventory, writes PlacementDecisions (KEP-5313)
//	                            with the per-member split
//	fleet-demo gateway          single entry point: spreads requests across
//	                            the members' own load balancers
//	fleet-demo all              inventory-agent + scheduler in one process
//
// None of this is part of kro-fleet proper; each piece is replaceable by a
// real cluster manager, a real scheduler (OCM, Karmada, Kueue) or weighted
// DNS without touching the fleet controller.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/danbruno101/kro-fleet/internal/demo/gateway"
	"github.com/danbruno101/kro-fleet/internal/demo/inventory"
	"github.com/danbruno101/kro-fleet/internal/demo/scheduler"
	"github.com/danbruno101/kro-fleet/internal/hub"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fleet-demo <inventory-agent|scheduler|gateway|all> [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet("fleet-demo "+cmd, flag.ExitOnError)
	var opts hub.Options
	opts.BindFlags(fs)
	accelerators := fs.String("accelerator-resources", "example.com/gpu,nvidia.com/gpu", "extended resource names counted as accelerators (inventory-agent)")
	capacityProperty := fs.String("capacity-property", "gpus-free.example.com", "ClusterProfile property holding free accelerators; one replica consumes one (scheduler)")
	totalProperty := fs.String("total-property", "gpus-total.example.com", "ClusterProfile property holding total accelerators, for the scheduler's own accounting (scheduler)")
	listen := fs.String("listen", ":8080", "address the gateway listens on (gateway)")
	workloadNamespace := fs.String("workload-namespace", "fleet-demo", "namespace of the FleetGenAIService the gateway fronts (gateway)")
	instance := fs.String("instance", "demo-llm", "FleetGenAIService the gateway fronts (gateway)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		usage()
	}

	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrllog.Log.WithName("fleet-demo").WithName(cmd)

	run := func() error {
		ctx := signals.SetupSignalHandler()
		mgr, cleanup, err := hub.NewManager(opts)
		defer cleanup()
		if err != nil {
			return err
		}
		switch cmd {
		case "inventory-agent", "all":
			a := &inventory.Agent{FleetNamespace: opts.FleetNamespace, AcceleratorResources: strings.Split(*accelerators, ",")}
			if err := a.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("failed to set up inventory agent: %w", err)
			}
			if cmd != "all" {
				break
			}
			fallthrough
		case "scheduler":
			s := &scheduler.Scheduler{FleetNamespace: opts.FleetNamespace, CapacityProperty: *capacityProperty, TotalProperty: *totalProperty}
			if err := s.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("failed to set up scheduler: %w", err)
			}
		case "gateway":
			g := &gateway.Gateway{Listen: *listen, Namespace: *workloadNamespace, Instance: *instance}
			if err := g.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("failed to set up gateway: %w", err)
			}
		default:
			usage()
		}
		return mgr.Start(ctx)
	}
	if err := run(); err != nil {
		log.Error(err, "fleet-demo failed")
		os.Exit(1)
	}
}
