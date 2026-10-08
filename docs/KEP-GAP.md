# KEP-GAP — how this PoC differs from the proposal

An **honest ledger** of where `kro-fleet` (this thin PoC) deliberately diverges from
the ideal design in [`docs/proposals/KEP-kro-multicluster.md`](proposals/KEP-kro-multicluster.md).
Keep this current as the PoC evolves — it exists so reviewers are never misled into
thinking the PoC *is* the finished native design.

> **Pins:** every upstream version this ledger assumes is recorded in
> [`rebaseline-2026-10.md`](rebaseline-2026-10.md) (October 2026) and, for the
> original PoC, [`phase0-validation.md`](phase0-validation.md).

> **One-line framing:** the PoC proves the **API, UX, and SIG-Multicluster
> integration** of fleet-scoped KRO objects; it does **not** implement the native,
> in-kro control loop. That is intentional and is future work.

## The gaps

| Aspect | KEP (native ideal) | This PoC | Why / follow-up |
|---|---|---|---|
| **Graph expansion** | Inside kro, on the **hub**, one control loop | **Stock kro on each member** expands the placed instance locally | Avoids forking kro; validates the UX + placement first. Native = a change inside `kubernetes-sigs/kro`. |
| **New code surface** | A multi-cluster mode *within* kro | A **separate hub placement controller**; kro unmodified | Keeps the PoC small and reviewable. |
| **Member credentials** | ClusterProfile `status.accessProviders` plugin mechanism (KEP-4322/5339) | **As proposed** (since the Oct 2026 re-baseline): every ClusterProfile advertises `status.accessProviders[]`, and the controller resolves the named provider through an exec credential plugin via the provider's AccessProvider strategy — on kind the upstream `kubeconfig-secretreader` plugin, on the clouds each CLI's exec plugin. The controller never reads a kubeconfig Secret itself | Was: the provider's (now deprecated) labeled-Secret strategy. Closed by `docs/rebaseline-2026-10.md`. What remains simplified is *who writes* `accessProviders`: the setup scripts, standing in for a cluster-manager agent (next row). |
| **Controller packaging** | The KRO control plane runs in-cluster on the hub | **Host process**: `go run ./cmd/fleet-controller` against the hub kubeconfig (`scripts/setup-fleet.sh`) | PoC convenience only; nothing architectural depends on where the controller runs. |
| **Member health** | A cluster manager agent maintains `status.conditions` (e.g. `ControlPlaneHealthy`) on each ClusterProfile | **Self-asserted**: setup scripts patch `ControlPlaneHealthy=True` at registration; the demo flips it to simulate an outage. The controller enforces the condition on every pass — an unhealthy member is neither written to nor cleaned up until it recovers — because the provider (v0.25.2) consults it only before engaging a member and keeps an engaged member engaged when it later turns unhealthy | kind members have no cluster-manager agent. The gate itself is exercised for real; the provider's engage-time-only check is worth raising upstream (a disengage on unhealthy would make the controller-side gate redundant). |
| **Placement source** | v2 adds `placement.decisionRef` — placement supplied by an external producer (scheduler / policy engine / failover controller) alongside the v1 selector | **As proposed** (since the Oct 2026 iteration): `decisionRef` (by name) or `placementKey` (KEP-5313 label discovery, slices merged by `decision-index`) consume a standard `PlacementDecision`; `clusterSelector` remains as the v1 fallback. Exactly one source per instance | The decision object is the SIG-Multicluster standard, not a local CRD. What the standard does not carry is supplied by producer annotations — see `docs/placement-decision-gaps.md`. |
| **Per-member parameters** | A decision may carry per-cluster values the graph references (`${placement.thisCluster.parameters.*}`) — what turns *replication* into *division* | **Built, with a convention**: parameters come from producer-set annotations on the decision (`parameters.fleet.kro.run/<cluster>`), and the hub controller folds them into each member's copy of the template before placing it (today: `replicas`). Values only, never graph shape | The KEP sketches a CEL hook inside kro; this PoC applies the override on the hub because members run stock kro. Same semantics, different seam. The annotation carrier is a gap written up for SIG-Multicluster. |
| **Empty placement** | Terminal, explicit `Placed=False` with a machine-readable reason; never a fallback to a broader set | **As proposed**: a `Placed` condition with reasons `DecisionPending` (decision absent), `NoEligibleClusters` (decision present and empty, or selector matches nothing — terminal, with the producer's reason), `InsufficientCapacity` (partial decision: the slice is placed, nothing spills), `DecisionViolatesRequirements` (refused as a whole). Nothing placed is never Ready — the explicit-zero-tolerance vacuous success is gone | The pending-vs-refused distinction relies on a convention (absent = pending, empty = refusal, reason via annotation) because the standard has no decision-level reason — see `docs/placement-decision-gaps.md`. |
| **Decision provenance** | Resolved decision + justification recorded in status for audit | **As proposed**: `status.placement` records source, decision slice names, scheduler name, the decision-level reason, and per-member reason + parameters | — |
| **Distribution model** | Design allows push or pull | **Push** (hub → member API via multicluster-runtime) only | Pull-mode agent out of scope for the PoC. |
| **Status aggregation** | Rolled-up conditions + per-member `status.clusters[]`; members unreachable beyond a grace period surface `Degraded`/`Unknown` rather than blocking the object | Per-member entries + tolerance rollup implemented. **No grace period, no `Degraded`/`Unknown`**: an unreachable member is marked not-ready with a reason and the reconcile retries sooner | The KEP's unreachable-member semantics are design, not built. |
| **Cross-cluster GC** | Native ownership/finalizer semantics | Hub-side inventory + finalizer (`fleet.kro.run/cleanup`). A member whose ClusterProfile is deleted **before** cleanup no longer blocks the instance, and is no longer silently skipped: its `AppliedManifestRecord` is kept with `Orphaned=True` as the ledger of what may remain there, and is settled (objects deleted, record removed) when the member registers again | Same idea as OCM `ManifestWork`/`AppliedManifestWork`. A registered-but-unreachable member still blocks finalization and retries. Remaining gap: nothing reaps a record whose member never returns, and the orphaned objects are unreachable in the meantime — surfaced, not solved. |
| **Applied-manifest inventory** | A dedicated per-`(instance, member)` manifest inventory (like OCM `AppliedManifestWork`) | **As proposed** (since the Oct 2026 iteration): one `AppliedManifestRecord` per (instance, member), keyed by GVK/namespace/name — intent written *before* the member is touched, confirmed with UIDs after; unplacement, pruning and finalization delete exactly the recorded objects (label- and UID-guarded). `status.clusters[]` is readiness only | Was: `status.clusters[]` doubling as the inventory, valid only while every member received one identical object. The shared workload namespace is ensured but deliberately not inventoried (the member's `ClusterPlatform` and other instances live there). |
| **Member readiness** | kro-defined instance readiness contract | **Heuristic**: `status.state == ACTIVE`, else a true `Ready`/`InstanceSynced` condition, combined with `status.readyReplicas` (the sister RGD surfaces it) against the member's assigned replicas. The fleet-level `Ready` is replica-based for divided placements (`tolerance.minReadyReplicas`, default all requested) and cluster-based for replicated ones (`tolerance.minReadyClusters`) | Matches stock kro's instance status; a contract inside kro would replace the heuristic. |
| **Scale** | Fleet-scale (many clusters) | A handful of **kind** clusters on a laptop | PoC proves correctness, not scale. |

## What the PoC *does* faithfully prove

Everything below is asserted by `scripts/e2e.sh` (the same script CI runs), on
kind, against **stock kro 0.9.4** expanding the placed objects on the members.
- One placement-enabled object on a hub, placed onto matching members, with
  aggregated status — the core UX of the KEP.
- Integration with **ClusterProfile** (inventory) and **multicluster-runtime**
  (cross-cluster reconciliation) — i.e. the "build on SIG-Multicluster, don't
  reinvent propagation" thesis.
- Clean lifecycle: place → converge → add/remove member → evacuate → delete, with
  no orphaned resources — and, when a member is deregistered before cleanup,
  the orphan is surfaced in its inventory record and settled when the member
  returns.

## v2 scope, not yet built

KEP v2 adds `decisionRef` and per-member parameters so that the layers which own
capacity, compliance and failover can drive kro. The plan is to prove each with a
**deliberately trivial external decision producer** — none of them part of kro,
each replaceable by a real implementation (OCM Placement, Karmada, a policy
engine) without changing kro. That substitutability is the property under test.

| Use case | Producer to build | e2e assertion |
|---|---|---|
| Capacity | Reads member allocatable, emits per-member `replicas` summing to the request | Members receive *different* counts; the sum matches; capacity change reconverges |
| Compliance | Filters to clusters with a compliance property; emits an empty decision when none qualify | Placement only on qualifying members; empty decision -> terminal `Placed=False`, **no** fallback; decision recorded in status |
| Failover | Watches ClusterProfile `ControlPlaneHealthy`, rewrites the decision | Unplaced from the failed member, placed on a standby; teardown retried without orphaning when the member returns |

## Not attempted (explicitly out of scope for the PoC)
- Any change to `kubernetes-sigs/kro` itself (the native mode).
- Cross-cluster networking / service discovery (MCS territory).
- **Computing** placement — capacity estimation, policy evaluation, scheduling.
  kro consumes decisions; the producers above are demo scaffolding, not a
  scheduler, and must never move inside kro.
- Splitting one graph across members (different parts of one unit in different
  clusters). See "Approach 2" in the companion design doc.
- Production concerns: HA hub, credential rotation, multi-tenancy, RBAC hardening.

_Update this table in the same PR whenever the PoC adds or changes a shortcut._
