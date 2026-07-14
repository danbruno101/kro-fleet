# kro-fleet

**A thin proof-of-concept for fleet-scoped KRO objects: author one object on a hub
cluster; it is placed across many member clusters (and clouds), with per-member
status aggregated back on the hub.**

This is the reference implementation that backs the design proposal
[`docs/proposals/KEP-kro-multicluster.md`](docs/proposals/KEP-kro-multicluster.md)
— "Native Multi-Cluster Mode for KRO."

> **Status:** discussion-stage PoC. Built on kro `v1alpha1` and SIG-Multicluster
> primitives (ClusterProfile / KEP-4322, `multicluster-runtime`). **Not for
> production.** Runs entirely on `kind` — no cloud account, no GPU.

## The idea

[KRO](https://github.com/kubernetes-sigs/kro) lets a platform team define a custom
API (a `ResourceGraphDefinition`) that expands a short developer instance into a
reconciled graph of Kubernetes resources — **with no custom Go controller**. The
sister project **[kro-genaiops-demo](https://github.com/danbruno101/kro-genaiops-demo)**
proves that graph is *portable*: the same `GenAIService` runs unchanged on GKE, AKS,
and EKS.

Its honest limit: **ownership is cluster-local.** `ownerReferences` never cross a
cluster boundary, so "the same workload on N clusters" is N independent objects,
applied N times, with N control loops and no aggregated view.

`kro-fleet` closes that gap: **one placement-enabled object on a hub cluster** →
placed onto the matching member clusters → **status aggregated back on the hub.**
Change it once, apply it once, it disperses.

## Scope (important, and settled)

- **This PoC does NOT fork or modify kro.** It runs **stock kro on each member
  cluster** (which does the local graph expansion, exactly as in the demo) and adds
  **only a hub-side fleet placement controller**.
- It is built **on existing SIG-Multicluster standards** — not a new propagation
  engine:
  - **[ClusterProfile / Cluster Inventory API](https://github.com/kubernetes-sigs/cluster-inventory-api)**
    (KEP-4322) for the fleet registry + member credentials.
  - **[multicluster-runtime](https://github.com/kubernetes-sigs/multicluster-runtime)**
    for reconciling across a dynamic fleet.
- The "native mode inside kro" (expand-on-hub, one control loop) is **future work**,
  tracked honestly in [`docs/KEP-GAP.md`](docs/KEP-GAP.md).

## Architecture (thin PoC)

```
                         HUB CLUSTER
   ┌───────────────────────────────────────────────────────────┐
   │  fleet placement controller (multicluster-runtime)         │
   │   • watches a FleetGenAIService (placement selector)       │
   │   • reads the ClusterProfile inventory                     │
   │   • places the GenAIService onto matching members          │
   │   • tracks applied manifests, GC on unplace/delete         │
   │   • aggregates per-member status onto the hub object       │
   └───────────────┬───────────────┬───────────────┬───────────┘
        credentials via ClusterProfile.status.accessProviders
                   │               │               │
             ┌─────▼─────┐   ┌─────▼─────┐   ┌─────▼─────┐
             │ member gke│   │ member aks│   │ member eks│
             │ stock kro │   │ stock kro │   │ stock kro │  ← expands the graph
             │ + RGDs    │   │ + RGDs    │   │ + RGDs    │    locally, per cloud
             └───────────┘   └───────────┘   └───────────┘
```

## Try it

```bash
scripts/setup-fleet.sh 2                          # 1 hub + 2 member kind clusters
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub &
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo -o yaml   # status.clusters[]
scripts/e2e.sh                                    # assert all six success criteria
scripts/teardown-fleet.sh
```

To *see* the fleet — one object across the members, its object graph, pod
logs — build the [Headlamp plugin](headlamp-plugin/README.md).

See [`docs/RUNBOOK.md`](docs/RUNBOOK.md) for the guided walkthrough, and
[`docs/phase0-validation.md`](docs/phase0-validation.md) for the pinned
versions and provider findings this is built on.

## Related

- **Demo (single-cluster portability):** https://github.com/danbruno101/kro-genaiops-demo
- **The proposal:** [`docs/proposals/KEP-kro-multicluster.md`](docs/proposals/KEP-kro-multicluster.md)
- **The MVP demo plan (3 clusters + Headlamp plugin + recording):** [`docs/proposals/kro-fleet-mvp-plan.md`](docs/proposals/kro-fleet-mvp-plan.md)
- **Fleet-scale operating model (inspiration):** https://lucy.sh/fleet-scale-kubernetes

## License

Apache License 2.0 — matching kro and the `kubernetes-sigs` ecosystem.
