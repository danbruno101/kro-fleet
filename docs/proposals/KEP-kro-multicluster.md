# KEP: Native Multi-Cluster Mode for KRO

| | |
|---|---|
| **Status** | Provisional / Draft (for discussion) |
| **Owning group** | SIG-Multicluster + KRO maintainers (`kubernetes-sigs/kro`) |
| **Stakeholders** | SIG Cloud Provider (cross-cloud portability), SIG Apps |
| **Authors** | @danbruno101 (+ TBD) |
| **Created** | 2026-07-02 |
| **Depends on** | KEP-4322 Cluster Inventory / ClusterProfile · `sigs.k8s.io/multicluster-runtime` |

---

## Summary

KRO (Kube Resource Orchestrator) lets a platform team define a custom API — a
`ResourceGraphDefinition` (RGD) — that expands a short, developer-facing instance
into a reconciled graph of Kubernetes resources, with **no custom Go controller**.
Today that graph is owned and reconciled **within a single cluster**.

This KEP proposes an **optional, opt-in multi-cluster mode** for KRO: a platform
team adds a `placement` selector to an RGD (or to a platform-owned instance), and a
KRO control plane running on a **hub** cluster reconciles the resource graph **into
each member cluster** selected from a **ClusterProfile** inventory, aggregating
per-member status back onto the hub object. **One object, authored and applied once
on the hub, is dispersed to the appropriate clusters across clouds.**

Crucially, this is built **on existing SIG-Multicluster standards** —
`ClusterProfile` (KEP-4322) for the fleet inventory and credentials, and
`multicluster-runtime` for cross-cluster reconciliation — **not** a new propagation
engine. KRO contributes the *authoring surface* and *graph semantics*; the
multi-cluster substrate is reused.

## Motivation

KRO already demonstrates a strong single-cluster value proposition: a
platform-owned template + a ~10-line developer instance, expanded and continuously
reconciled as an owned graph, portable **by re-application** across clusters and
clouds (see the reference demo `danbruno101/kro-genaiops-demo`, which runs the same
`GenAIService` unchanged on GKE, AKS, and EKS).

The honest limit of that model: **ownership and reconciliation are cluster-local.**
`ownerReferences` and garbage collection never cross a cluster boundary, so "the
same workload on N clusters" means N independent objects, applied N times, with N
separate control loops and no aggregated view. As organizations move to *fleets* of
tens–thousands of clusters ("homogeneous clusters with decoupled capacity"), the
per-cluster apply model does not scale operationally.

Multi-cluster orchestration engines exist (KubeFleet, Open Cluster Management,
Karmada, Argo CD ApplicationSets), but composing KRO with them externally loses
KRO's core DX wins: a **single authoring surface**, **placement as a first-class
field of the same object**, and **status folded back into the instance**. This KEP
brings the fleet into KRO's model *without* reinventing propagation, by adopting the
SIG-Multicluster inventory and runtime.

### Goals
- An **opt-in** `placement` concept on an RGD/instance that selects member clusters
  from a `ClusterProfile` inventory by label.
- A KRO **hub control plane** that reconciles the (selected subset of the) resource
  graph into each selected member and keeps it converged.
- **Per-member status aggregation** onto the hub instance (rolled-up + per-cluster
  conditions).
- **Cross-cluster lifecycle**: create, update, delete, and re-placement (a member
  joining/leaving the selector) drive corresponding changes in members, with clean
  teardown.
- Reuse **ClusterProfile** (KEP-4322) for inventory + credentials and
  **multicluster-runtime** for reconciliation. No new propagation protocol.
- Preserve single-cluster KRO behavior unchanged when `placement` is absent.

### Non-Goals
- Building a new cluster registry, credential system, or propagation engine (use
  SIG-Multicluster primitives).
- Cross-cluster networking / service discovery (that is MCS / SIG-Multicluster
  territory; out of scope).
- A scheduler with bin-packing/capacity awareness in v1 (start with
  label-selector placement; richer strategies are future work).
