# The KubeCon EU 2027 demo — EKS + GKE + AKS, one kro API

The session abstract promises a live demo where **one kro API goes from a
single cluster to a fleet spanning cloud providers**, showing: storage
classes, load balancers and identity kept out of the developer's spec; one
readiness status across clusters; every cluster cleaned up with one delete.
This runbook is that demo. The scripts are the source of truth; this page is
the operator's guide around them.

Rehearse and fall back with `--simulated` (CPU nodes, a fake accelerator
resource, the mock model server). `--real-gpu` is the same demo on real GPU
node pools; the developer's instance is identical in both modes.

## What runs where

| Where | What | Why there |
|---|---|---|
| **Your laptop** | the hub: a kind cluster, the fleet controller, the demo inventory agent + scheduler, the gateway | No cross-cloud identity federation on the critical path: the hub reaches each cloud through that cloud's **own exec plugin** (`aws eks get-token`, `gke-gcloud-auth-plugin`, `kubelogin`) using the logins already on the laptop, via `ClusterProfile.status.accessProviders` (KEP-4322 / KEP-5339). It is the one component you fully control on stage, it makes the neutrality point (the hub is not one of the clouds), and a reset is instant and free. |
| EKS (Auto Mode), GKE, AKS | one member each: stock kro 0.9.4, the fleet RGDs, that cloud's `ClusterPlatform`, the About API properties | The three clouds the abstract names. Nothing fleet-aware runs on a member. |

A cloud-hosted hub (e.g. on GKE, reaching EKS and AKS through workload
identity federation) is a stretch goal, not built: it adds two federation
setups and token-expiry failure modes to a 25-minute slot for no visible
gain.

## Prerequisites

- CLIs: `kubectl`, `helm` (v4), `kind`, `docker`, `go` 1.26, `python3`, plus
  `aws` + `eksctl` (≥ 0.199, for `--enable-auto-mode`), `gcloud` +
  `gke-gcloud-auth-plugin`, `az` + `kubelogin` (`az aks install-cli`).
- Logged in: `aws sts get-caller-identity`, `gcloud auth login` with a
  project set, `az login`. `scripts/provision.sh` checks all of this first.
- `--real-gpu` only: GPU quota on all three clouds **before** the day
  (EKS: G-family vCPUs in `us-east-1`; GKE: `NVIDIA_L4_GPUS` in
  `us-central1`; AKS: `Standard NCASv3_T4 Family vCPUs` in `eastus`). Quota
  tickets take days; the simulated mode exists so the talk never depends on
  them.
- Cost (list prices, approximate): about **$1/hour simulated**, about
  **$9/hour real-gpu**, plus a few cents per load balancer. The provisioning
  script prints the table and asks before creating anything.

## Commands

```bash
scripts/provision.sh --simulated          # or --real-gpu; prints cost, asks, creates/reuses, installs kro
scripts/demo.sh --simulated               # the 11 steps; Enter between steps; logs under logs/
scripts/reset.sh                          # back to the start state between takes (keeps clusters + registration)
scripts/reset.sh --deregister             # ... and restart from step 1
scripts/teardown.sh                       # delete everything billable (asks first)
```

Flags on every script: `--dry-run` (print every command, create nothing;
also what CI runs), `--yes` (no prompts), and on `demo.sh`: `--step N`
(resume), `--no-pause` (hands-free rehearsal). Env knobs (names, regions,
sizes, `FAKE_GPUS`) are at the top of `scripts/lib/demo.sh`.

## The steps (what the audience sees)

| # | Step | The line |
|---|---|---|
| 1 | Register the fleet | Three clusters, three clouds, one inventory; the hub holds no cloud credential, each profile names the plugin that mints one. |
| 2 | Advertise properties | Each cloud advertises what it is and has (About API); the hub mirrors it. **No single cluster has eight free GPUs.** |
| 3 | The platform layer | The same RGDs everywhere; the only per-cloud file is the platform config (storage class, LB flavor, identity, what backs "gpu"). |
| 4 | The developer's instance | Eight replicas. No cluster, cloud, storage class, load balancer or identity named. |
| 5 | The placement decision | A demo scheduler divides by the GPUs each cloud advertises (total / free, net of what it already assigned) and writes a standard `PlacementDecision`; kro-fleet consumes it. |
| 6 | One readiness | Each cloud expands its slice with stock kro; one `Ready` on the hub with per-cluster detail; per-cloud storage classes shown. |
| 7 | One URL | The gateway spreads requests across the three clouds' own load balancers; every answer names its cloud. |
| 8 | Compliance | A FedRAMP-only instance lands only on the accredited cloud, reports the shortfall, never spills; an impossible requirement is a terminal refusal. |
| 9 | Drain | A cloud drains; its replicas move; readiness dips then recovers; its inventory record is gone. |
| 10 | Outage | A cloud goes unhealthy; kro-fleet stops touching it, the rest absorb what they can; it returns and is cleaned. |
| 11 | One delete | Every cloud's slice, volumes and load balancer go. |

Timing from rehearsal: steps 1–3 about 3 minutes, step 6 is the slow one
(first image pull per cloud), the whole run 12–18 minutes with narration.

## Backup video

Every run writes `logs/demo-<timestamp>/run.log` and one `step-NN.log` per
step, exactly what the terminal showed. Record a hands-free rehearsal
(`--no-pause`) with a screen recorder; the logs let you redo a cut without
re-provisioning.

## What is simulated, honestly

- `--simulated`: nodes advertise `example.com/gpu` through the documented
  node-status patch; the model server is `ghcr.io/danbruno101/mock-vllm:demo`
  requesting one `example.com/gpu`; everything else — placement, division,
  storage classes, load balancers, identity annotations, readiness, cleanup —
  is real. The reply `[mock:demo-llm@eks]` names the cloud it ran on.
- `--real-gpu`: `nvidia.com/gpu` + `vllm/vllm-openai:latest` on L4/T4 pools.
  Untested in this repository's CI (no GPUs there); exercise it once before
  the session.
- The inventory agent and the scheduler are demo stand-ins for a cluster
  manager and a placement producer (OCM, Karmada, Kueue); see
  `docs/KEP-GAP.md`.
- Health is self-asserted (`ControlPlaneHealthy` patched by the scripts);
  step 10 flips it by hand.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| A ClusterProfile never engages | Check the controller log (`logs/.../fleet-controller.log`) for the exec plugin error: expired cloud login (`aws sso login`, `gcloud auth login`, `az login`), or a missing plugin binary. |
| `gpus-free` stays `?` | The inventory agent needs the About API CRD on the member and the node capacity patch (simulated) or the GPU node pool (real). `kubectl --context eks get clusterproperties`. |
| Step 7 finds fewer than three endpoints | The cloud LB takes 1–3 minutes; AKS and GKE show an IP, EKS a hostname. `kubectl --context eks get svc -n fleet-demo`. |
| Readiness stuck below 8/8 on real GPUs | Image pull of vLLM is slow and the model downloads per replica; give it 5–10 minutes, or rehearse simulated. |
| A step was interrupted | `scripts/demo.sh --step N` resumes; `scripts/reset.sh` returns to the start state. |
