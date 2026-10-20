# KEP-GAP — how this PoC differs from the proposal

An **honest ledger** of where `kro-fleet` (this thin PoC) deliberately diverges from
the ideal design in [`docs/proposals/KEP-kro-multicluster.md`](proposals/KEP-kro-multicluster.md).
Keep this current as the PoC evolves — it exists so reviewers are never misled into
thinking the PoC *is* the finished native design.

> **Pins:** every upstream version this ledger assumes is recorded in
> [`rebaseline-2026-10.md`](rebaseline-2026-10.md) (October 2026) and, for the
> original PoC, [`phase0-validation.md`](phase0-validation.md).

> **One-line framing:** the PoC proves the **API, UX, and SIG-Multicluster
> integration** of fleet-scoped KRO objects — including, since October 2026,
> consuming a standard `PlacementDecision` to *divide* one object across
> clouds, and authoring the `ResourceGraphDefinition` once on the hub and
> distributing it to members (the distribution half of upstream kro's
> KREP-013); it does **not** implement the native, in-kro control loop
> (KREP-013's expand-on-hub). That is intentional and is future work.

## The gaps

| Aspect | KEP (native ideal) | This PoC | Why / follow-up |
|---|---|---|---|
| **Graph expansion** | Inside kro, on the **hub**, one control loop | **Stock kro on each member** expands the placed instance locally | Avoids forking kro; validates the UX + placement first. Native = a change inside `kubernetes-sigs/kro`. |
| **New code surface** | A multi-cluster mode *within* kro | A **separate hub placement controller**; kro unmodified | Keeps the PoC small and reviewable. |
| **Member credentials** | ClusterProfile `status.accessProviders` plugin mechanism (KEP-4322/5339) | **As proposed** (since the Oct 2026 re-baseline): every ClusterProfile advertises `status.accessProviders[]`, and the controller resolves the named provider through an exec credential plugin via the provider's AccessProvider strategy — on kind the upstream `kubeconfig-secretreader` plugin, on the clouds each CLI's exec plugin (`aws eks get-token`, `gke-gcloud-auth-plugin`, `kubelogin`). The controller never reads a kubeconfig Secret itself | Was: the provider's (now deprecated) labeled-Secret strategy. What remains simplified is *who writes* `accessProviders`: the setup scripts, standing in for a cluster-manager agent (next row). |
| **Controller packaging** | The KRO control plane runs in-cluster on the hub | **Host process**: `go run ./cmd/fleet-controller` against the hub kubeconfig; for the cloud demo the hub itself is a kind cluster on the presenter's laptop (`docs/demo-cloud.md`) | PoC convenience only; nothing architectural depends on where the controller runs. |
| **Member health** | A cluster manager agent maintains `status.conditions` (e.g. `ControlPlaneHealthy`) on each ClusterProfile | **Self-asserted**: setup scripts patch `ControlPlaneHealthy=True` at registration; the demo flips it to simulate an outage. The controller enforces the condition on every pass — an unhealthy member is neither written to nor cleaned up until it recovers — because the provider (v0.25.2) consults it only before engaging a member and keeps an engaged member engaged when it later turns unhealthy | kind members have no cluster-manager agent. The gate itself is exercised for real; the provider's engage-time-only check is worth raising upstream (a disengage on unhealthy would make the controller-side gate redundant). |
| **Cluster properties** | Members advertise About API `ClusterProperty` objects (KEP-2149); the cluster manager mirrors them into `ClusterProfile.status.properties` (KEP-4322) | **As proposed, with a demo stand-in for the cluster manager**: `fleet-demo inventory-agent` mirrors each member's `ClusterProperty` objects as-is and computes `gpus-total` / `gpus-free` from node allocatable minus pod requests (publishing them on the member first). The vocabulary is a documented demo namespace under `example.com` (`docs/properties.md`), not a kro vocabulary | A real cluster manager (OCM, KubeFleet, GKE Fleet) replaces the agent without any change to kro-fleet or the scheduler. Property *trust/provenance* is an open question for SIG-Multicluster, not addressed. |
| **Placement source** | v2 adds `placement.decisionRef` — placement supplied by an external producer (scheduler / policy engine / failover controller) alongside the v1 selector | **As proposed** (since the Oct 2026 iteration): `decisionRef` (by name) or `placementKey` (KEP-5313 label discovery, slices merged by `decision-index`) consume a standard `PlacementDecision`; `clusterSelector` remains as the v1 fallback. Exactly one source per instance | The decision object is the SIG-Multicluster standard, not a local CRD. What the standard does not carry is supplied by producer annotations — see `docs/placement-decision-gaps.md`. |
| **Per-member parameters** | A decision may carry per-cluster values the graph references (`${placement.thisCluster.parameters.*}`) — what turns *replication* into *division* | **Built, with a convention**: parameters come from producer-set annotations on the decision (`parameters.fleet.kro.run/<cluster>`), and the hub controller folds them into each member's copy of the template before placing it (today: `replicas`). Values only, never graph shape | The KEP sketches a CEL hook inside kro; this PoC applies the override on the hub because members run stock kro. Same semantics, different seam. The annotation carrier is a gap written up for SIG-Multicluster. |
| **Hard requirements** | A constraint expressed in kro, evaluated by the scheduler, enforced fail-closed by kro | **As proposed**: `spec.placement.requirements.matchProperties` over `ClusterProfile.status.properties`; the demo scheduler honors it as input, and kro-fleet refuses, as a whole, any decision naming a cluster that violates it (`Placed=False/DecisionViolatesRequirements`, nothing placed) | Exact-match only; no must-vs-prefer vocabulary (an open question put to SIG-Multicluster in the design doc). |
| **The decision producer** | Not part of kro: a scheduler, a policy engine, a failover controller | **Demo-only reference scheduler** (`fleet-demo scheduler`): reads the hub inventory, filters on health / draining / requirements, sizes candidates from the advertised total / free accelerators net of its own earlier decisions (quota-style accounting, so a decision stays put while its pods land), policies `spread` / `cheapest-first` / `spot-first` / `bin-pack` (and `manual` to opt an instance out), writes one `PlacementDecision` with the split and a decision-level reason. Deliberately trivial and labeled so; its only job is to exist | Replaceable by OCM (standard-`PlacementDecision` support in progress, ocm#1373), Karmada or Kueue without changing kro-fleet — minus the two annotation conventions. It must never move inside kro. |
| **Empty placement** | Terminal, explicit `Placed=False` with a machine-readable reason; never a fallback to a broader set | **As proposed**: a `Placed` condition with reasons `DecisionPending` (decision absent), `NoEligibleClusters` (decision present and empty, or selector matches nothing — terminal, with the producer's reason), `InsufficientCapacity` (partial decision: the slice is placed, nothing spills), `DecisionViolatesRequirements` (refused as a whole). Nothing placed is never Ready — the explicit-zero-tolerance vacuous success is gone | The pending-vs-refused distinction relies on a convention (absent = pending, empty = refusal, reason via annotation) because the standard has no decision-level reason — see `docs/placement-decision-gaps.md`. |
| **Decision provenance** | Resolved decision + justification recorded in status for audit | **As proposed**: `status.placement` records source, decision slice names, scheduler name, the decision-level reason, and per-member reason + parameters | — |
| **Distribution model** | Design allows push or pull | **Push** (hub → member API via multicluster-runtime) only | Pull-mode agent out of scope for the PoC. The Work API was evaluated for pull mode in Oct 2026 and not adopted — see below. |
| **Status aggregation** | Rolled-up conditions + per-member `status.clusters[]`; members unreachable beyond a grace period surface `Degraded`/`Unknown` rather than blocking the object | Per-member entries (assigned / ready replicas, endpoint, message) + a rollup by **replicas** for divided placements (`tolerance.minReadyReplicas`, default all requested) and by clusters for replicated ones (`tolerance.minReadyClusters`). **No grace period, no `Degraded`/`Unknown`**: an unreachable member is marked not-ready with a reason and the reconcile retries sooner | The KEP's unreachable-member semantics are design, not built. |
| **Cross-cluster GC** | Native ownership/finalizer semantics | Hub-side inventory + finalizer (`fleet.kro.run/cleanup`). A member whose ClusterProfile is deleted **before** cleanup no longer blocks the instance, and is no longer silently skipped: its `AppliedManifestRecord` is kept with `Orphaned=True` as the ledger of what may remain there, and is settled (objects deleted, record removed) when the member registers again | Same idea as OCM `ManifestWork`/`AppliedManifestWork`. A registered-but-unreachable member still blocks finalization and retries. Remaining gap: nothing reaps a record whose member never returns, and the orphaned objects are unreachable in the meantime — surfaced, not solved. |
| **Applied-manifest inventory** | A dedicated per-`(instance, member)` manifest inventory (like OCM `AppliedManifestWork`) | **As proposed** (since the Oct 2026 iteration): one `AppliedManifestRecord` per (instance, member), keyed by GVK/namespace/name — intent written *before* the member is touched, confirmed with UIDs after; unplacement, pruning and finalization delete exactly the recorded objects (label- and UID-guarded). `status.clusters[]` is readiness only | Was: `status.clusters[]` doubling as the inventory, valid only while every member received one identical object. The shared workload namespace is ensured but deliberately not inventoried (the member's `ClusterPlatform` and other instances live there). |
| **Member readiness** | kro-defined instance readiness contract | **Heuristic**: `status.state == ACTIVE`, else a true `Ready`/`InstanceSynced` condition, combined with `status.readyReplicas` (the fleet RGD surfaces it) against the member's assigned replicas | Matches stock kro's instance status; a contract inside kro would replace the heuristic. |
| **Serving across clusters** | Cross-cluster networking is a non-goal (MCS-API territory) | Each member exposes its slice through **its own cloud load balancer**, created by the fleet RGD from the platform ConfigMap (per-cloud annotations; the developer names nothing); the hub collects each member's endpoint into `status.clusters[].endpoint`. The **single entry point** is a demo-only gateway on the hub (`fleet-demo gateway`: smooth weighted round-robin by ready replicas, `X-Fleet-Cluster` on every answer) | **MCS-API (`ServiceExport`/`ServiceImport`) is future work**, deliberately not used in this iteration: the model server replicas are stateless and never talk to each other, so per-cloud ingress plus weighted DNS (or this gateway) is enough for the capacity case. |
| **Definition distribution** | KREP-013 ([kubernetes-sigs/kro#1223](https://github.com/kubernetes-sigs/kro/pull/1223)): RGDs live on the hub only; the hub's kro compiles each revision and distributes the generated CRD to every spoke (opt-out via the `kro.run/distribute` label); spoke instances are reconciled from the hub | **Built — the distribution half only, as a PoC envelope**: a cluster-scoped `FleetResourceGraphDefinition` embeds kro's own `ResourceGraphDefinitionSpec` (imported from kro 0.9.4, never copied) plus `distribution.{enabled, memberSelector}`. The controller server-side-applies a plain `ResourceGraphDefinition` named after the wrapper — spec verbatim, only the placed-by and `fleet.kro.run/spec-hash` labels added — to every registered, healthy member the selector matches, re-applies when the hash changes, and hands every applicable definition to a member on registration. The hub never evaluates a graph: each member's **stock kro** accepts the definition, generates the CRD and expands instances. `distribution.enabled` mirrors KREP-013's label; `memberSelector` is what KREP-013 lists as future work (per-cluster RGD targeting) | The wrapper exists only because the hub runs no kro. If KREP-013 ships hub-side RGDs it collapses into the `kro.run/distribute` label on a plain RGD (and `memberSelector` into whatever per-cluster targeting upstream adopts). What a member receives is byte-for-byte what a platform engineer would have applied by hand. |
| **Per-member definition status** | One verdict on the hub (`GraphAccepted`): KREP-013 compiles once, on the hub, and lists API-server version skew across spokes as out of scope | `status.members[]` on the wrapper: per member the applied hash, the member kro's `status.state`, its `Ready` condition status, reason and message **verbatim**, and its kro version (read off the kro Deployment's version label, best effort) — fed by a watch on the distributed RGD through multicluster-runtime, never polled. `Distributed` = every targeted healthy member holds the current hash; `Accepted` = every targeted member reports Active, else False naming the first failing member with its own reason. A member that cannot be reached reports `MemberNotEngaged` with state and readiness left empty — unknown, not guessed | Member-side validation gives **per-cluster graph acceptance by construction**: the version-skew criterion (e2e 20) shows one member rejecting a revision the other two accept, with that member's own reason on the hub (`InvalidResourceGraph: failed to build resource "skewMarker": … schema not found`) — something a single hub-side verdict cannot express. |
| **Placement gate** | n/a — in KREP-013 instances are reconciled from the hub, so an instance never meets a spoke without its CRD | An instance is placed onto a member only once the definition for its kind reports that member accepted. Otherwise the member stays in `status.clusters[]` with reason `RGDNotReady` and the definition's own reason, `Placed` becomes `False/RGDNotReady` and `Ready` counts it as not ready — never silently skipped. A copy placed earlier whose definition stops being Active there is held as is (observed, not converged) | — |
| **Definition deletion guard** | KREP-013: cross-cluster finalizer GC for composed instances. RGD deletion itself is stock kro's: kro 0.9.4 deletes the generated CRD with the RGD, and with the CRD every instance and its graph | Removing a definition from a member (unplacement or delete) is **refused** while instances of that kind exist on that member — **hub-placed** (the hub's `AppliedManifestRecord`s for that member) or **member-local** (listed on the member itself, uncached, because stock kro accepts an instance created there directly and the hub's ledger cannot see it): `Deleting=False/BlockedByInstances` names the members with both counts (`member-2 (0 hub-placed, 1 member-local instance(s))`), the `fleet.kro.run/cleanup` finalizer holds the wrapper in Terminating, and removal proceeds on its own once the instances are gone. A member that cannot be looked at (not engaged, or the list fails) is never removed from: removal stays pending and is retried. A member deregistered before removal keeps its copy (logged; there is no orphan ledger for definitions) | The PoC's answer to what deleting a spanning *definition* means: never destroy workloads the hub placed. The inventory for definitions is `status.members[]` itself (hash + UID per member), not a record — the record controller would settle a definition's record as the orphan of an instance that never existed. |
| **placementDefaults** | KEP v2: placement attached to the RGD (platform-owned) as well as to a platform instance | **Type defined, not wired**: `spec.placementDefaults` (same shape as an instance's `spec.placement.requirements`) is accepted and validated on the wrapper but not yet consumed by the instance controller | Follow-up: an instance without its own requirements inherits the definition's, the instance's own always win, and the resolved source is recorded in `status.placement`. |
| **Workload portability surface** | The developer instance is unchanged across clusters and modes | **As proposed**: the fleet RGDs (`config/fleet-rgd/`, derived from the sister project's, authored once on the hub) resolve storage class, load-balancer flavor, workload identity annotation, accelerator resource and serving image from each member's `ClusterPlatform`; the developer's `GenAIService` is byte-identical on kind, EKS, GKE, AKS and in simulated vs real-GPU mode | The fleet RGD uses a StatefulSet with per-replica volumes instead of the sister's Deployment + shared RWO PVC — the only way N replicas run on a multi-node cluster while keeping the storage-class story. Candidate for upstreaming to the sister repo. |
| **Scale** | Fleet-scale (many clusters) | A handful of **kind** clusters on a laptop; three real clusters in the cloud demo | PoC proves correctness, not scale. |

## What the PoC *does* faithfully prove

Everything below is asserted by `scripts/e2e.sh` (the same script CI runs), on
kind, against **stock kro 0.9.4** expanding the placed objects on three
members with simulated accelerators.
- One placement-enabled object on a hub, placed onto matching members, with
  aggregated status — the core UX of the KEP (criteria 1–6).
- The definition authored once on the hub too: distributed to every member,
  to a late joiner on registration, narrowed by a selector, switched off,
  updated (every member's copy carries the new hash), gated (no instance
  lands where the member's kro does not serve the kind), guarded (no
  definition removed from under instances, hub-placed or created on the
  member directly) and version-skewed (one member rejects a revision the
  others accept, in its own words) — criteria 13–21.
- Integration with **ClusterProfile** (inventory + `accessProviders`
  credentials + `status.properties`), **About API** properties, a standard
  **PlacementDecision** and **multicluster-runtime** — i.e. the "build on
  SIG-Multicluster, don't reinvent propagation" thesis.
- Clean lifecycle: place → converge → add/remove member → evacuate → delete,
  with no orphaned resources — and, when a member is deregistered before
  cleanup, the orphan is surfaced in its inventory record and settled when the
  member returns (criterion 7).
- The three use cases of the design doc, each driven by the trivial external
  producer and consumed without kro-fleet knowing which:
  - **capacity**: 8 replicas no single member can hold, divided 3/3/2, one
    replica-based Ready, consolidated 4/4/0 by `cheapest-first` (criterion 9);
  - **compliance**: a hard requirement only one member satisfies is placed
    only there, the shortfall is reported, nothing spills; a requirement no
    member satisfies is a terminal refusal (criterion 10);
  - **failover (drain)**: a draining member's replicas move to the others,
    readiness recovers, the drained member's record is gone (criterion 11);
  - plus the consumer-side contract on hand-written decisions: pending,
    violation, partial, empty (criterion 8), and one URL answered from
    several clouds through the members' own load balancers (criterion 12).

## Work API — evaluated, not adopted (October 2026)

`kubernetes-sigs/work-api` was evaluated as a second, pull-mode delivery back
end (R6 in the design doc). It is alive but pre-release (no release or image
ever cut; the Azure fork archived 2026-06-15), and measured on its agent at
master: only the `Applied` condition is set (per manifest and per `Work`;
`Available`/`Progressing`/`Degraded` are documented but never set), and its
finalize controller still carries `// TODO add clean resource logic` —
deleting a `Work` does not remove what it applied. That fails both readiness
aggregation and "every cluster cleaned up with one delete" outright. OCM's
`ManifestWork` (with `feedbackRules`) is the production implementation of
the idea. Decision: keep multicluster-runtime as the one delivery back end;
a pull-mode back end (OCM `ManifestWork`) remains the documented route for
accredited environments. Details and measurements: `docs/rebaseline-2026-10.md`.

## Not attempted (explicitly out of scope for the PoC)
- Any change to `kubernetes-sigs/kro` itself (the native mode).
- **Expand-on-hub** (KREP-013's control loop: the hub's kro compiles
  `GraphRevision`s and reconciles spoke instances from the hub) — the hub
  runs no kro here; members expand locally, which is what makes per-cluster
  acceptance possible.
- **Hub-only composition** (KREP-013's meta-RGD model: child instances
  created on the hub, leaf resources pushed back to the originating spoke)
  — needs expand-on-hub plus KREP-012 targets; nothing here composes across
  clusters.
- **Phased rollout of a definition across members** (canary one member,
  then the rest) — out of scope for KREP-013 as well; every targeted member
  receives the current revision.
- Cross-cluster networking / service discovery (MCS-API) — future work; see
  "Serving across clusters" above.
- **Computing** placement inside kro — capacity estimation, policy
  evaluation, scheduling. kro-fleet consumes decisions; the demo scheduler
  is scaffolding, not a scheduler, and must never move inside kro.
- Splitting one graph across members (different parts of one unit in different
  clusters). See "Approach 2" in the companion design doc. Division here is
  *replicas of the same unit*, not parts of it.
- Property provenance / trust, must-vs-prefer constraint vocabulary, a
  grace period for unreachable members.
- Production concerns: HA hub, credential rotation, multi-tenancy, RBAC hardening.

_Update this table in the same PR whenever the PoC adds or changes a shortcut._
