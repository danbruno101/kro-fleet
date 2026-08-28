# KEP: Native Multi-Cluster Mode for KRO

| | |
|---|---|
| **Status** | Provisional / Draft (for discussion) — **v2** |
| **Owning group** | SIG-Multicluster + KRO maintainers (`kubernetes-sigs/kro`) |
| **Stakeholders** | SIG Cloud Provider (cross-cloud portability), SIG Apps |
| **Authors** | @danbruno101 (+ TBD) |
| **Created** | 2026-07-02 · **revised** 2026-08-27 |
| **Depends on** | KEP-4322 Cluster Inventory / ClusterProfile · `sigs.k8s.io/multicluster-runtime` |
| **Companion** | `docs/design/fleet-scoped-kro.md` — problem framing, requirements and open questions for SIG-Multicluster |

---

## What changed in v2

v1 of this KEP proposed placement as an inline label selector and replication of
the whole graph to each selected member. Testing that model against three
concrete use cases — **capacity**, **compliance** and **failover** (see the
companion design doc) — showed it addresses none of them *directly*, and that
trying to make it address them directly turns KRO into a scheduler, which this
KEP explicitly refuses to be.

v2 keeps the entire v1 mechanism and adds two affordances so that the layers
which *do* own those problems can drive KRO:

  - **`placement.decisionRef`** — placement may be a decision computed by
    something else (a scheduler, a policy engine, a failover controller) rather
    than an inline selector.
  - **Per-member parameters** — a decision may carry per-cluster values that the
    graph can reference, which is what distinguishes *division* from
    *replication*.

Everything else — ClusterProfile inventory, multicluster-runtime, hub-side
applied-manifest tracking, finalizer-driven GC, status aggregation — is unchanged
and already implemented in the PoC.

## Summary

KRO (Kube Resource Orchestrator) lets a platform team define a custom API — a
`ResourceGraphDefinition` (RGD) — that expands a short, developer-facing instance
into a reconciled graph of Kubernetes resources, with **no custom Go controller**.
Today that graph is owned and reconciled **within a single cluster**.

There are two layers here, and only one of them exists across clusters today.
**Composition** knows that a set of resources is one unit and how they relate —
KRO derives that from the CEL references between templates and publishes it.
**Placement** decides where things go. SIG-Multicluster has built substantial
placement and delivery machinery; nothing carries composition knowledge across
the boundary.

This KEP proposes an **optional, opt-in multi-cluster mode** for KRO: a platform
team attaches `placement` to an RGD (or to a platform-owned instance), and a
KRO control plane running on a **hub** cluster reconciles the resource graph **into
each member cluster** selected from a **ClusterProfile** inventory, aggregating
per-member status back onto the hub object. **One object, authored and applied once
on the hub, is dispersed to the appropriate clusters across clouds.**

Crucially, this is built **on existing SIG-Multicluster standards** —
`ClusterProfile` (KEP-4322) for the fleet inventory and credentials, and
`multicluster-runtime` for cross-cluster reconciliation — **not** a new propagation
engine. KRO contributes the *authoring surface* and *graph semantics*; the
multi-cluster substrate is reused.

Equally crucially, KRO does **not** decide where things go. It consumes a
placement decision and converges on it. That single property is what lets one
thin API serve capacity, compliance and failover without KRO owning any of them.

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

### The three use cases this has to be able to serve

These are the cases that drove v2. KRO does not solve any of them itself; the
test is whether the API lets the layer that *does* own each one drive KRO.

| Case | What it needs | Who owns it |
|---|---|---|
| **Capacity** — 8 GPU replicas, no single cluster has 8 free | Which clusters **and how many each** | A scheduler / capacity estimator |
| **Compliance** — must stay in a FedRAMP region, never spill | A constrained candidate set, and a **terminal, auditable refusal** when nothing qualifies | A policy engine |
| **Failover** — a member goes unhealthy or is drained | The decision **recomputed** on cluster-state change | A failover/drain controller |

All three are the same statement: *placement is a function of cluster state and
policy, recomputed when either changes.* An inline, human-authored label selector
cannot be driven by a scheduler, a policy engine or a failover controller — which
is why v2 adds `decisionRef`.

Capacity is the only one of the three that also needs **per-member parameters**
(how many replicas each cluster gets). Compliance and failover need only the
cluster set. This is the line between *replication* (v1) and *division*, and it
is called out explicitly so the choice is deliberate.

### Goals
- An **opt-in** `placement` concept on an RGD/instance that selects member clusters
  from a `ClusterProfile` inventory, **either** by label selector **or** by
  reference to a decision computed elsewhere.
