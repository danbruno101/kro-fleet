# Re-baseline — October 2026 (for the KubeCon EU 2027 iteration)

The PoC was pinned in July 2026 (see [`phase0-validation.md`](phase0-validation.md)).
Before building the capacity use case on it, every upstream dependency was
re-checked against its current state on **2026-10-08**. This note records what
moved, what broke, and what new capability the iteration is built on — the same
discipline as phase 0: confirm the foundation, then build.

## Versions

| Component | July pin | October pin | Notes |
|---|---|---|---|
| `sigs.k8s.io/multicluster-runtime` | v0.24.1 | **v0.25.2** (tagged 2026-10-08) | No exported API change in `pkg/`. Pulls controller-runtime 0.25.2 and k8s 1.37. Go ≥ 1.26.0. |
| `…/providers/cluster-inventory-api` | v0.24.1 | **v0.25.2** | Adds the **AccessProvider** kubeconfig strategy; the **Secret strategy the PoC used is deprecated**. |
| `sigs.k8s.io/controller-runtime` | v0.24.1 | **v0.25.2** | Transitive. |
| `k8s.io/{api,apimachinery,client-go}` | v0.36.0 | **v0.37.1** | Transitive. |
| `sigs.k8s.io/cluster-inventory-api` | v0.1.3 | **v0.1.3** (still the latest tag) | `PlacementDecision` v1alpha1 is already in this tag (types + CRD). **v0.2.0 is pending** (issue #90, opened 2026-09-30): adds `v1alpha2` for ClusterProfile and PlacementDecision, drops `status.credentialProviders`, validates more strictly; v1alpha1 stays served and stays the storage version. |
| `sigs.k8s.io/about-api` (ClusterProperty) | — | commit **93309ed** (2026-09-14; the repo has no tags) | `about.k8s.io` v1alpha1 + v1beta1 (storage). Cluster-scoped, `spec.value` only. |
| kro | 0.9.2 | **0.9.4** (2026-09-04) | 0.10.0-rc.0 (2026-09-22) carries the KREP-024 "RGD on Graph" rework; not adopted yet. |
| sister repo `kro-genaiops-demo` | 9a2fa5c | **cf7ae45** | Must move with kro (below). |
| kind / node image | v0.32.0 / v1.36.1 | **v0.33.0** / **v1.37.0** | |
| kubectl / helm / controller-tools | v1.36.2 / v4.2.2 / v0.21.0 | **v1.37.1** / **v4.3.0** / **v0.22.0** | |
| `sigs.k8s.io/cloud-provider-kind` | — | v0.12.0 (2026-10-02) | Real `LoadBalancer` IPs on kind; used for the per-cloud load-balancer beat. |
| `sigs.k8s.io/work-api` | — | untagged master (2026-09-14) | Evaluated, not adopted — see below. |

## What broke

- **kro ≥ 0.9.3 rejects the sister RGD at the July pin.** The RGD declared
  `status.conditions: '${deployment.status.conditions}'` (a string) and kro
  now refuses a graph whose `status.conditions` is not a list. Sister `main`
  renamed the field to `deploymentConditions` and switched the Deployment to
  `strategy: Recreate` (shared RWO cache PVC). The kro and sister pins
  therefore move together.
- **The Secret kubeconfig strategy is deprecated** in the provider. The
  controller now uses the AccessProvider strategy: each `ClusterProfile`
  advertises `status.accessProviders[]` (KEP-4322 / KEP-5339) and the
  controller resolves the provider name through an exec credential plugin —
  on kind, the upstream `kubeconfig-secretreader` plugin, which reads the
  member's kubeconfig Secret on the hub at credential time. The controller
  itself never reads a kubeconfig Secret any more. This closes the "member
  credentials simplified" row of `KEP-GAP.md`, and it is the same mechanism
  the cloud demo uses with `aws eks get-token`, `gke-gcloud-auth-plugin` and
  `kubelogin`.
- Nothing else: the multicluster-runtime bump compiled without a source change.

## New capabilities this iteration builds on

- **`PlacementDecision` (KEP-5313)** — namespaced, data-only, ≤ 100 clusters
  per slice, labels `multicluster.x-k8s.io/{decision-key,decision-index,placement-key}`,
  a per-cluster `reason`, `schedulerName`. Consumers are read-only. It carries
  **no per-member parameters** and **no decision-level reason** (an empty
  `decisions: []` is indistinguishable from "not decided yet" without a
  convention), and consumer feedback is explicitly out of scope. The
  conventions this PoC adopts for those two gaps, and the ask to
  SIG-Multicluster, are in [`placement-decision-gaps.md`](placement-decision-gaps.md).
- **`ClusterProfile.status.properties`** — KEP-4322 says cluster managers
  SHOULD mirror a member's About API `ClusterProperty` objects into it,
  name and value as-is. That gives a standards-shaped path from "a cluster
  advertises its properties" to "a scheduler reads the hub".
- **Published exec-plugin images** for the access-provider mechanism
  (`registry.k8s.io/cluster-inventory-api/{secretreader,kubeconfig-secretreader}`).
- **Producer-side adoption**: OCM's support for the standard PlacementDecision
  is "in progress" (ocm#1373) and Argo CD has a generator proposal
  (argo-cd#27663), so the demo scheduler is replaceable in principle, which
  is the property the KEP claims.

## Work API — evaluated, not adopted

`kubernetes-sigs/work-api` is alive but pre-release: current SIG-Multicluster
maintainers in `OWNERS`, dependencies current (k8s 0.36.4, controller-runtime
0.24.1), commits in September 2026 — but no release or image has ever been
cut, and the Azure fork (`Azure/k8s-work-api`) was archived on 2026-06-15.
Measured on the agent at master:

- it sets only the `Applied` condition, per manifest and per `Work`;
  `Available`, `Progressing` and `Degraded` are documented in the type but
  never set — **readiness cannot be derived from Work status**;
- its finalize controller contains `// TODO add clean resource logic`:
  **deleting a Work does not remove what it applied**, which fails
  "every cluster cleaned up with one delete" outright.

Open Cluster Management's `ManifestWork` (with `feedbackRules`) is the only
production implementation of the idea. Decision: keep multicluster-runtime
as the one delivery back end, keep a small delivery seam so a ManifestWork
back end can be added later, and ledger the option in `KEP-GAP.md`.

## Sandbox note (not needed in CI)

Some restricted dev sandboxes lack `CAP_SYS_RESOURCE`; `runc` then fails
with `failed to update /proc/self/oom_score_adj: Permission denied` and no
kind cluster bootstraps. The phase 0 workaround still applies: a derived node
image whose `runc` is wrapped to strip `process.oomScoreAdj` from the bundle
before exec'ing the real binary. GitHub Actions runners do not need it.
