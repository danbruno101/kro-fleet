# Demo script — one object, three clusters, one view

The click-by-click walkthrough for recording the kro-fleet MVP demo, validated
live against the exact setup below. The narration follows the **audience
narrative** of [`proposals/kro-fleet-mvp-plan.md`](proposals/kro-fleet-mvp-plan.md) §2:
*this is the north star, live*. Every technical caveat lives in
[`KEP-GAP.md`](KEP-GAP.md), not in the recording.

Everything below was exercised end-to-end on 2026-07-15 (Headlamp app 0.43.0,
plugin toolchain 0.14.0, kro 0.9.2, kind v1.36.1) — following this script
requires no improvisation. Reference frames for every screen are in
[`img/`](img/).

---

## 0. Pre-flight (before you hit record)

From the repo root:

```bash
# 1. Fleet up: 1 hub + 3 members (gke / aks / eks personas). ~15 min cold.
scripts/setup-fleet.sh 3

# 2. The hub placement controller, as a host process. Leave it running.
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub &

# 3. One kubeconfig with all four contexts (gitignored).
scripts/headlamp-kubeconfig.sh

# 4. Plugin build + deploy layout.
cd headlamp-plugin && npm install && npm run build
mkdir -p dist-plugins/kro-fleet && cp dist/main.js package.json dist-plugins/kro-fleet/
cd ..

# 5. Headlamp (browser mode — easiest to record). macOS app-bundle server:
/Applications/Headlamp.app/Contents/Resources/headlamp-server \
  -kubeconfig "$PWD/kro-fleet.kubeconfig" \
  -html-static-dir /Applications/Headlamp.app/Contents/Resources/frontend \
  -plugins-dir "$PWD/headlamp-plugin/dist-plugins" -port 4466
```

Open `http://localhost:4466` in the browser you will record.

**Demo must start EMPTY:** if `demo-llm` exists from a previous run, reset:

```bash
kubectl --context kind-kro-fleet-hub delete fgs demo-llm -n fleet-demo --ignore-not-found
# wait ~30 s until members drain (watch the KRO Fleet view empty out)
```

Keep a terminal visible next to (or overlaid on) the browser — the demo's
only typing is two `kubectl` commands against **the hub only**.

Sanity checks before recording:

- [ ] Headlamp Home shows 4 clusters, all **Active**.
- [ ] **KRO Fleet** in the sidebar of any cluster view; fleet view shows the
      3 ClusterProfiles (gke/aks/eks) and *no* FleetGenAIService.
- [ ] `kubectl --context kind-kro-fleet-hub get clusterprofiles -n fleet-system`
      → 3 profiles.

---

## 1. The fleet (≈30 s)

**Click:** Home (Headlamp logo) → *All Clusters* table.
*(frame: [`img/headlamp-home-clusters.png`](img/headlamp-home-clusters.png))*

> "Here's a fleet: one hub and three member clusters. Think of the members as
> GKE, AKS, and EKS — each one ships a different storage story, different
> defaults, different quirks. Today they're kind clusters so you can rerun
> this at home, but nothing you'll see cares about that."

**Click:** `kind-kro-fleet-hub` → sidebar **KRO Fleet**.

> "The hub keeps an inventory — one ClusterProfile per member, the
> SIG-Multicluster standard. gke, aks, eks, all tier prod, all healthy. And
> right now: nothing deployed anywhere."

Point at the empty *FleetGenAIServices* table and the three inventory rows.

---

## 2. Author once, on the hub (≈45 s)

**In the terminal**, show then apply the sample:

```bash
cat examples/fleetgenaiservice-sample.yaml
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
```

> "One object. A GenAIService — a model server for Qwen 0.5B — wrapped with
> one thing only: a placement rule, *every cluster labeled tier prod*. Notice
> what it does NOT say: no cluster names, no clouds, no storage classes, no
> per-environment overlays. The developer authors this once, on the hub."

**Switch to the browser** (KRO Fleet view, it updates live):