- Managing cluster *provisioning* (Cluster API's job).

## Proposal

### User stories
1. **Platform engineer, fleet rollout.** I author one RGD with
   `placement: {clusterSelector: {matchLabels: {tier: prod}}}`. I apply a single
   instance on the hub. KRO materializes the workload on every `prod` member across
   GKE/AKS/EKS and shows me a rolled-up status. When I bump `replicas`, all members
   converge. When a new `prod` cluster registers a `ClusterProfile`, the workload
   lands there automatically.
2. **Developer, unchanged.** I still submit the same cloud-agnostic ~10-line
   instance. I never name a cluster or a cloud. Placement is platform-owned.
3. **Operator, decommission.** I delete the hub instance (or a member leaves the
   selector); KRO removes the placed resources from the affected members with no
   orphans.

### The API (illustrative sketch)

Placement is **platform-owned** and lives on the RGD (or a platform instance),
keeping the developer instance cloud- and cluster-agnostic — mirroring KRO's
existing platform/developer separation.

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: genaiservice.kro.run
spec:
  # NEW — opt-in. Absent  => today's single-cluster behavior, unchanged.
  placement:
    # Select members from the ClusterProfile inventory (multicluster.x-k8s.io).
    clusterSelector:
      matchLabels: { tier: prod }
    # Optional: how KRO treats partial failure for the rolled-up status.
    tolerance: { minReadyClusters: 1 }
  schema: { ... }              # unchanged (the GenAIService developer API)
  resources:
    - id: deployment
      placement: members       # NEW, per-resource: members | hub  (default: members
      template: { ... }        #   when spec.placement is set, else hub)
    - id: fleetStatus
      placement: hub           # e.g. an aggregation/rollup object kept on the hub
      template: { ... }
```

Per-instance status gains a fleet view:

```yaml
status:
  clusters:
    - name: gke-prod-1     # ClusterProfile name
      ready: true
      conditions: [ ... ]  # reflected from the member
    - name: aks-prod-1
      ready: true
  summary:
    placed: 3
    ready: 3
  conditions:
    - type: Ready
      status: "True"       # per spec.placement.tolerance
```

### Architecture

```
                         HUB CLUSTER
   ┌───────────────────────────────────────────────────────────┐
   │  KRO control plane (multicluster-runtime based)            │
   │   • watches RGD instances (GenAIService, ...)              │
   │   • reads ClusterProfile inventory (KEP-4322)              │
   │   • resolves placement -> set of member ClusterProfiles    │
   │   • for each member: SSA the graph, track applied manifests│
   │   • collect member status -> aggregate onto hub instance   │
   └───────────────┬───────────────┬───────────────┬───────────┘
       provider/creds via ClusterProfile.status.accessProviders
                   │               │               │
             ┌─────▼─────┐   ┌─────▼─────┐   ┌─────▼─────┐
             │  member   │   │  member   │   │  member   │
             │  (GKE)    │   │  (AKS)    │   │  (EKS)    │
             │ PVC/Deploy│   │ PVC/Deploy│   │ PVC/Deploy│
             │ Svc/...   │   │ Svc/...   │   │ Svc/...   │
             └───────────┘   └───────────┘   └───────────┘
```

- **Inventory + credentials:** each member is represented by a `ClusterProfile`
  (`multicluster.x-k8s.io`). KRO uses `status.accessProviders` (KEP-4322/5339) to
  obtain member credentials via the standardized plugin mechanism — KRO does not
  invent a kubeconfig store.
- **Reconciliation:** KRO's per-RGD controller is built on **multicluster-runtime**,
  which starts/stops reconciliation against clusters discovered through a provider
  (a ClusterProfile provider). The existing single-cluster DAG/CEL/server-side-apply
  logic is reused per member.
- **Placement:** v1 = label selector over ClusterProfiles, evaluated
  continuously; membership changes trigger place/unplace.

## Design details

### Cross-cluster ownership, tracking, and GC
`ownerReferences` cannot span clusters, so KRO maintains a **hub-side applied-manifest
inventory** per `(instance, member)` (conceptually like OCM `ManifestWork`'s
`AppliedManifestWork`): the set of GVKs/names KRO applied to each member. A
**finalizer on the hub instance** drives deletion — on instance delete or when a
member leaves the selector, KRO deletes exactly the tracked objects from that member,
then clears the record. Within each member, KRO still uses normal
`ownerReferences`/SSA field ownership for the local sub-graph.

### Status aggregation & partial failure
KRO reflects each member's relevant conditions into `status.clusters[]` and computes
`status.conditions` from `spec.placement.tolerance` (e.g. `Ready` iff
`ready >= minReadyClusters`). Members unreachable beyond a grace period surface a
`Degraded`/`Unknown` condition rather than blocking the whole object.

### Placement & rescheduling
v1: static label-selector placement, re-evaluated on ClusterProfile inventory
changes. Explicit non-goals for v1: capacity-aware scheduling, spread constraints,
and drain/evacuate — designed as future `placement.strategy` extensions so the v1
API is forward-compatible.

### Credentials & security
- Member access flows exclusively through ClusterProfile `accessProviders` plugins;
  no credentials embedded in RGDs/instances.
- **Blast radius:** the hub holds fleet-wide reach and must be treated as
  high-value — HA control plane, least-privilege per-member RBAC, and per-RGD
  scoping of which ClusterProfiles an RGD may target (an admission/policy hook).
- Push model (hub → member API) via multicluster-runtime provider by default; a
  pull-mode agent variant is possible but out of scope for v1.

### Test plan
- **e2e on kind:** 1 hub + N members (each a kind cluster, labeled to simulate
  GKE/AKS/EKS), members registered as ClusterProfiles. Assert: an instance applied
  on the hub is placed on all matching members; `status.clusters[]` aggregates;
  updating the instance converges all members; deleting it (and removing a member
  from the selector) cleans up with no orphans. **Reuse `kro-genaiops-demo`'s
  `GenAIService` as the sample workload.**
- Unit: placement resolution, applied-manifest tracking, finalizer teardown,
  tolerance/rollup logic.
- Scale/soak: reconcile latency and watch load vs. cluster count (validate the
  "cluster size only bounds your biggest single workload; fleet scales by adding
  clusters" thesis).

## Alternatives considered
1. **Compose KRO + KubeFleet/OCM externally (no KRO change).** Works today, but the
   platform must stitch a KRO graph to a separate placement CRD and there is no
   unified authoring surface or status rollup — the DX this KEP is about.
2. **Karmada `PropagationPolicy`.** A capable, separate propagation layer; heavier
   operational surface and not KRO-native DX. A Karmada-backed multicluster-runtime
   provider could be *an implementation option* under this API.
3. **Argo CD ApplicationSets (cluster generator).** GitOps distribution of manifests;
   excellent for delivery, but not reconciled-graph semantics or aggregated
   object-level status.
4. **Do nothing / keep KRO single-cluster.** Users keep applying per cluster; the
   fleet operating model gap remains.

The through-line: this KEP is deliberately **thin** — it adds an API surface and
reuses (1) SIG-Multicluster inventory and (2) multicluster-runtime. If it starts
reimplementing propagation, it has become Karmada and should be reconsidered.

## Risks and mitigations
| Risk | Mitigation |
|---|---|
| Reinventing a propagation engine | Build strictly on multicluster-runtime + ClusterProfile; keep placement label-only in v1 |
| Hub as SPOF / credential blast radius | HA hub, least-privilege per-member RBAC, per-RGD placement scoping via policy |
| Divergence from SIG-Multicluster direction | Co-develop in SIG-Multicluster; track KEP-4322 graduation |
| Scale: watching many clusters | Lean on multicluster-runtime provider lifecycle; soak tests; shard by RGD if needed |
| API forward-compat | v1 selector-only, with `placement.strategy` reserved for future scheduling |

## Graduation criteria (phased)
- **Alpha:** multi-cluster mode behind a feature gate; label-selector placement;
  push model; kind e2e green; ClusterProfile + multicluster-runtime integration.
- **Beta:** status aggregation hardened; finalizer/GC soak-tested; policy scoping of
  targetable clusters; docs + reference demo across ≥2 real clouds.
- **GA:** aligned with ClusterProfile/accessProviders GA; scale targets published;
  optional pull-mode; placement strategy extensions.

## References
- KRO — https://github.com/kubernetes-sigs/kro
- Reference single-cluster demo — https://github.com/danbruno101/kro-genaiops-demo
- KEP-4322 Cluster Inventory / ClusterProfile —
  https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/4322-cluster-inventory
- ClusterProfile API overview — https://multicluster.sigs.k8s.io/concepts/cluster-profile-api/
- cluster-inventory-api — https://github.com/kubernetes-sigs/cluster-inventory-api
- multicluster-runtime — https://github.com/kubernetes-sigs/multicluster-runtime
- Fleet-scale operating model (inspiration) — https://lucy.sh/fleet-scale-kubernetes