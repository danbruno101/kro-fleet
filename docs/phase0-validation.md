# Phase 0 — foundation validation (PASSED)

Phase 0 of the build plan (see `CLAUDE.md`) required validating, **before writing
any FleetGenAIService code**, that the SIG-Multicluster foundation actually works:
two kind clusters, ClusterProfile CRDs, one cluster registered as a ClusterProfile
on the other, multicluster-runtime wired with a ClusterProfile provider, and a
trivial object (a ConfigMap) reconciled hub → member.

**Result: the foundation works as assumed.** Validated on 2026-07-03 with the
harness in [`hack/phase0/main.go`](../hack/phase0/main.go).

## Confirmed versions (pinned for the PoC)

| Component | Version | Notes |
|---|---|---|
| kind | v0.32.0 | installed via `go install sigs.k8s.io/kind@v0.32.0` |
| kindest/node | v1.36.1 (`sha256:3489c767…`) | Kubernetes v1.36.1 |
| `sigs.k8s.io/cluster-inventory-api` | **v0.1.3** | ClusterProfile CRD (`multicluster.x-k8s.io/v1alpha1`), KEP-4322 |
| `sigs.k8s.io/multicluster-runtime` | **v0.24.1** | versioned in lockstep with controller-runtime v0.24.x; requires go ≥ 1.26.3 |
| `sigs.k8s.io/multicluster-runtime/providers/cluster-inventory-api` | **v0.24.1** | **official ClusterProfile provider — exists and works**, separate Go module |
| `sigs.k8s.io/controller-runtime` | v0.24.1 | transitive |
| k8s.io/api & friends | v0.36.0 | transitive |

Key discovery: multicluster-runtime ships an **official cluster-inventory-api
provider** (out-of-tree module, tagged `providers/cluster-inventory-api/v0.24.1`).
We did not have to write a provider — exactly the "reuse the fabric" outcome the
scope decision demands.

## How the provider consumes ClusterProfiles

- The provider runs a controller on the hub watching `ClusterProfile` objects
  (any namespace). Cluster names are `<namespace>/<name>` (e.g.
  `fleet-system/member-1`).
- **Readiness gate:** by default a ClusterProfile is only engaged when its status
  has condition `ControlPlaneHealthy=True` (overridable via `Options.IsReady`).
- **Credentials** come from a pluggable `kubeconfigstrategy`:
  - `Secret` strategy (used here): a Secret in the ClusterProfile's namespace
    labeled `x-k8s.io/cluster-inventory-consumer=<consumer>` and
    `x-k8s.io/cluster-profile=<name>`, with the kubeconfig under data key
    `Config`. Kubeconfig changes are detected and the cluster is re-engaged.
  - `CredentialsProvider` strategy: the KEP-5339 `status.accessProviders` /
    exec-plugin mechanism via `sigs.k8s.io/cluster-inventory-api/pkg/access`.
    Not used in the PoC (ledgered in `KEP-GAP.md`).

## What was proven end to end

With the harness running against the hub (`go run ./hack/phase0`):

1. **Engagement** — the provider engaged `fleet-system/member-1` from its
   ClusterProfile + labeled kubeconfig Secret.
2. **Placement** — a ConfigMap in `fleet-demo` on the hub labeled
   `fleet.kro.run/place=true` was server-side-applied into the member
   (field manager `kro-fleet-phase0`).
3. **Convergence** — mutating the hub ConfigMap converged the member copy.
4. **Drift repair** — hand-editing the member copy was reverted from the hub
   source (member watch events flow through the same multicluster reconciler).
5. **Dynamic registration** — deleting and re-creating the ClusterProfile
   disengaged and re-engaged the member while the controller stayed up;
   convergence resumed immediately.

## Caveats / environment notes

- **ClusterProfile health is self-asserted in the PoC.** Nothing on a bare kind
  fleet sets `ControlPlaneHealthy`; we patch the status condition manually at
  registration time. A real fleet has a cluster-manager agent doing this.
  Ledgered in `KEP-GAP.md`.
- **kubelet ≥ 1.36 refuses cgroup v1 hosts** unless
  `KubeletConfiguration.failCgroupV1: false` is set — baked into
  [`config/kind/cluster.yaml`](../config/kind/cluster.yaml) (no-op on cgroup v2
  hosts such as GitHub Actions runners).
- Restricted dev sandboxes (like the one this validation ran in) may lack
  `CAP_SYS_RESOURCE` and the `name=systemd`/`cpuset` cgroup v1 hierarchies;
  standard CI runners do not have these problems. Workarounds used here (not
  needed in CI): mount the missing v1 hierarchies, and wrap runc inside the
  node image to neutralize `oomScoreAdj`.
- Docker Hub's blob CDN may be blocked by egress policy; `mirror.gcr.io`
  serves the same `kindest/node` images.
