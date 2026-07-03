---
marp: true
theme: default
paginate: true
title: "Fleet-scoped KRO objects — a thin PoC"
description: "One object on a hub, placed across clouds, status folded back. Built on SIG-Multicluster, not beside it."
---

<!-- _class: lead -->

# Fleet-scoped KRO objects

**Author one object on a hub. It lands on every matching cluster, across
clouds. Status folds back onto the object you wrote.**

A thin PoC backing *KEP: Native Multi-Cluster Mode for KRO*
`github.com/danbruno101/kro-fleet`

---

## Where the story left off

The sister demo (`kro-genaiops-demo`) proved **portability**:

- one `ResourceGraphDefinition`, one ~10-line developer YAML
- identical behavior on kind / EKS / GKE / AKS
- no custom Go controller anywhere

Its honest limit: portability **by re-application**.
`ownerReferences` stop at the cluster boundary — N clusters means N applies,
N control loops, **no aggregated view**.

---

## The fleet gap

Fleets are the operating model: many homogeneous clusters, decoupled capacity.

What's missing from single-cluster KRO:

- **one authoring surface** for a workload that spans the fleet
- **placement as a field** of the same object — not a second system
- **status rolled back** onto the thing you applied

Engines exist (OCM, KubeFleet, Karmada, ApplicationSets) —
but composing them externally loses exactly those three properties.

---

## The scope decision (the whole design in one slide)

> **Do not build a propagation engine. Reuse the fabric.**

| Need | Reused standard |
|---|---|
| fleet registry + credentials | **ClusterProfile** (KEP-4322, cluster-inventory-api) |
| cross-cluster reconciliation | **multicluster-runtime** + its official ClusterProfile provider |
| graph expansion | **stock kro on each member** — unmodified |

The *only* new code: one hub-side placement controller.
If this PoC ever reimplements propagation, it has become Karmada — stop.

---

## Architecture

```
                      HUB (kind)
  ┌────────────────────────────────────────────────┐
  │ FleetGenAIService  {template, clusterSelector} │
  │ fleet placement controller (mc-runtime)        │
  │  resolve placement → SSA into members          │
  │  track → GC (finalizer) → aggregate status     │
  └──────────┬──────────────────────┬──────────────┘
     ClusterProfile + Secret   (inventory + creds)
  ┌───────────▼─────────┐  ┌────────▼────────────┐
  │ member-1 "gke"      │  │ member-2 "aks"      │
  │ stock kro + RGDs    │  │ stock kro + RGDs    │
  │ expands the graph   │  │ expands the graph   │
  │ PVC: premium-rwo    │  │ PVC: managed-csi    │
  └─────────────────────┘  └─────────────────────┘
```

---

## The authoring surface

```yaml
apiVersion: fleet.kro.run/v1alpha1
kind: FleetGenAIService
metadata: { name: demo-llm, namespace: fleet-demo }
spec:
  placement:                          # platform-owned
    clusterSelector: { matchLabels: { tier: prod } }
    tolerance: { minReadyClusters: 1 }
  template:
    spec:                             # the developer's GenAIService, verbatim
      name: demo-llm
      model: "Qwen/Qwen2.5-0.5B-Instruct"
      mode: mock
      replicas: 1
```

The template stays **cloud- and cluster-agnostic**. Placement is a field.

---

## The status surface

```yaml
status:
  clusters:
    - { name: kro-fleet-member-1, ready: true,  message: instance state ACTIVE }
    - { name: kro-fleet-member-2, ready: true,  message: instance state ACTIVE }
  summary: { placed: 2, ready: 2 }
  conditions:
    - type: Ready
      status: "True"          # ready >= tolerance.minReadyClusters
      reason: MinReadyClustersMet
```

One `kubectl get` answers "is my fleet converged?"

---

## What CI proves (all six, on kind, hermetic)

1. one hub object → workload **Ready on every matching member**
2. mutate the hub object → **all members converge**
3. add a matching ClusterProfile → workload **lands automatically**
4. unmatch a member → workload removed there, **no orphans**
5. delete the hub object → **fleet-wide GC**
6. `status.clusters[]` + rolled-up condition **correct** (tolerance honored)

Plus the portability beat: the *same* hub object binds
`premium-rwo` on the gke sim and `managed-csi` on the aks sim.

---

## What this PoC is NOT (KEP-GAP, the honest ledger)

| KEP (native ideal) | This PoC |
|---|---|
| expansion on the hub, inside kro | stock kro expands **on each member** |
| `status.accessProviders` credential plugins | labeled kubeconfig **Secret** strategy |
| cluster manager maintains health | health **asserted at registration** |
| dedicated applied-manifest inventory | `status.clusters[]` doubles as inventory |
| fleet scale | 3 kind clusters on a laptop |

Every row is deliberate; every row is documented in `docs/KEP-GAP.md`.

---

## Why this matters for the KEP discussion

- The **UX works**: placement as a field + folded-back status, demonstrated
  end to end with zero changes to kro.
- The **fabric holds**: ClusterProfile + multicluster-runtime carried the
  whole PoC — inventory, credentials, engagement lifecycle — with an
  official provider, out of the box.
- The remaining distance to "native" is **inside kro** (hub-side expansion,
  per-resource `placement: hub|members`) — precisely the part worth
  designing with the SIG rather than prototyping around.

---

<!-- _class: lead -->

## Try it

```bash
scripts/setup-fleet.sh 2
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml
scripts/e2e.sh        # the six criteria, asserted
```

**github.com/danbruno101/kro-fleet** — KEP draft in `docs/proposals/`
