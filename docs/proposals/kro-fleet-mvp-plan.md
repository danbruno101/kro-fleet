# kro-fleet MVP plan — the north star, demoable

| | |
|---|---|
| **Status** | Approved build plan |
| **Audience** | SIG Cloud Provider, KRO maintainers, and whoever builds this |
| **Backs** | [`KEP-kro-multicluster.md`](KEP-kro-multicluster.md) (the proposal) · [`../KEP-GAP.md`](../KEP-GAP.md) (the honest ledger) |
| **Supersedes** | The earlier separate `kro-fleet-poc-prompt.md` and `kro-fleet-headlamp-prompt.md` drafts — this is the single MVP spec |

This document is the **single, comprehensive MVP spec** for turning the kro-fleet
PoC into a visual, recordable demo. It exists because SIG Cloud Provider (elmiko)
and a KRO inventor (Jesse Butler) validated the fleet idea and asked for a
**visual demo** — a Headlamp dashboard — plus a **screen recording**.

---

## 1. Goal & framing

**The MVP of the north star is the *experience*, not the final architecture:**

> Author one object on the hub → it runs across **three** clusters → you *see* it —
> the fleet, the object graph, the pod logs — in one dashboard.

It is built **thin**: the hub placement controller + stock kro on members, exactly
as this repo already works. **No kro fork.** The expand-on-hub-vs-expand-on-member
difference is invisible in the demo; forking kro remains premature and buys nothing
the audience sees.

## 2. The two-narrative honesty rule

Two audiences, two lanes — never mix them:

- **Audience narrative** (the demo, the recording, the deck): *"this is the north
  star, live."* One object, three clusters, one view.
- **Maintainer narrative** (the KEP, KEP-GAP, any technical Q&A): *"this is a hub
  placement controller + stock kro on members; the native in-kro mode is the
  proposal, not the demo."*

[`docs/KEP-GAP.md`](../KEP-GAP.md) holds every technical caveat. The demo never
claims to be the native mode; the KEP never pretends the demo is less than the
real UX. Keep both honest, in their lanes.

## 3. Two components, one repo

### 3a. Controller — `FleetGenAIService` + hub placement controller — **largely DONE**

Already merged and e2e-proven in this repo (see `cmd/`, `api/`, `internal/`,
`scripts/e2e.sh`, and [`../phase0-validation.md`](../phase0-validation.md)):

- `FleetGenAIService` CRD carrying the GenAIService spec + `placement.clusterSelector`.
- Hub placement controller on **multicluster-runtime + ClusterProfile**: place →
  track applied manifests → finalizer GC → aggregate `status.clusters[]` + rolled-up
  condition.
- Stock kro (pinned, currently 0.9.2) expands the placed instance on each member.