- Placement is an **input KRO consumes**, never something KRO computes. Serving
  capacity, compliance and failover is achieved by being drivable, not by
  absorbing those responsibilities.
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
- **Any scheduler.** Not bin-packing, not capacity estimation, not spread
  constraints, not cost optimisation — in v1 or later. KRO consumes decisions.
  If this KEP ever starts computing them, it has become Karmada and should be
  reconsidered.
- **Policy evaluation.** Deciding whether a placement is *permitted* belongs to a
  policy engine. KRO's obligation is to refuse cleanly and record why.
- **Data movement.** If a workload's state must move between clusters
  (a PVC cannot follow a Deployment across a boundary), that is a data-plane
  problem, out of scope here.
- **Splitting one graph across members.** v1 and v2 replicate the whole graph to
  each selected member. Placing *different parts* of one unit in different
  clusters is a materially larger change — it introduces co-location constraints
  and distributed ordering — and is deliberately deferred. See the companion
  design doc, "Approach 2: placement groups".
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
    # Exactly one source. Both resolve to the same thing: a set of members
    # (optionally with per-member parameters).
    #
    # (a) v1: a selector the platform team authors by hand.
    clusterSelector:
      matchLabels: { tier: prod }
    #
    # (b) v2: a decision computed by someone else — a scheduler, a policy
    #     engine, a failover controller. KRO does not care which; it converges
    #     on whatever the decision says, and re-converges when it changes.
    # decisionRef:
    #   apiVersion: <group>/<version>   # e.g. a future portable decision API,
    #   kind: <Kind>                    # OCM PlacementDecision, or a local CRD
    #   name: sentiment-api-placement
    #
    # Optional: how KRO treats partial failure for the rolled-up status.
    tolerance: { minReadyClusters: 1 }
  schema: { ... }              # unchanged (the GenAIService developer API)
  resources:
    - id: deployment
      placement: members       # per-resource: members | hub  (default: members
      template:                #   when spec.placement is set, else hub)
        spec:
          # Per-member parameters, when the decision supplies them. This is what
          # turns replication into division; absent, every member gets an
          # identical copy (v1 behaviour).
          replicas: ${placement.thisCluster.parameters.replicas ?? schema.spec.replicas}
    - id: fleetStatus
      placement: hub           # e.g. an aggregation/rollup object kept on the hub
      template: { ... }
```

The decision object itself is deliberately *not* specified here — that is the
open question this KEP puts to SIG-Multicluster (see the companion design doc).
Whatever shape it takes, KRO needs only two things from it: an ordered set of
member identities that resolve to `ClusterProfile`s, and an optional per-member
parameter map. Shape sketch:

```yaml
# illustrative only — the point is the shape KRO consumes, not the API
clusters:
  - name: gke-prod-1          # -> ClusterProfile
    parameters: { replicas: 3 }
  - name: eks-prod-2
    parameters: { replicas: 5 }
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
- **Placement:** a label selector over ClusterProfiles **or** a referenced
  decision, evaluated continuously; membership changes trigger place/unplace.
  Either way the resolution step produces the same internal value — an ordered
  set of members with optional per-member parameters — so the two sources are
  interchangeable to everything downstream.

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

### Refusing to place, and recording why
Distinct from "not ready yet": if placement resolves to **no** eligible members —
no cluster matches the selector, or the referenced decision is empty because a
policy engine refused — the instance must reach a **terminal, explicit** state
(`Placed=False` with a machine-readable reason) rather than sitting Pending or,
worse, falling back to some broader set. There is no such thing as a graceful
fallback for a compliance constraint.

KRO also records the resolved decision in status — which members were chosen and,
where the decision supplies it, the justification — so that a regulated user can
demonstrate after the fact where a workload ran and why it was permitted. KRO
does not evaluate the policy; it records the decision it was given and refuses
cleanly when that decision is empty.

### Per-member parameters
When the resolved placement carries per-member parameters, they are exposed to
the graph for that member's expansion (sketched above as
`${placement.thisCluster.parameters.*}`). Absent parameters, every member gets an
identical copy — v1 behaviour, unchanged.

Two constraints worth stating: parameters must not be able to alter the *shape*
of the graph (only values), or per-member graphs diverge and status aggregation
stops being meaningful; and the applied-manifest inventory must be keyed per
`(instance, member)` by GVK/name, not derived from an assumed-identical graph.
The PoC currently takes the shortcut of letting `status.clusters[]` double as the
inventory, which is only valid while every member receives exactly one identical
object — see `docs/KEP-GAP.md`.

### Placement & rescheduling
v1: static label-selector placement, re-evaluated on ClusterProfile inventory
changes.

v2 adds `decisionRef`, which changes *where the decision comes from* but not what
KRO does with it: resolve to a member set, converge, re-converge when the
resolved value changes. KRO watches the referenced decision exactly as it watches
the inventory.

