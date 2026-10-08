# RUNBOOK — kro-fleet PoC

A guided walkthrough of the fleet loop: one placement-enabled object on a hub
kind cluster, placed onto member kind clusters running **stock kro**, with
per-member status aggregated back on the hub.

Honesty first: this is a **PoC of the KEP's UX and SIG-Multicluster
integration**, not the native in-kro implementation. The shortcuts are
ledgered in [`KEP-GAP.md`](KEP-GAP.md).

## Prerequisites

- docker, kind ≥ 0.33, kubectl ≥ 1.37, helm ≥ 3.14 (v4 works), Go ≥ 1.26
- ~6 GB RAM headroom for 3 single-node kind clusters
- Nothing else: no cloud account, no GPU, no secrets.

## 1. Stand up the fleet

```bash
scripts/setup-fleet.sh 3          # 1 hub + 3 members: gke / aks / eks personas (~8 min first run)
```

What this does (all versions pinned inside the script):

| Where | What |
|---|---|
| hub | `ClusterProfile` + `PlacementDecision` CRDs (cluster-inventory-api v0.1.3, KEP-4322 / KEP-5313) + the `fleet.kro.run` CRDs |
| members | stock **kro 0.9.4** (Helm, `registry.k8s.io`), the About API `ClusterProperty` CRD + that persona's properties (`docs/properties.md`; member-2 = aks is the one `fedramp-high` member, member-3 = eks the cheapest tier), **4 fake `example.com/gpu`** on the node, the fleet RGDs (`config/rgd/`, derived from the sister repo's), one `ClusterPlatform` per simulated cloud (member-1 = gke/`premium-rwo`, member-2 = aks/`managed-csi`, member-3 = eks/`gp3`) |
| hub | one `ClusterProfile` per member (labels `tier=prod`, `fleet.kro.run/cloud=…`) whose `status.accessProviders[]` points the `kubeconfig-secretreader` exec plugin at that member's kubeconfig `Secret` (KEP-4322 / KEP-5339); the plugin binary is built into `bin/` |

Members run **zero fleet-aware code**.

> Nodes can't reach registries on your network (e.g. localhost proxy)? Add
> `PRELOAD_IMAGES=true`.

## 2. Start the placement controller (hub-side, the only new code)

```bash
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub \
  --kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin
```

Watch the log: the cluster-inventory-api provider engages each healthy
ClusterProfile (`Cluster engaged manager…`), fetching each member's credentials
through the exec plugin named in its `status.accessProviders`. Cloud fleets
pass `--access-providers-file` (the `pkg/access` JSON format) instead, mapping
their provider names to `aws eks get-token`, `gke-gcloud-auth-plugin` or
`kubelogin`.

## 2b. Start the demo-only producers (inventory agent + scheduler)

```bash
go run ./cmd/fleet-demo all --hub-context kind-kro-fleet-hub \
  --kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin
```

The inventory agent mirrors each member's `ClusterProperty` objects into its
`ClusterProfile.status.properties` and computes `gpus-free.example.com`; the
scheduler writes a `PlacementDecision` for every instance that asks for one.
Neither is part of kro-fleet; both are replaceable (see `docs/KEP-GAP.md`).

## 3. Place one object across the fleet

```bash
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo -w
```

Within ~a minute: `PLACED 3, READY 3`. On each member, stock kro expanded the
placed `GenAIService` into a StatefulSet (one `cache` PVC per replica, in that
member's storage class) + Services, and the mock-vllm pod went Ready. The fleet
view:

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

## 4b. The capacity case: divide one instance by a decision

```bash
kubectl --context kind-kro-fleet-hub get clusterprofiles -n fleet-system \
  -o custom-columns='NAME:.metadata.name,FREE:.status.properties[?(@.name=="gpus-free.example.com")].value'   # 4 / 4 / 4: nobody has 8
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-divided.yaml     # replicas: 8, mode: gpu
kubectl --context kind-kro-fleet-hub get placementdecision demo-llm -n fleet-demo -o yaml  # 3 / 3 / 2, written by the scheduler
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo                        # REPLICAS 8 / REQUESTED 8 when ready
kubectl --context kind-kro-fleet-hub get fgs demo-llm -n fleet-demo -o jsonpath='{.status.placement}'   # provenance

# efficiency: consolidate onto the cheapest tiers (eks=1, gke=2, aks=3)
kubectl --context kind-kro-fleet-hub annotate fgs demo-llm -n fleet-demo scheduler.example.com/policy=cheapest-first --overwrite
# compliance: only member-2 is fedramp-high and has 4 free -> 4 of 6 placed there, Placed=False/InsufficientCapacity, nothing spilled
# drain: kubectl --context kind-kro-fleet-member-1 apply -f - <<< '{"apiVersion":"about.k8s.io/v1beta1","kind":"ClusterProperty","metadata":{"name":"draining.example.com"},"spec":{"value":"true"}}'
# one URL (needs cloud-provider-kind running): go run ./cmd/fleet-demo gateway --instance demo-llm ... ; curl localhost:8080/v1/chat/completions
```

Every one of these beats is asserted by the e2e (criteria 8–12); the cloud
version of the same demo is `scripts/demo.sh` (`docs/demo-cloud.md`).

## 5. The whole thing, asserted

```bash
scripts/e2e.sh    # runs all twelve criteria; same script CI runs (~35 min)
E2E_FROM=8 scripts/e2e.sh   # dev: only the decision/demo scenarios, on a fleet that is already up
```

## 6. Teardown

```bash
scripts/teardown-fleet.sh
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| ClusterProfile never engages | Its status needs `ControlPlaneHealthy=True` (setup asserts it; see KEP-GAP) and an `accessProviders[]` entry named `kubeconfig-secretreader` whose `client.authentication.k8s.io/exec` extension names the kubeconfig Secret (`name`, `key: Config`, `namespace`). The plugin must be reachable: `--kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin`. |
| Member pods stuck `ErrImagePull` behind a proxy | Node containers can't see a localhost proxy. `PRELOAD_IMAGES=true scripts/setup-fleet.sh`. |
| `kind load docker-image` fails with `content digest … not found` | Docker's containerd image store + multi-arch images. The scripts already work around it (`docker save \| ctr import`). |
| kubelet refuses to start on cgroup v1 hosts | Handled by `failCgroupV1: false` in `config/kind/cluster.yaml` (no-op on cgroup v2). |
| GenAIService placed but never Ready | Check `genaiops-platform-config` exists in `fleet-demo` on the member (the ClusterPlatform instance creates it; the RGD reads it via `externalRef`). In `mode: gpu` the pods need the accelerator resource the platform names (`example.com/gpu` on kind: `kubectl get node -o jsonpath='{.items[0].status.allocatable}'`). |
| `Placed=False/DecisionPending` forever | No `PlacementDecision` named in `decisionRef` exists: the demo scheduler is not running, or the instance is annotated `scheduler.example.com/policy: manual` and nobody wrote one. |
| `gpus-free.example.com` missing on the hub | The inventory agent (`fleet-demo all` or `inventory-agent`) is not running, or the member lacks the About API CRD. |
| `Placed=False/DecisionViolatesRequirements` | The decision names a member that lacks a `requirements.matchProperties` property; fix the property (or the decision), never the requirement. |
