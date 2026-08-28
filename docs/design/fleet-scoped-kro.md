# Fleet-scoped kro

Firstly note that this document is the *problem framing*. The mechanism proposal
is [KEP-kro-multicluster](../proposals/KEP-kro-multicluster.md) in this same
repository, and a **working PoC of it already exists here**: a hub placement
controller built on ClusterProfile (KEP-4322) and multicluster-runtime, hub-side
applied-manifest tracking with finalizer-driven teardown, per-member status
aggregation, e2e on kind running in CI, and a Headlamp fleet view. `docs/KEP-GAP.md`
is an honest ledger of where the PoC diverges from the proposal.

So this is not a greenfield ask. What it *is*: the KEP answers **how** for one
model (replicate the graph to selected members); this document asks **what and
why**, and surfaces three use cases the original KEP addresses only indirectly.
Working through them is what produced KEP v2's `decisionRef` — the single
affordance that lets a scheduler, a policy engine or a failover controller drive
KRO without KRO becoming any of them.

It is deliberately not a proposal in the sense that matters here: the goal is to
establish whether this group agrees the problem is real and where the layer
belongs. Nothing here should block MCS-API, ClusterProfile or About API work; if
anything it consumes all three.

Written from SIG Cloud Provider, following the kro demo shown to that group.
Please add commentary inline.

