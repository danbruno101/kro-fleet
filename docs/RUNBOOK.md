# RUNBOOK — kro-fleet PoC

A guided walkthrough of the fleet loop: one placement-enabled object on a hub
kind cluster, placed onto member kind clusters running **stock kro**, with
per-member status aggregated back on the hub.

Honesty first: this is a **PoC of the KEP's UX and SIG-Multicluster
integration**, not the native in-kro implementation. The shortcuts are
ledgered in [`KEP-GAP.md`](KEP-GAP.md).

## Prerequisites

- docker, kind ≥ 0.32, kubectl ≥ 1.36, helm ≥ 3.14 (v4 works), Go ≥ 1.26
- ~6 GB RAM headroom for 3 single-node kind clusters
- Nothing else: no cloud account, no GPU, no secrets.

## 1. Stand up the fleet

```bash
scripts/setup-fleet.sh 2          # 1 hub + 2 members (~5 min first run)
```

What this does (all versions pinned inside the script):

| Where | What |
|---|---|
| hub | `ClusterProfile` CRD (cluster-inventory-api v0.1.3, KEP-4322) + our `FleetGenAIService` CRD |
| members | stock **kro 0.9.2** (Helm, `registry.k8s.io`), the sister repo's `GenAIService` + `ClusterPlatform` RGDs (pinned commit), one `ClusterPlatform` per simulated cloud (member-1 = gke/`premium-rwo`, member-2 = aks/`managed-csi`) |
| hub | one `ClusterProfile` per member (labels `tier=prod`, `fleet.kro.run/cloud=…`) + a labeled kubeconfig `Secret` (the provider's Secret strategy) |

Members run **zero fleet-aware code**.

> Nodes can't reach registries on your network (e.g. localhost proxy)? Add
> `PRELOAD_IMAGES=true`.

## 2. Start the placement controller (hub-side, the only new code)

```bash
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub
```

Watch the log: the cluster-inventory-api provider engages each healthy
ClusterProfile (`Cluster engaged manager…`).

## 3. Place one object across the fleet

```bash
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo -w
```

Within ~a minute: `PLACED 2, READY 2`. On each member, stock kro expanded the
placed `GenAIService` into PVC + Deployment + Services and the mock-vllm pod
went Ready. The fleet view:

```bash
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo \
  -o jsonpath='{range .status.clusters[*]}{.name}: ready={.ready} ({.message}){"\n"}{end}'
```

The portability proof — same hub object, per-cloud storage:

```bash
kubectl --context kind-kro-fleet-member-1 get pvc demo-llm-cache -n fleet-demo -o jsonpath='{.spec.storageClassName}'   # premium-rwo (gke sim)
kubectl --context kind-kro-fleet-member-2 get pvc demo-llm-cache -n fleet-demo -o jsonpath='{.spec.storageClassName}'   # managed-csi (aks sim)
```

## 4. Exercise the lifecycle

```bash
# converge: mutate once on the hub, all members follow (via kro)
kubectl --context kind-kro-fleet-hub patch fgs demo-llm -n fleet-demo --type=merge \
  -p '{"spec":{"template":{"spec":{"name":"demo-llm","model":"Qwen/Qwen2.5-0.5B-Instruct","mode":"mock","replicas":2,"cacheSize":"1Gi","monitoring":true}}}}'

# unmatch: member leaves the selector -> workload + expanded graph removed there
kubectl --context kind-kro-fleet-hub label clusterprofile kro-fleet-member-2 -n fleet-system tier=dev --overwrite

# re-match: lands again automatically
kubectl --context kind-kro-fleet-hub label clusterprofile kro-fleet-member-2 -n fleet-system tier=prod --overwrite

# delete: fleet-wide GC via the finalizer
kubectl --context kind-kro-fleet-hub delete fgs demo-llm -n fleet-demo
```

## 5. The whole thing, asserted

```bash
scripts/e2e.sh    # runs all six success criteria; same script CI runs
```

## 6. Teardown

```bash
scripts/teardown-fleet.sh
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| ClusterProfile never engages | Its status needs `ControlPlaneHealthy=True` (setup asserts it; see KEP-GAP) and a Secret labeled `x-k8s.io/cluster-inventory-consumer=kro-fleet`, `x-k8s.io/cluster-profile=<name>` with key `Config`. |
| Member pods stuck `ErrImagePull` behind a proxy | Node containers can't see a localhost proxy. `PRELOAD_IMAGES=true scripts/setup-fleet.sh`. |
| `kind load docker-image` fails with `content digest … not found` | Docker's containerd image store + multi-arch images. The scripts already work around it (`docker save \| ctr import`). |
| kubelet refuses to start on cgroup v1 hosts | Handled by `failCgroupV1: false` in `config/kind/cluster.yaml` (no-op on cgroup v2). |
| GenAIService placed but never Ready | Check `genaiops-platform-config` exists in `fleet-demo` on the member (the ClusterPlatform instance creates it; the RGD reads it via `externalRef`). |
