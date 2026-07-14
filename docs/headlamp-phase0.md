# Headlamp plugin Phase 0 — findings

The MVP plan ([`proposals/kro-fleet-mvp-plan.md`](proposals/kro-fleet-mvp-plan.md), §6)
gates the plugin build on validating three unknowns: cross-cluster reads in one
Headlamp instance, a custom map source, and pod logs from members. This note
records what is now **verified against the pinned toolchain** and what still
needs a **live** pass against the kind fleet.

## Pinned versions

| Component | Version | How pinned |
|---|---|---|
| `@kinvolk/headlamp-plugin` (build toolchain + plugin API) | **0.14.0** (exact) | `headlamp-plugin/package.json` |
| TypeScript (toolchain override) | 5.6.2 | npm `overrides`, matching the upstream template |

## Desk-validated (against the 0.14.0 package contents — its shipped type
declarations and official plugins are the source of truth)

- **Custom map source** — `registerMapSource({ id, label, icon, useData() → { nodes, edges } })`
  is exported from `@kinvolk/headlamp-plugin/lib`; the **official Cluster API
  plugin** shipped inside the package uses exactly this to draw its topology
  (nodes carry `kubeObject`, edges are `{ id, source, target, label }`).
  `headlamp-plugin/src/fleetMap.tsx` follows that pattern.
- **Cross-cluster reads in one view** — the plugin lib is natively
  multi-cluster: `useClustersConf()` enumerates all configured clusters, and
  `KubeObject.useList({ cluster })` / **`useList({ clusters: string[] })`** /
  `useGet(name, ns, { cluster })` target specific clusters from a single view
  (signatures in `lib/lib/k8s/KubeObject.d.ts`). Custom resources come from
  `makeCustomResourceClass(...)`, which the fleet CRDs use in
  `headlamp-plugin/src/fleet.ts`.
- **Pod logs from a member** — the **official Kubeflow plugin** streams logs
  with `Pod.useGet(podName, namespace, { cluster })` +
  `pod.getLogs(container, callback, { follow: true, tailLines, ... })`.
  `headlamp-plugin/src/components/PodLogs.tsx` mirrors it (without the xterm
  dependency).
- **Build** — `headlamp-plugin tsc`, `lint`, and `build` all pass on the
  scaffold; the bundle is `dist/main.js`.

## Still requires LIVE validation (next session with docker/kind)

The scaffold compiles against the real API surface, but none of it has run
against a cluster yet. Before building further UI:

1. `setup-fleet.sh 3` + controller + sample, `scripts/headlamp-kubeconfig.sh`,
   launch Headlamp with the plugin (see `headlamp-plugin/README.md`) and
   confirm:
   - all four contexts appear and the fleet view resolves the hub by the
     `-hub` name heuristic;
   - `useList({ clusters: [...] })` returns member objects (and how errors
     surface for an unreachable member);
   - the map renders the cross-cluster graph — in particular whether the map
     view scopes to the *selected* cluster or accepts nodes from several
     clusters in one canvas (the one behavior desk analysis cannot prove);
   - log streaming works against pods on members.
2. Pin the Headlamp **app** version used for the demo alongside the plugin
   version above.

If the map view turns out to be single-cluster-scoped, the fallback is a
dedicated graph inside the KRO Fleet route (own React Flow canvas) instead of
a map source — the data hooks stay identical.