- The `demo-llm` row appears; *Placed / Ready* climbs `3/0 → 3/3`; the three
  member chips flip green (~60–90 s; talk over it).

> "The platform takes it from here. Placed on three clusters… and one by one
> they come back Ready. That roll-up — 3 of 3, minimum met — is aggregated
> back onto the hub object itself; your CI can wait on that one condition."

*(frame: [`img/headlamp-fleet-view.png`](img/headlamp-fleet-view.png))*

---

## 3. One object, every cluster — and the portability proof (≈30 s)

**Scroll** to *"One object, every cluster"*.

> "Same object on all three clusters — and here's the part that matters:
> look at the storage class column. premium-rwo on the GKE cluster,
> managed-csi on AKS, gp3 on EKS. Nobody wrote three YAMLs. Each cluster
> resolved the same intent against its own platform. That's the promise:
> intent is portable, resolution is local."

---

## 4. The graph (≈45 s)

**Click:** sidebar **Map** → click the source-filter chip (reads
*"Workloads, Storage, +2"*) → **check only "KRO Fleet"** → Esc → click
**Expand All** → the fit-view button (⛶, bottom-left) if needed.
*(frame: [`img/headlamp-fleet-map.png`](img/headlamp-fleet-map.png))*

> "This is the whole story in one picture. In the middle: the one object we
> authored, on the hub. Fanning out: its copy on each of the three clusters.
> And beyond that, what each cluster expanded it into — deployment, services,
> the model cache volume. One intent, twelve resources, three clusters, one
> canvas."

Hover a `GenAIService` node → its member cluster shows in the details.

---

## 5. Down to the pods (≈30 s)

**Click:** sidebar **KRO Fleet** → scroll to *"Placed workloads"* → **View**
on a `kro-fleet-member-3` pod.
*(frame: [`img/headlamp-pod-logs.png`](img/headlamp-pod-logs.png))*

> "And it's really running — these are live logs streaming from the pod on
> cluster three: the model server is up and serving. Fleet to graph to pod
> logs, without ever switching kubeconfigs."

Close the dialog (Esc).

---

## 6. Day two: mutate once, converge everywhere (≈45 s)

**In the terminal:**

```bash
kubectl --context kind-kro-fleet-hub patch fgs demo-llm -n fleet-demo --type=merge \
  -p '{"spec":{"template":{"spec":{"name":"demo-llm","model":"Qwen/Qwen2.5-0.5B-Instruct","mode":"mock","replicas":2,"cacheSize":"1Gi","monitoring":true}}}}'
```

> "Day two. Scale to two replicas — again, one edit, on the hub."

**Browser:** the pods table grows to 6 rows (2 per member) within seconds.
*(frame: [`img/headlamp-fleet-converged.png`](img/headlamp-fleet-converged.png))*

> "All three clusters converge on their own. No fan-out scripts, no drift."

---

## 7. Delete, and the graph drains (≈30 s)

**In the terminal:**

```bash
kubectl --context kind-kro-fleet-hub delete fgs demo-llm -n fleet-demo
```

**Browser:** on the KRO Fleet view the row and pods disappear; on the Map the
graph collapses back to nothing (~30 s).

> "And the exit is as clean as the entrance: delete the hub object, and
> everything it placed — on every cluster — is garbage-collected. No orphans.
> One object, three clusters, one view. That's the north star, and you just
> watched it run."

**End recording.**

---

## Re-takes & timing

- Total runtime: ≈4–5 minutes.
- Reset between takes = §7 delete + wait for drain, then start again at §2
  (the fleet, controller, and Headlamp all stay up).
- The two slow moments are §2 (60–90 s to 3/3) and §7 (~30 s drain) — both
  are narration-sized, but both can also be cut in editing; the fleet view
  updates live either way.
- If a member ever wedges (kind on a laptop): `docker restart
  kro-fleet-member-N-control-plane`, wait for green, re-take the scene —
  the hub status chips tell you when it's safe.
