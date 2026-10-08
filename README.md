# kro-fleet

**A thin proof-of-concept for fleet-scoped KRO objects: author one object on a hub
cluster; it is placed across many member clusters (and clouds), with per-member
status aggregated back on the hub.**

Two documents drive this repository, and the PoC is their reference
implementation:

- **The design doc** — [`docs/design/fleet-scoped-kro.md`](docs/design/fleet-scoped-kro.md):
  the problem framing for SIG-Multicluster. Composition vs placement, the three
  motivating use cases (**capacity, compliance, failover**), requirements, and
  the open questions. **Start here if you came to review the idea.**
- **The KEP** — [`docs/proposals/KEP-kro-multicluster.md`](docs/proposals/KEP-kro-multicluster.md)
  ("Native Multi-Cluster Mode for KRO", **v2**): the mechanism. v2 reframes
  placement as something KRO *consumes* — an inline selector or a referenced
  external decision (`decisionRef`) — never something it computes.

> **Status:** discussion-stage PoC. Built on kro `v1alpha1` and SIG-Multicluster
> primitives (ClusterProfile / KEP-4322, PlacementDecision / KEP-5313, About
> API / KEP-2149, `multicluster-runtime`). **Not for production.** CI runs
> entirely on `kind` — no cloud account, no GPU; the KubeCon EU 2027 demo runs
> the same code on EKS + GKE + AKS ([`docs/demo-cloud.md`](docs/demo-cloud.md)).
> The PoC implements the KEP's **v2.1 scope**: selector or consumed
> `PlacementDecision`, per-member replicas (division), fail-closed `Placed`,
> per-member inventory. What still differs from the native design is
> ledgered in [`docs/KEP-GAP.md`](docs/KEP-GAP.md).

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
placed onto the member clusters a placement resolves to → **status aggregated
back on the hub.** Change it once, apply it once, it disperses. Placement is
either an inline selector (*replication*: every member gets the whole graph)
or a standard `PlacementDecision` written by something else (*division*: a
model server asking for 8 replicas that no single cluster can hold lands as
4 + 2 + 2 across clouds, with one rolled-up Ready). kro-fleet consumes the
decision; it never computes one.

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
  - **[PlacementDecision](https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/5313-placement-decision-api)**
    (KEP-5313) as the placement input, and **[About API `ClusterProperty`](https://github.com/kubernetes-sigs/about-api)**
    (KEP-2149) for what each cluster advertises — the two things the standard
    does not carry (per-member parameters, a decision-level reason) are
    conventions written up for the SIG in [`docs/placement-decision-gaps.md`](docs/placement-decision-gaps.md).
- The "native mode inside kro" (expand-on-hub, one control loop) is **future
  work**; so is MCS-API for cross-cluster networking. Every proposed-vs-built
  difference is tracked honestly in [`docs/KEP-GAP.md`](docs/KEP-GAP.md).
- The scheduler, inventory agent and gateway in `cmd/fleet-demo` are
  **demo-only** scaffolding so the capacity story has something producing
  properties and decisions; OCM, Karmada or Kueue replace them without touching
  kro-fleet.

## Architecture (thin PoC)

```
                         HUB CLUSTER
   ┌───────────────────────────────────────────────────────────┐
   │  fleet placement controller (multicluster-runtime)         │
   │   • watches a FleetGenAIService: placement = clusterSelector│
   │     or a consumed PlacementDecision (+ per-member replicas) │
   │   • reads the ClusterProfile inventory + status.properties │
   │   • places each member's copy; AppliedManifestRecord per   │
   │     (instance, member); GC on unplace/delete, orphan ledger │
   │   • Placed (fail-closed) + Ready (by replicas) on the hub   │
   │  demo-only: inventory agent, scheduler, gateway (fleet-demo)│
   └───────────────┬───────────────┬───────────────┬───────────┘
        credentials via ClusterProfile.status.accessProviders (exec plugins)
                   │               │               │
             ┌─────▼─────┐   ┌─────▼─────┐   ┌─────▼─────┐
             │ member gke│   │ member aks│   │ member eks│
             │ stock kro │   │ stock kro │   │ stock kro │  ← expands the graph
             │ + RGDs    │   │ + RGDs    │   │ + RGDs    │    locally, per cloud
             └───────────┘   └───────────┘   └───────────┘
```

## Try it

```bash
scripts/setup-fleet.sh 3                          # 1 hub + 3 member kind clusters (gke/aks/eks personas, 4 fake GPUs each)
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub \
  --kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin &   # built by setup-fleet.sh
go run ./cmd/fleet-demo all --hub-context kind-kro-fleet-hub \
  --kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin &   # demo-only: inventory agent + scheduler

# replication: one selector, every matching member gets the whole graph
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
# division: 8 replicas no member can hold alone, split by a PlacementDecision
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-divided.yaml
kubectl --context kind-kro-fleet-hub get fgs,placementdecisions,appliedmanifestrecords -n fleet-demo
scripts/e2e.sh                                    # assert all twelve criteria (CI runs this)
scripts/teardown-fleet.sh
```

The KubeCon EU 2027 demo on real clouds — `scripts/provision.sh`,
`scripts/demo.sh`, `scripts/reset.sh`, `scripts/teardown.sh` — is in
[`docs/demo-cloud.md`](docs/demo-cloud.md).

To *see* the fleet — one object across the members, its object graph, pod
logs — build the [Headlamp plugin](headlamp-plugin/README.md).

See [`docs/RUNBOOK.md`](docs/RUNBOOK.md) for the guided walkthrough,
[`docs/rebaseline-2026-10.md`](docs/rebaseline-2026-10.md) for the pinned
versions, and [`docs/phase0-validation.md`](docs/phase0-validation.md) for the
original provider findings this is built on.

## Related

- **Demo (single-cluster portability):** https://github.com/danbruno101/kro-genaiops-demo
- **The design doc (start here):** [`docs/design/fleet-scoped-kro.md`](docs/design/fleet-scoped-kro.md)
- **The KEP (v2):** [`docs/proposals/KEP-kro-multicluster.md`](docs/proposals/KEP-kro-multicluster.md)
- **The honest ledger (proposed vs built):** [`docs/KEP-GAP.md`](docs/KEP-GAP.md)
- **What the PoC consumes from KEP-5313, and the two gaps for SIG-Multicluster:** [`docs/placement-decision-gaps.md`](docs/placement-decision-gaps.md)
- **Cluster properties the demo uses:** [`docs/properties.md`](docs/properties.md)
- **The cloud demo (EKS + GKE + AKS):** [`docs/demo-cloud.md`](docs/demo-cloud.md)
- **October 2026 re-baseline (versions, what broke, Work API evaluation):** [`docs/rebaseline-2026-10.md`](docs/rebaseline-2026-10.md)
- **The MVP demo plan (3 clusters + Headlamp plugin + recording):** [`docs/proposals/kro-fleet-mvp-plan.md`](docs/proposals/kro-fleet-mvp-plan.md)
- **Fleet-scale operating model (inspiration):** https://lucy.sh/fleet-scale-kubernetes

## License

Apache License 2.0 — matching kro and the `kubernetes-sigs` ecosystem.