**MVP delta for the controller:** run the demo with **three members**
(`scripts/setup-fleet.sh 3` — the scripts are already N-member), label the members
as three "clouds" (gke/aks/eks personas, as in the sister repo
[kro-genaiops-demo](https://github.com/danbruno101/kro-genaiops-demo)), and make
sure `status.clusters[]` reads well on screen. No architectural work expected.

### 3b. Headlamp plugin — `headlamp-plugin/` — **the real build**

A TypeScript/React [Headlamp](https://headlamp.dev) plugin, living in
`headlamp-plugin/` in this repo, that shows:

1. **Fleet view** — the hub's ClusterProfile inventory + the `FleetGenAIService`
   with its per-member `status.clusters[]`, as one screen.
2. **The one object across 3 clusters** — hub object on top, its three placed
   copies underneath, statuses live.
3. **The object graph** — a custom **map source** feeding Headlamp's resource-map
   view: hub `FleetGenAIService` → placed `GenAIService` per member → the kro-expanded
   children (Deployment/Service/PVC/…) on that member.
4. **Pod logs** — click through from a placed workload to its pods' logs, so the
   demo ends on "and here is the model server actually serving, on cluster 3."

Borrow patterns, don't invent: the **Kubeflow** and **Cluster API (CAPI)** Headlamp
plugins are the reference implementations for CRD-centric views and multi-object
maps; [pnz1990/kro-ui](https://github.com/pnz1990/kro-ui) has prior art for
rendering a kro object graph — reuse its ideas for the graph layout.

## 4. MVP scope — cut hard

**IN:**

- Place **1** `FleetGenAIService` → **3** members; basic `status.clusters[]`.
- Headlamp plugin: fleet view + object graph (map source) + pod logs.
- A **screen recording** of the full loop (elmiko's fallback if a live demo is
  impractical — record it regardless).

**OUT / deferred** (all already ledgered in [`KEP-GAP.md`](../KEP-GAP.md) — add a
row there if the MVP introduces any *new* shortcut):

- Native expand-on-hub (the KEP itself).
- Placement strategies beyond the label selector.
- Pull mode; GC edge cases beyond what e2e already covers; scale.
- Hardened credentials/RBAC, HA hub, multi-tenancy.
- Any new ML/workload content — reuse the sister repo's RGDs and
  `ghcr.io/danbruno101/mock-vllm:demo`.

**Fleet target:** default to **kind** (1 hub + 3 members) — it is what this repo's
scripts, CI, and pinned versions already prove, and it is the more demo-stable
choice: reproducible, resettable in minutes, no cloud quota or credential risk
mid-recording. Re-running the demo against the sister repo's real GKE/AKS/EKS
clusters is a stretch goal, not the MVP.

## 5. Build sequence

1. **Controller: 3-member demo profile.** `setup-fleet.sh 3`, cloud-persona labels,
   a demo `FleetGenAIService`, verify `status.clusters[]` reads well. (Small — the
   controller and scripts exist.)
2. **Plugin Phase 0 — the only real unknown, start in parallel with (1):** prove
   Headlamp can (a) read hub + all 3 members in one deployment, (b) register a
   custom map source rendering the cross-cluster graph, (c) stream pod logs from a
   member. Pin the Headlamp version + plugin API this works against.
3. **Plugin build:** fleet view → object-across-clusters view → graph → logs, in
   that order (each screen is independently demoable).
4. **Record it.** Script the narration to the audience narrative (§2); capture the
   full loop: author on hub → three clusters converge → graph → logs. Update the
   Marp deck (`docs/deck.md`) to embed/point at the recording.

## 6. Phase-0 risks — validate before building on them

| Risk | Status |
|---|---|
| multicluster-runtime + ClusterProfile hub→member reconcile actually works, versions pinned | ✅ **Validated** — [`../phase0-validation.md`](../phase0-validation.md), enforced by CI (`scripts/e2e.sh`) |
| Headlamp cross-cluster-in-one-view: read hub **and** members in a single UI; custom **map source** API available in the current release; pod logs across clusters | ❌ **Open — validate FIRST** (step 2 above), before writing any real plugin code. If Headlamp cannot do one of these, surface it and propose alternatives (per CLAUDE.md) rather than building around it silently |

The Headlamp Phase-0 mirrors how this repo did the controller: a throwaway spike
under `hack/`, findings written to a `docs/` note with pinned versions, then the
real build on top.

## 7. Where this runs

This plan is executed **in this repo** (`danbruno101/kro-fleet`), in sessions that
load the existing [`CLAUDE.md`](../../CLAUDE.md),
[`KEP-kro-multicluster.md`](KEP-kro-multicluster.md), and
[`../KEP-GAP.md`](../KEP-GAP.md). All of that context — the scope decision (no kro
fork, build on SIG-Multicluster), the conventions (kind, pinned versions, hermetic
CI, Apache-2.0 headers, no secrets), and the git workflow (feature branch → draft
PR, never push to `main`) — applies to the MVP work unchanged.

The plugin work adds one layout entry to CLAUDE.md's suggested tree:

```
headlamp-plugin/   # TS/React Headlamp plugin: fleet view, object graph, pod logs
```
