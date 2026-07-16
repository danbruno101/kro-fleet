# Headlamp plugin Phase 0 — findings

The MVP plan ([`proposals/kro-fleet-mvp-plan.md`](proposals/kro-fleet-mvp-plan.md), §6)
gates the plugin build on validating three unknowns: cross-cluster reads in one
Headlamp instance, a custom map source, and pod logs from members. **All three
are now live-validated** against the 3-member kind fleet (see below); the
desk-validation notes are kept for reference.

## Pinned versions

| Component | Version | How pinned |
|---|---|---|
| `@kinvolk/headlamp-plugin` (build toolchain + plugin API) | **0.14.0** (exact) | `headlamp-plugin/package.json` |
| TypeScript (toolchain override) | 5.6.2 | npm `overrides`, matching the upstream template |
| Headlamp **app** (demo runtime) | **0.43.0** (macOS, Homebrew cask) | the demo serves its bundled `headlamp-server` + frontend: `Headlamp.app/Contents/Resources/{headlamp-server,frontend}` |

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

## Live validation results (2026-07-15, 3-member kind fleet, Headlamp 0.43.0)

Run: `setup-fleet.sh 3` + fleet controller + `fleetgenaiservice-sample.yaml`
(3/3 ready), `scripts/headlamp-kubeconfig.sh`, then the app bundle's server:

```bash
/Applications/Headlamp.app/Contents/Resources/headlamp-server \
  -kubeconfig "$PWD/kro-fleet.kubeconfig" \
  -html-static-dir /Applications/Headlamp.app/Contents/Resources/frontend \
  -plugins-dir "$PWD/headlamp-plugin/dist-plugins" -port 4466
```

1. **All four contexts in one Headlamp; hub found by the `-hub` heuristic** ✅
   Home lists hub + 3 members (all Active); the KRO Fleet view resolves
   `kind-kro-fleet-hub` and renders the FleetGenAIService (3/3,
   `MinReadyClustersMet`, per-member chips), the ClusterProfile inventory
   (gke/aks/eks), and pods from all three members in one table.
2. **`useList({ clusters })` returns member objects** ✅ — with one critical
   caveat: the *aggregated* `items` of a multi-cluster list stays empty while
   any member hangs (validated by `docker pause`-ing member-3: the whole pods
   table blanked). A hung member never yields a clean `ApiError` — its
   per-cluster query just stays pending. The fleet view therefore reads
   **`result.clusterResults[cluster].items/.errors`** so reachable members
   always render, clean per-member failures surface as inline warnings, and
   the hub's own `status.clusters[]` (which flipped to 3/2 with a red chip
   within seconds of the pause — the controller noticed on its own) stays the
   authoritative signal. This is exactly the KEP's aggregation story.
3. **The map canvas is NOT single-cluster-scoped** ✅ — the key open
   question, answered live: one canvas renders the hub `FleetGenAIService` →
   the three placed `GenAIService`s (one per member) → each member's
   kro-expanded children. 16 nodes across 4 clusters, edges crossing cluster
   boundaries. The React-Flow-canvas fallback is **not needed**.
4. **Log streaming from member pods** ✅ — the dialog streams the mock-vllm
   startup line from a pod on member-3 via `pod.getLogs(..., {follow: true})`.

Two plugin-side corrections came out of the live pass (both in-tree):

- **kro 0.9.2 children carry NO ownerReferences.** Stock kro links expanded
  children to their instance via labels (`kro.run/instance-id` = instance
  uid, `kro.run/owned=true`, …). The map source now matches children by that
  label; the original ownerReferences matching rendered an empty graph.
- **`scripts/headlamp-kubeconfig.sh` was broken on macOS** (BSD `paste`
  without `-`), silently flattening the user's default kubeconfig instead of
  the fleet contexts — fixed and regenerated.

Benign noise observed (no action): 404s for `metrics.k8s.io` /
`autoscaling.k8s.io` on kind (no metrics-server) and transient WebSocket
multiplexer errors; lists and log streaming are unaffected.
