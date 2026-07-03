# KEP-GAP — how this PoC differs from the proposal

An **honest ledger** of where `kro-fleet` (this thin PoC) deliberately diverges from
the ideal design in [`docs/proposals/KEP-kro-multicluster.md`](proposals/KEP-kro-multicluster.md).
Keep this current as the PoC evolves — it exists so reviewers are never misled into
thinking the PoC *is* the finished native design.

> **One-line framing:** the PoC proves the **API, UX, and SIG-Multicluster
> integration** of fleet-scoped KRO objects; it does **not** implement the native,
> in-kro control loop. That is intentional and is future work.

## The gaps

| Aspect | KEP (native ideal) | This PoC | Why / follow-up |
|---|---|---|---|
| **Graph expansion** | Inside kro, on the **hub**, one control loop | **Stock kro on each member** expands the placed instance locally | Avoids forking kro; validates the UX + placement first. Native = a change inside `kubernetes-sigs/kro`. |
| **New code surface** | A multi-cluster mode *within* kro | A **separate hub placement controller**; kro unmodified | Keeps the PoC small and reviewable. |
| **Member credentials** | ClusterProfile `status.accessProviders` plugin mechanism (KEP-4322/5339) | **Simplified**: the provider's `Secret` kubeconfig strategy (labeled Secret next to the ClusterProfile, data key `Config`) | ClusterProfile is still the inventory API; only the credential resolution is simplified for kind. The same provider also supports the KEP-5339 `CredentialsProvider` strategy — switching is config, not architecture. |
| **Member health** | A cluster manager agent maintains `status.conditions` (e.g. `ControlPlaneHealthy`) on each ClusterProfile | **Self-asserted**: setup scripts patch `ControlPlaneHealthy=True` manually; the provider's default readiness gate consumes it unchanged | kind members have no cluster-manager agent. The gate itself (engage only healthy profiles) is exercised for real. |
| **Placement** | Extensible `placement.strategy` (spread, capacity, drain/evacuate) | **Label selector only** | Matches the KEP's v1 scope; strategies are reserved future work. |
| **Distribution model** | Design allows push or pull | **Push** (hub → member API via multicluster-runtime) only | Pull-mode agent out of scope for the PoC. |
| **Status aggregation** | Rolled-up conditions + per-member `status.clusters[]` | Implemented (may be simplified) | Track any simplifications here as they happen. |
| **Cross-cluster GC** | Native ownership/finalizer semantics | Hub-side tracking + finalizer (`fleet.kro.run/cleanup`). Edge case: a member whose ClusterProfile is deleted **before** cleanup is skipped — its copy is orphaned on that (now unreachable) member | Same idea as OCM `ManifestWork`. A registered-but-unreachable member correctly blocks finalization and retries. |
| **Applied-manifest inventory** | A dedicated per-`(instance, member)` manifest inventory (like OCM `AppliedManifestWork`) | **`status.clusters[]` doubles as the inventory** — valid because the PoC places exactly one object (the GenAIService) per member; intent is persisted *before* touching members so a crash cannot orphan an untracked copy | A real implementation needs a separate record keyed by GVK/name per member. |
| **Member readiness** | kro-defined instance readiness contract | **Heuristic**: `status.state == ACTIVE`, else a true `Ready`/`InstanceSynced` condition | Matches stock kro's instance status; revisit when phase 2 runs real kro on members. |
| **Scale** | Fleet-scale (many clusters) | A handful of **kind** clusters on a laptop | PoC proves correctness, not scale. |

## What the PoC *does* faithfully prove

Everything below is asserted by `scripts/e2e.sh` (the same script CI runs), on
kind, against **stock kro 0.9.2** expanding the placed objects on the members.
- One placement-enabled object on a hub, placed onto matching members, with
  aggregated status — the core UX of the KEP.
- Integration with **ClusterProfile** (inventory) and **multicluster-runtime**
  (cross-cluster reconciliation) — i.e. the "build on SIG-Multicluster, don't
  reinvent propagation" thesis.
- Clean lifecycle: place → converge → add/remove member → evacuate → delete, with
  no orphaned resources.

## Not attempted (explicitly out of scope for the PoC)
- Any change to `kubernetes-sigs/kro` itself (the native mode).
- Cross-cluster networking / service discovery (MCS territory).
- Capacity-aware or policy-driven scheduling.
- Production concerns: HA hub, credential rotation, multi-tenancy, RBAC hardening.

_Update this table in the same PR whenever the PoC adds or changes a shortcut._
