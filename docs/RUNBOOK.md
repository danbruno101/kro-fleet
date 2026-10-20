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
| members | stock **kro 0.9.4** (Helm, `registry.k8s.io`), the About API `ClusterProperty` CRD + that persona's properties (`docs/properties.md`; member-2 = aks is the one `fedramp-high` member, member-3 = eks the cheapest tier), **4 fake `example.com/gpu`** on the node |
| hub | one `ClusterProfile` per member (labels `tier=prod`, `fleet.kro.run/cloud=…`) whose `status.accessProviders[]` points the `kubeconfig-secretreader` exec plugin at that member's kubeconfig `Secret` (KEP-4322 / KEP-5339); the plugin binary is built into `bin/` |
| hub → members | the two fleet definitions (`config/fleet-rgd/`, derived from the sister repo's) as `FleetResourceGraphDefinition`s on the hub; a **bootstrap fleet controller** (built into `bin/`, stopped when setup finishes) hands each member a plain `ResourceGraphDefinition`, the script waits until the hub reports both `Accepted` on every member, then applies one `ClusterPlatform` per simulated cloud (member-1 = gke/`premium-rwo`, member-2 = aks/`managed-csi`, member-3 = eks/`gp3`) |

Members run **zero fleet-aware code** — and nobody applies an RGD to a member
by hand. `BOOTSTRAP_CONTROLLER=false` skips the setup-time controller when one
is already running against the hub.

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

## 2a. The definitions, authored once on the hub

```bash
kubectl --context kind-kro-fleet-hub get frgd
# NAME                      KIND              DISTRIBUTED   ACCEPTED   MEMBERS   AGE
# clusterplatform.kro.run   ClusterPlatform   True          True       3/3       9m
# genaiservice.kro.run      GenAIService      True          True       3/3       9m
kubectl --context kind-kro-fleet-hub get frgd genaiservice.kro.run \
  -o jsonpath='{range .status.members[*]}{.name}: hash={.appliedHash} {.state} ready={.ready} kro={.kroVersion}{"\n"}{end}'
kubectl --context kind-kro-fleet-member-1 get rgd      # the plain ResourceGraphDefinition stock kro accepted there
```

Every value under `status.members[]` is the member's own kro-reported state,
read back through the member watch — never inferred on the hub. `DISTRIBUTED`
says every targeted member holds the current `status.specHash`; `ACCEPTED`
says every member's kro reports it Active (False names the first member that
does not, with its reason). Edit the wrapper's `spec.resourceGraphDefinition`
and every member's copy follows (its `fleet.kro.run/spec-hash` label changes);
set `spec.distribution.enabled: false` or a `memberSelector` and the definition
is removed where it no longer belongs — **unless** hub-placed instances of its
kind still exist there (`kubectl get frgd … -o jsonpath='{.status.conditions[?(@.type=="Deleting")]}'`
then says `BlockedByInstances`, naming the members and counts; it proceeds on
its own once they are gone). An instance is only placed onto a member once its
definition reports that member Active; until then the member appears in the
instance's `status.clusters[]` with reason `RGDNotReady`.

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
scripts/e2e.sh    # runs all twenty criteria; same script CI runs (~45 min)
E2E_FROM=8 scripts/e2e.sh   # dev: only the decision/demo scenarios, on a fleet that is already up
E2E_FROM=13 scripts/e2e.sh  # dev: only the definition-distribution scenarios (14, 15, 17-20)
E2E_LOADBALANCER=false scripts/e2e.sh   # skip cloud-provider-kind + criterion 12 (hosts that cannot resolve kind node names)
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
| `Placed=False/RGDNotReady`, a member listed with reason `RGDNotReady` | The `FleetResourceGraphDefinition` for `GenAIService` does not report that member Active: `kubectl get frgd genaiservice.kro.run -o yaml` shows the member's own kro reason (not distributed there, an older hash, or a rejected graph — e.g. a resource kind that member's API server does not have). Nothing is placed there until it is. |
| `frgd` stuck Terminating, `Deleting=False/BlockedByInstances` | Instances of that kind placed by the hub still exist on the named members (stock kro would delete the generated CRD, and every instance with it). Delete or unplace those `FleetGenAIService`s; the definition is removed on its own afterwards. |
| `kubectl get rgd` on the hub says the resource type is unknown | Correct: no kro runs on the hub. Definitions live there as `frgd`; the plain `rgd` exists only on members. |
