# PlacementDecision (KEP-5313) — what kro-fleet consumes, and two gaps for SIG-Multicluster

kro-fleet consumes `PlacementDecision` (`multicluster.x-k8s.io/v1alpha1`,
`sigs.k8s.io/cluster-inventory-api` v0.1.3) as its placement input: a
scheduler writes the decision, kro-fleet converges on it and re-converges
when it changes. It never ranks, scores or selects. This note records
exactly what is consumed from the standard, the two things the standard does
not carry that the capacity and compliance cases need, the conventions this
PoC uses for them, and the ask. It is written up here, as a proposal to the
SIG, rather than silently extending their API.

## What is consumed, as specified

| KEP-5313 | How kro-fleet uses it |
|---|---|
| Namespaced, data-only `decisions[]` of `clusterProfileRef {name, namespace}` | `spec.placement.decisionRef.name` reads one decision in the instance's namespace; `spec.placement.placementKey` lists slices by the `multicluster.x-k8s.io/placement-key` label and merges them in `multicluster.x-k8s.io/decision-index` order. Every reference must resolve to a `ClusterProfile` in the fleet namespace; an unknown one refuses the whole decision. |
| `clusterProfileRef.namespace` unset = the decision's namespace | Honored. Our decisions live next to the instance (`fleet-demo`) while profiles live in `fleet-system`, so producers set the namespace explicitly. |
| Per-cluster `reason`, `schedulerName` | Copied into `status.placement` (provenance) on the instance. |
| Consumers are read-only | kro-fleet only `get`s/`list`s/`watch`es decisions. |
| Delete = remove workloads from the listed clusters | A deleted decision makes the placement *pending* (`Placed=False/DecisionPending`) and the copies are unplaced through the inventory. |

## Gap 1 — per-member parameters

The capacity case divides one unit across clusters: 8 replicas as 3 + 3 + 2.
`PlacementDecision` answers "which clusters", not "how much each". Without
per-member values the only possible semantics is replication (every member
runs the whole template), which is exactly what the capacity case cannot
use.

**Convention in this PoC** (producer-set, consumer-read annotations on the
decision — the least invasive carrier: no new kind, it travels with the
decision consumers already watch, any producer can set it, and when absent
the behaviour degrades to plain replication):

```yaml
metadata:
  annotations:
    parameters.fleet.kro.run/eks-prod: '{"replicas":"4"}'   # one per ClusterProfile name
    parameters.fleet.kro.run/gke-prod: '{"replicas":"2"}'
```

kro-fleet understands `replicas` today. Parameters alter *values* of
existing template fields only, never the graph's shape — the constraint the
KEP states for per-member parameters.

**Proposal to the SIG:** an optional, open-ended field on `ClusterDecision`,
e.g. `parameters: map[string]string`, or an explicit statement that
producer annotations are the sanctioned extension point. Either removes the
need for consumers and producers to agree out of band.

## Gap 2 — a decision-level reason (refusal vs latency)

An empty `decisions: []` is valid, but from where a consumer sits it looks
the same as "the scheduler has not decided yet". `reason` lives on each
cluster entry, so an empty decision has nowhere to say "nothing qualifies".
The compliance case needs that distinction: no compliant cluster must be a
**terminal, auditable refusal** — never "not ready yet", and never a
fallback to a broader set.

**Convention in this PoC:**

- decision **absent** → pending (`Placed=False/DecisionPending`);
- decision **present and empty** → terminal refusal
  (`Placed=False/NoEligibleClusters`), with the producer's reason read from
  the annotation `fleet.kro.run/decision-reason` when present;
- decision present but assigning **fewer replicas than requested** → the
  decided slice is placed, nothing is spilled elsewhere, and the instance
  reports `Placed=False/InsufficientCapacity` with the same annotation as
  explanation.

**Proposal to the SIG:** either a decision-level `reason`/`conditions` on
`PlacementDecision`, or a written convention that producers publish nothing
until they have an answer and that an empty decision is a deliberate
refusal. Either works for consumers; today a consumer cannot tell refusal
from latency on its own.

## What kro-fleet adds on its own side (not an ask)

- `spec.placement.requirements.matchProperties` — hard constraints over
  `ClusterProfile.status.properties`. They are *input* for the producer and
  a *guard* for the consumer: kro-fleet refuses, as a whole, a decision that
  names a cluster violating them (`Placed=False/DecisionViolatesRequirements`).
  A constraint expressed on the kro object, evaluated by the scheduler,
  enforced fail-closed by kro.
- `status.placement` — the resolved decision (source, decision names,
  scheduler, reason, per-member reason and parameters), so a regulated user
  can show after the fact where a workload ran and why it was permitted.

## Producer swappability

The demo scheduler in this repository is deliberately trivial and labeled
demo-only. OCM's support for the standard `PlacementDecision` is in progress
(ocm#1373) and Argo CD has a generator proposal (argo-cd#27663); an OCM- or
Karmada-backed producer that writes the standard object would drive
kro-fleet unchanged — minus the two conventions above, which is precisely
why they are written up here.