Note also that there is separate prototype work in the kro community on an
adjacent piece — [manifold](https://github.com/a-hilaly/k8s-multi-cluster), an
aggregated API server that projects remote cluster resources onto a hub
(Approach 5 below). It is deliberately included here rather than worked around:
part of the point of this document is to get that work in front of this group
early rather than let conventions harden without SIG input.

# Problem statement

## What exists in one cluster

[kro](https://github.com/kubernetes-sigs/kro) (Kube Resource Orchestrator,
`v1alpha1`) lets a platform team define a `ResourceGraphDefinition`: a schema
that becomes a real generated CRD, plus a set of resource templates. A developer
creates one instance of that generated kind, and kro expands it into real
Kubernetes objects. Crucially the ordering between those objects is **derived,
not declared** — kro reads the CEL references between templates, computes the
DAG, and publishes it (`status.topologicalOrder`,
`status.resources[].dependencies`).

The property that matters here is not the templating. It is that after
expansion, **kro knows those N objects are one unit**: one owner, one readiness
object, one delete, one place where environment branching lives.

## The gap

That knowledge stops at the cluster boundary. A kro instance is a single-cluster
object, and every resource it expands into lands in the cluster the instance
lives in.

There are two distinct layers, and only one of them exists across clusters
today:

  - **Composition** knows that a set of resources is one unit, and how they
    relate.
  - **Placement** decides where things go.

SIG Multicluster has built substantial placement and delivery machinery —
ClusterProfile for inventory, About API / ClusterProperty for cluster
attributes, Work API for delivery, and placement decisions in the wider
ecosystem (OCM `PlacementDecision`, Karmada `ResourceBinding`). **Nothing carries
composition knowledge across the boundary.**

To be explicit, because this is the most likely misreading: the claim is *not*
that multicluster workload delivery is unsolved. Karmada, OCM, Fleet and Argo
all propagate workloads to many clusters, and do it well. They propagate
manifests that were already written. They do not compose a dependency graph from
references, and they do not produce a single object whose status means "this
application is ready".

## Why this matters in practice

Three shapes keep recurring in SIG Cloud Provider conversations. They are not
feature requests; they are the cases that made the boundary visible.

  - **Capacity.** A model server needs eight GPU replicas and no single cluster
    has eight free. Today the workload is split by hand into per-cluster
    deployments that then drift apart.
  - **Compliance.** Regulated data may only run in a FedRAMP region. Note this
    *inverts* the capacity case: spilling over to a cluster with room is not a
    graceful fallback, it is a violation. Hard constraint, must fail closed.
  - **Failover.** A cluster goes unhealthy, is drained for maintenance, or is
    consolidated away for cost. Everything on it has to end up somewhere still
    valid — together, and in dependency order.

Efficiency framings (bin-packing for utilisation, spot capacity, cheaper
clusters) reduce to these same three shapes with different triggers, so they are
not listed separately.

All three are the same underlying statement: *placement is a function of cluster
state and policy, recomputed when either changes* — while the composed unit has
to survive being placed, moved, or refused.

## Why today's answers don't close it

  - **Propagation tools** (Karmada, OCM, Fleet, Argo) place and deliver, but the
    unit they move is a manifest set someone else assembled.
  - **Per-resource CRD models** (Crossplane, Config Connector, ACK) reconcile
    each cloud resource independently and continuously — correct, but with no
    notion of a group. Ordering is left to retry-until-ready, which works for
    compute and does not work where a missing dependency loses data rather than
    degrading.
  - **Flux `dependsOn` + health checks** genuinely provides ordering today, at
    Kustomization granularity. But the DAG is hand-authored and kept in sync by
    people, grouping lives in folder layout rather than in the API, nothing
    answers "is my app ready" as one object, and it says nothing about teardown
    order.

# Requirements

What any solution has to do, whether or not kro is the vehicle.

  - **R1.** Express "these resources are one unit" such that it survives crossing
    a cluster boundary — ordering, readiness and teardown included.
  - **R2.** Consume SIG-MC primitives rather than inventing parallel ones:
    ClusterProfile for inventory, About API / ClusterProperty for selection
    predicates, and (probably) Work API for delivery.
  - **R3.** Leave the developer-facing object unchanged. A fleet-scoped instance
    should look like today's instance; cluster names must not leak into the
    developer's spec.
  - **R4.** Treat placement as an **input** the composition layer consumes, never
    something it computes.
  - **R5.** Support hard, fail-closed constraints as a first-class case.
    "No compliant cluster has capacity" must be a terminal, auditable condition —
    never a fallback to a non-compliant cluster.
  - **R6.** Keep the delivery mechanism pluggable. In some accredited
    environments a hub holding credentials into member clusters is not permitted,
    so push-based delivery cannot be assumed (see Issues).
  - **R7.** Degrade honestly under partial failure. A half-placed unit must be
    visible as such, with per-cluster detail, rather than reporting healthy.

## Out of scope

  - **Building a scheduler.** Deciding *where* things go based on capacity is
    Karmada / OCM / Kueue territory. Re-implementing it would duplicate mature
    work and explode the scope. We want to consume a decision, not produce one.
  - **Data movement.** If a workload's state must move between clusters, that is
    a data-plane problem (backup/restore, storage replication), not this layer's.
  - **Cross-cluster networking.** Connectivity for a spanning unit should come
    from MCS-API (ServiceExport/ServiceImport), not from anything new here.
  - **Replacing GitOps.** Flux/Argo still deliver. The question is only whether
    what they deliver can be one object instead of N.

# Desired outcomes

What should be observably true if this works.

  - A developer submits the **same** instance regardless of how many clusters
    are involved, and never names a cluster, a region, or a storage class.
  - A platform team expresses "this must stay in a FedRAMP region", "this needs
    GPUs", or "this cluster is draining" **once**, as policy over cluster
    properties — not as N hand-maintained per-cluster variants.
  - **One object answers "is my application ready"** across the fleet, with
    per-cluster detail underneath it — something a rollout can actually gate on.
  - **One delete reclaims the whole unit**, in dependency order, across every
    cluster it touched — and orphans are surfaced rather than silently left.
  - Placement decisions and the properties that justified them are **recorded in
    status**, so a regulated user can prove after the fact where a workload ran
    and why it was allowed.
  - A compliance violation is **impossible by construction** rather than
    prevented by convention: when no cluster qualifies, the instance reports a
    terminal, explicit refusal with the reason recorded — never "not ready
    yet", never a fallback to a broader set.
  - SIG-MC primitives are **consumed, not duplicated** — no kro-specific cluster
    inventory, no kro-specific cluster property vocabulary.

# Design

Five shapes, roughly in order of increasing ambition. **Do not read too much
into the YAML — the target is to answer the design questions below first.**

They are not mutually exclusive, and they are not all the same kind of thing.
1–3 are **semantics**: what a composed unit means once it can span clusters.
4–5 are **transports**: how a resource actually gets created in another cluster.
A real design is one of 1–3 sitting on one of 4–5. Approaches 1 and 3 also
compose with each other.

## Summary

| # | Approach | Layer | Pros | Cons |
|---|---|---|---|---|
| 1 | Whole-instance placement — **specified in the KEP, PoC built** | Semantics | Simplest possible semantics; no new composition concepts; the graph never spans a boundary; largely serves compliance (R5); already implemented incl. cross-cluster GC | Replication is not division, so the capacity case is unserved; coarse — a unit that fits nowhere simply fails |
| 2 | Placement groups | Semantics | Serves the capacity/GPU case; encodes the real co-location constraint (a Deployment and its PVC cannot be separated); Pod analogy is familiar | Largest new API surface; cross-group references become distributed dataflow; ordering becomes distributed; partial failure becomes a first-class state |
| 3 | Cross-cluster `externalRef` | Semantics | Small, reuses an existing kro concept; probably covers a large share of real "the thing is over there" cases | Read-only — creates nothing remotely, so it does not serve capacity or drain; every reference is potential data movement across a jurisdiction (governance surface) |
| 4 | Work API delivery | Transport | Best ecosystem fit; least new machinery; kro stays a graph engine; supports a pull model, so R6 comes nearly free | Status returns summarised and asynchronous, which may not be enough to drive readiness aggregation; async references constrain what CEL can express |
| 5 | API projection (manifold) | Transport | Exists today and is fast; needs no changes to kro; every existing tool works unchanged; kro can type-check CEL against remote schemas | Hub sits on the request path and holds credentials for every member (fails R6); ownership/GC does not cross the boundary, so teardown is unsolved; no placement at all (R4); defines its own cluster identity and API-group naming (tension with R2) |

The honest summary of the table: **no single row is sufficient.** The transports
solve reachability and say nothing about policy or lifecycle; the semantics rows
assume a transport exists. That split is itself an argument for the design
questions below being the right place to start.

(One transport is deliberately absent from the table because it is neither a
proposal nor adjacent work — it is what the PoC *already uses*:
**multicluster-runtime** with a ClusterProfile provider. It is compared against
Approaches 4 and 5 under the transport design question below.)

## Approach 1: whole-instance placement

The instance is atomic. It lands in exactly one cluster (or is replicated
wholesale to N); placement chooses which.

```yaml
apiVersion: kro.run/v1alpha1
kind: GenAIService
metadata:
  name: sentiment-api
spec:
  name: sentiment-api
  replicas: 1
placement:                      # or a sibling object? see design questions
  clusterSelector:
    matchProperties:
      compliance.example.com/fedramp: high
      region.topology.k8s.io: us-gov-west-1
```

Simplest possible semantics: no new composition concepts, the graph never spans
a boundary, and the compliance case (R5) is largely served. Does nothing for the
capacity case, because replicating a graph to N members is not the same as
dividing it across them.

**This is the approach specified in KEP-kro-multicluster and implemented in this
repository's PoC** — including the parts that are genuinely hard: credentials via
ClusterProfile `accessProviders`, hub-side applied-manifest tracking, and
finalizer-driven teardown with no orphans. KEP v2 extends it with `decisionRef`
(placement supplied by an external producer) and per-member parameters, which is
what makes capacity reachable without KRO owning a scheduler.

## Approach 2: placement groups inside one instance

Resources are grouped; groups are placed; edges *within* a group are guaranteed
co-located, edges *across* groups must be connectivity-only.

This is deliberately the Pod analogy. A Pod is the atomic scheduling unit
precisely because its containers must share a node. kro would need the
fleet-level equivalent, because some edges in a resource graph cannot be cut — a
Deployment cannot mount a PVC in another cluster (see Issues).

```yaml
# in the ResourceGraphDefinition
spec:
  placementGroups:
    - id: serving              # co-located: Deployment + its PVC + SA + ConfigMap
      resources: [deployment, cache, serviceAccount, appConfig]
    - id: frontend
      resources: [api, ingress]
  resources:
    - id: deployment
      ...
```

This is what the GPU case actually needs: put the serving group where the
accelerators are, everything else elsewhere. Costs: cross-group references
become distributed dataflow, ordering becomes distributed, and partial failure
becomes a first-class state.

## Approach 3: cross-cluster externalRef (read, don't create)

kro already has `externalRef` — a read-only reference to an object it does not
own. Allow it to name another cluster.

```yaml
  resources:
    - id: platformConfig
      externalRef:
        apiVersion: v1
        kind: ConfigMap
        metadata: {name: genaiops-platform-config, namespace: platform}
        clusterRef:                     # new
          name: hub                     # ClusterProfile name
```

Far smaller than Approach 2, and probably covers a good fraction of real "the
thing is over there" cases: a platform config on a hub, a model registry, an
existing managed database. It is also where the data-residency question bites
hardest (see design questions).

## Approach 4: delegate delivery to the Work API

kro resolves the graph but does not apply remotely; it emits Work objects that
existing fleet agents deliver, and reads status back.

```yaml
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: Work
metadata:
  name: sentiment-api-serving
  # ownerReferences -> the GenAIService instance
spec:
  workload:
    manifests: [ ... the expanded, ordered resources destined for one member ... ]
```

Best ecosystem fit and least new machinery; kro stays a graph engine and R6 comes
almost for free. Open question is whether status fidelity coming back through
Work is sufficient to drive readiness aggregation and CEL references.

## Approach 5: API projection (existing work — manifold)

Worth calling out separately because it already exists, in-family:
[manifold](https://github.com/a-hilaly/k8s-multi-cluster) (kro maintainer
a-hilaly) registers an aggregated API server on a hub so that remote cluster
resources are addressable as native objects — `kubectl get pods.cluster-1`,
`deployments.apps.cluster-1.k8s.io` — rewriting URLs, JSON, protobuf, watch
streams and discovery bidirectionally. A `ClusterImport` CR (itself an RGD)
reconciles into the proxy Deployment plus `APIService` registrations.

```yaml
apiVersion: kro.run/v1alpha1
kind: ClusterImport
metadata:
  name: cluster-1
spec:
  cluster:
    name: cluster-1
    host: "cluster-1-control-plane"
    kubeconfigSecret: imported-kubeconfig
  imports:
    - group: "apps"
    - group: ""
```

Its README explicitly frames this as an enabler for cross-cluster composition:
because discovery and OpenAPI are projected too, kro can type-check CEL against
imported schemas, so an RGD on the hub can reference a remote resource by its
projected group and "just work".

This is the cheapest possible answer to R1 in mechanical terms — it needs *no*
changes to kro. It is also the approach with the most unanswered semantics, and
it is where alignment with this group matters most:

  - It defines cluster identity inline (`spec.cluster.{name,host,port,
    kubeconfigSecret}`) rather than referencing ClusterProfile — a parallel
    cluster abstraction, which R2 argues against.
  - It introduces a synthetic API group naming convention
    (`<group>.<cluster>.<domain>`) with ecosystem-wide consequences for kubectl
    UX, RBAC and discovery. That convention probably wants review from this group
    and SIG API Machinery before it becomes de facto.
  - It is orthogonal to placement (R4): it tells you how to *reach* cluster-1,
    never which cluster should host a workload. Capacity, compliance and drain
    are untouched.
  - Garbage collection does not cross the boundary. Kubernetes ownership is
    per-cluster, and manifold strips `ownerReferences` by default (necessary —
    otherwise hub GC would act on remote objects). So "one delete reclaims the
    whole unit" is *not* obtained for free; teardown remains an open problem.
  - The hub holds credentials for every member, which is the push model that is
    frequently not permitted in accredited environments (R6).

None of that is a criticism of the implementation, which is careful and fast. It
is an argument that the semantics layer this document is about is still needed
on top of, or instead of, a projection layer — and that the projection layer's
own conventions deserve SIG input.

## Design questions

These are what we would most like direction on.

### Is a single instance spanning multiple clusters even the right model?

Or should a spanning application be several coordinated instances under a parent
object? Concretely: is one object whose resources live in several clusters
consistent with how this group thinks about clustersets and **namespace
sameness**, or does it violate an assumption we have not spotted?

Biggest fork in the road, hence first — it changes everything downstream.

### Where does the placement decision come from?

Per R4, kro should consume a decision rather than compute one. That implies
something like a portable placement-decision resource: the *output* side of
scheduling, the way ClusterProfile standardises the *inventory* side.

OCM has `PlacementDecision`, Karmada has `ResourceBinding`; nothing is portable.
**Is there appetite in this group for standardising that?** If yes, capacity,
compliance and failover all collapse into one input and kro needs no scheduler.

This is the single most valuable thing we could get from this group, and it is
concrete rather than hypothetical: KEP v2 already reserves
`placement.decisionRef` for exactly this, and the plan is to demonstrate all
three use cases with three deliberately trivial external decision producers —
none of them part of kro — so that "kro is drivable, not a scheduler" is
demonstrated rather than asserted. If a portable decision object exists, those
producers are replaced by real implementations and nothing in kro changes. If
this group would rather it stay implementation-specific, `decisionRef` simply
points at a local CRD and we lose portability, not function.

  - Encouragingly, the MCS cluster-selection design doc independently reaches for
    the same idea in its "Placement Decision Style" section (suggested by Mike
    NG), which suggests this is a shared gap rather than a kro-specific wish.
  - Note that Approach 5 (manifold) deliberately does not answer this: naming a
    cluster in a `ClusterImport` is addressing, not placement. Whatever we build,
    something still has to decide *which* cluster.

### What is the atomic unit of placement — instance, group, or resource?

Per-resource is most flexible and, we think, wrong: some edges cannot be cut.
Whole-instance is simplest and does not serve the GPU case. Groups (Approach 2)
are the middle. Is the Pod analogy a good one here, or misleading?

### Should cross-cluster CEL references be allowed at all?

In one cluster, `${platformConfig.data.endpoint}` is a template reference. Across
clusters it is an **export of data from one jurisdiction into another** — a
Secret referenced from a US cluster into an EU cluster has moved that secret.

Proposal for a first iteration: allow hub→member and within-cluster references,
and require an explicit `export` declaration for anything crossing a boundary.
Unrestricted cross-cluster references are effectively arbitrary distributed
joins, and in a regulated context they are also a governance surface. Too
restrictive to be useful?

### What does "Ready" mean for a spanning instance? — *we have an answer, does it hold?*

Three of five clusters converged — is the instance Ready? **Our proposed answer,
implemented in the PoC:** per-cluster entries in `status.clusters[]` plus an
explicit tolerance (`minReadyClusters`) folding into the rolled-up `Ready`
condition. (The KEP additionally specifies that members unreachable beyond a
grace period surface `Degraded`/`Unknown` rather than blocking the object; the
PoC currently just marks them not-ready with a reason and retries — one of the
gaps ledgered in `docs/KEP-GAP.md`.)

Open part: whether tolerance should generalise to a policy enum
(`All`/`Any`/`Quorum`/percentage), and whether this matches how this group thinks
about fleet-level status elsewhere.

Separately and more sharply — an **empty** placement (no cluster qualifies, or a
policy engine refused) must be terminal and explicit, never "not ready yet" and
never a fallback to a broader set. That distinction is what makes R5 real.

### How are hard vs soft constraints expressed?

Compliance predicates must be hard filters applied *before* any capacity-based
selection. Is there an existing convention for must-vs-prefer over
ClusterProperty we should adopt rather than invent?

### Should the transport be API projection or Work-style delivery?

There is a third answer we should put on the table because it is what the PoC
actually uses: **multicluster-runtime** with a ClusterProfile provider. The hub
controller starts and stops reconciliation against clusters as they are
discovered, and applies server-side. It is push, like projection, but without
putting the hub on the *request* path for every client — and it reuses an
existing SIG-Multicluster component rather than an aggregation-layer trick.

Approaches 4 and 5 are the other two answers to "how does a resource actually get
created in another cluster", and they have very different properties.

  - **Projection** (Approach 5): synchronous, every existing tool works
    unchanged, kro can type-check CEL against remote schemas. But the hub is on
    the request path, holds credentials for every member, and ownership/GC does
    not cross the boundary.
  - **Work-style delivery** (Approach 4): asynchronous, agent-side apply, works
    with a pull model, status comes back summarised. But references between
    clusters can no longer be resolved synchronously, which constrains what CEL
    can express.

Is there a reason this group would prefer one as *the* multicluster convention,
or should the composition layer treat both as pluggable back ends? Note that
the compliance case (R6) may force pull for some users regardless of preference.

### Who is trusted to assert cluster properties?

If placement respects `compliance/fedramp: high` from a ClusterProperty, a
mislabelled cluster is a compliance breach rather than a scheduling mistake. Is
there a trust or provenance model for About API properties, or is that an open
gap?

## Issues

Known-hard problems, listed so nobody has to find them in review.

### Data gravity is the hardest constraint, and it is not theoretical

A Deployment cannot mount a PVC in another cluster. Full stop. This makes certain
edges non-cuttable and is the main argument for placement *groups* over
per-resource placement.

  - We hit the single-cluster version of this in the demo itself: two replicas
    sharing one ReadWriteOnce PVC work on single-node kind and deadlock with a
    Multi-Attach error the moment replicas land on different nodes of a real
    cluster. The fleet version of that mistake is worse and quieter.
  - Consequence for failover/drain: stateless groups can genuinely be evacuated;
    stateful ones need a data plane and a human decision. An RGD probably has to
    declare which is which.

### Teardown ordering, and the fact that ownership does not cross clusters

"Each component retries until its dependency exists" says nothing about
*deletion* order. Deleting a dataset with live tables, a topic with
subscriptions, or a bucket a function still writes to are each distinct
incidents. kro handles reverse-order teardown within a cluster; across clusters,
an unreachable cluster at delete time means orphans nothing owns.

More fundamentally: **Kubernetes garbage collection is per-cluster.** An owner
object in cluster A cannot own a dependent in cluster B, whatever the transport.
Approach 5 makes this concrete — manifold strips `ownerReferences` from projected
responses by default, because otherwise the hub's GC would act on objects whose
owners it cannot see. So an API projection layer gives you addressability
without giving you ownership.

**Our proposed answer, already implemented in the PoC:** a hub-side
applied-manifest inventory per `(instance, member)` — conceptually OCM's
`AppliedManifestWork` — with a finalizer on the hub instance driving deletion.
On delete, or when a member leaves the placement, exactly the tracked objects are
removed from that member and the record cleared. Within each member, ordinary
`ownerReferences` and SSA field ownership still apply to the local sub-graph.

Two honest caveats, both tracked in `docs/KEP-GAP.md`: the PoC currently lets
`status.clusters[]` double as the inventory, which is valid only while each
member receives exactly one identical object (per-member parameters break that
assumption and require the real per-GVK/name record); and a member whose
ClusterProfile is deleted *before* cleanup is skipped, orphaning its copy on a
now-unreachable cluster.

The question for this group is therefore narrower than "how do we do
cross-cluster GC": is hub-side tracking plus a finalizer the pattern this group
wants standardised, given OCM already does essentially this?

### Push vs pull may be dictated by compliance, not preference

In FedRAMP High / IL5-style environments, a hub outside the accreditation
boundary holding credentials into clusters inside it is frequently not
permissible. That rules out hub-centric remote apply for exactly the users who
most want the compliance case — hence R6.

### Version skew breaks static analysis

kro's graph acceptance is computed against one cluster's API surface. Member
clusters differ, so `GraphAccepted` may need to become per-target-cluster rather
than a single condition.

### Scale

N clusters × M instances of watches and status is the obvious wall. Summarised
per-cluster status is probably required, but that trades away the detail the
composition layer exists to provide.

## Prior art

  - **KubeFed `ReplicaSchedulingPreference`** — total replicas divided across
    clusters with per-cluster min/max. Archived; understanding *why* is directly
    relevant to the capacity case.
  - **Karmada** — divided replica scheduling with a dedicated scheduler-estimator,
    cluster taints, and `GracefulEvictionTask` for drain. Closest existing thing
    to the capacity and failover cases.
  - **OCM** — `ManagedCluster` taints with Placement tolerations (including
    toleration seconds), and `PlacementDecision` as a decision output.
  - **Crossplane / Config Connector / ACK** — the per-resource-CRD model: correct
    about continuous reconciliation, silent about grouping.
  - **Flux `dependsOn` + health checks** — real ordering today at Kustomization
    granularity, hand-authored rather than derived.
  - **manifold / k8s-multi-cluster** (a-hilaly, kro maintainer) — aggregated API
    server projecting remote cluster resources onto a hub as native objects, with
    kro reconciling the import itself. The reason Approach 5 exists. It solves
    addressability; it is silent on placement, ownership/teardown, readiness
    aggregation and policy — which is the argument for a semantics layer
    regardless of transport.
  - **kro-fleet** (this repository) — KEP-kro-multicluster plus a working PoC of
    Approach 1 on ClusterProfile + multicluster-runtime: placement, per-member
    status aggregation, hub-side applied-manifest tracking, finalizer teardown,
    e2e on kind, and a Headlamp fleet view. The closest work to this document
    because it *is* the same effort — this document is its problem framing, and
    `docs/KEP-GAP.md` is the honest ledger of what the PoC does and does not
    prove.

# Key takeaways from the SIG-Multicluster discussion

*(to fill in during/after the meeting)*

# Appendix: what the composition layer looks like today

For anyone who has not seen kro. The platform team writes one RGD; the developer
writes this:

```yaml
apiVersion: kro.run/v1alpha1
kind: GenAIService
metadata:
  name: sentiment-api
spec:
  name: sentiment-api
  model: "Qwen/Qwen2.5-0.5B-Instruct"
  replicas: 1
  monitoring: true
```

kro expands it into a PVC, a Deployment, two Services and the monitoring wiring,
in an order derived from the CEL references between them. The developer named no
cloud, no storage class and no ordering. Demonstrated live on GKE, AKS and EKS:
the same instance resolves `premium-rwo`, `managed-csi` and a kro-created `gp3`
respectively, because each cluster's platform config says what that cluster is.

All of it lands in exactly one cluster. That is the gap this document is about.