Note what this does and does not buy. Capacity-aware scheduling, spread
constraints and drain/evacuate remain **non-goals for KRO** — permanently, not
just for v1. They become *reachable* because an external producer can express
them in a decision KRO consumes. The distinction matters: KRO must never rank,
score, estimate, or reconcile toward a target it computed itself. If a future
contributor finds themselves writing a scoring function inside KRO, this KEP has
been misread.

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
- Unit: placement resolution (both sources), applied-manifest tracking, finalizer
  teardown, tolerance/rollup logic, per-member parameter substitution.
- Scale/soak: reconcile latency and watch load vs. cluster count (validate the
  "cluster size only bounds your biggest single workload; fleet scales by adding
  clusters" thesis).

#### Proving the three use cases without building a scheduler

The point of `decisionRef` is that each case is demonstrated by an **external,
deliberately trivial decision producer** — none of which is part of KRO. If KRO
converges correctly on all three without knowing which produced the decision,
the "drivable, not a scheduler" thesis is demonstrated rather than asserted.

| Scenario | Producer (~100 LOC, outside KRO) | Assertion |
|---|---|---|
| **Capacity** | Reads member allocatable, emits per-member `replicas` summing to the requested total | Members receive *different* replica counts; the sum matches; rebalancing on capacity change converges |
| **Compliance** | Filters candidates to those with a compliance property; emits an empty decision when none qualify | Placement occurs **only** on qualifying members; an empty decision yields terminal `Placed=False` with a reason and **no** fallback placement; the decision is recorded in status |
| **Failover** | Watches ClusterProfile `ControlPlaneHealthy`; rewrites the decision when a member goes unhealthy | Workload is unplaced from the failed member and placed on a standby; teardown of the failed member is retried and does not orphan when it returns |

Each producer should be replaceable by a real implementation (OCM Placement,
Karmada, a policy engine) without changing KRO — that substitutability is the
property under test.

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
| Reinventing a propagation engine | Build strictly on multicluster-runtime + ClusterProfile; KRO consumes decisions and never computes them |
| `decisionRef` becomes a scheduler by the back door | KRO reads a decision and converges. It must never rank, score, estimate or reconcile *toward* a target — those verbs belong to the producer |
| No portable decision API exists yet | Ship `clusterSelector` as the working default; `decisionRef` is additive and can point at an implementation-specific CRD until (or unless) a standard appears |
| Per-member parameters fragment the graph | Parameters may alter values only, never graph shape; applied-manifest inventory keyed per `(instance, member)` by GVK/name |
| Hub as SPOF / credential blast radius | HA hub, least-privilege per-member RBAC, per-RGD placement scoping via policy |
| Push model unusable in accredited environments | Pull-mode agent variant kept explicitly on the roadmap; delivery kept behind the multicluster-runtime provider seam so it is swappable |
| Divergence from SIG-Multicluster direction | Co-develop in SIG-Multicluster; track KEP-4322 graduation; companion design doc raises the open questions before implementation hardens |
| Scale: watching many clusters | Lean on multicluster-runtime provider lifecycle; soak tests; shard by RGD if needed |

## Graduation criteria (phased)
- **Alpha:** multi-cluster mode behind a feature gate; label-selector placement;
  push model; kind e2e green; ClusterProfile + multicluster-runtime integration.
- **Alpha+ (v2 scope):** `decisionRef` and per-member parameters; the three
  reference decision producers (capacity / compliance / failover) with an e2e
  scenario each; terminal `Placed=False` on an empty decision; resolved decision
  recorded in status.
- **Beta:** status aggregation hardened; finalizer/GC soak-tested; applied-manifest
  inventory keyed per `(instance, member)`; policy scoping of targetable clusters;
  docs + reference demo across ≥2 real clouds.
- **GA:** aligned with ClusterProfile/accessProviders GA; scale targets published;
  optional pull-mode; alignment with whatever portable decision API (if any) SIG
  Multicluster settles on.

## References
- KRO — https://github.com/kubernetes-sigs/kro
- Reference single-cluster demo — https://github.com/danbruno101/kro-genaiops-demo
- KEP-4322 Cluster Inventory / ClusterProfile —
  https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/4322-cluster-inventory
- ClusterProfile API overview — https://multicluster.sigs.k8s.io/concepts/cluster-profile-api/
- cluster-inventory-api — https://github.com/kubernetes-sigs/cluster-inventory-api
- multicluster-runtime — https://github.com/kubernetes-sigs/multicluster-runtime
- Fleet-scale operating model (inspiration) — https://lucy.sh/fleet-scale-kubernetes