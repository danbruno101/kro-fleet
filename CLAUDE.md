# CLAUDE.md — kro-fleet

Durable project context for Claude Code working in this repo. Read this and
the three source-of-truth documents before doing anything:
`docs/design/fleet-scoped-kro.md` (problem framing),
`docs/proposals/KEP-kro-multicluster.md` (the KEP, v2), and
`docs/KEP-GAP.md` (proposed-vs-built ledger).

## What this repo is

A **thin proof-of-concept** for fleet-scoped KRO objects: one placement-enabled
object on a **hub** cluster is placed onto matching **member** clusters, with
per-member status aggregated back on the hub. It is the reference implementation
backing `docs/proposals/KEP-kro-multicluster.md`.

Sister project (reuse heavily): https://github.com/danbruno101/kro-genaiops-demo —
its `GenAIService` + `ClusterPlatform` RGDs and the `ghcr.io/danbruno101/mock-vllm:demo`
image are the workload placed across the fleet.

## Scope decision — DO NOT DEVIATE

- **Do NOT fork or modify `kubernetes-sigs/kro`.** Run **stock kro** (released Helm
  chart) on each **member**; it does local graph expansion. The ONLY new code is a
  **hub-side fleet placement controller**.
- Build **on SIG-Multicluster standards**, never a new propagation engine:
  - `ClusterProfile` / Cluster Inventory API (`kubernetes-sigs/cluster-inventory-api`,
    KEP-4322) — fleet registry + credentials via `status.accessProviders`.
  - `multicluster-runtime` (`kubernetes-sigs/multicluster-runtime`) — cross-cluster
    reconciliation via a provider.
- "Native mode inside kro" (expand-on-hub) is **future work** — document the gap in
  `docs/KEP-GAP.md`, do not build it here.
- If you ever find yourself reimplementing propagation, STOP — that means the design
  is drifting toward Karmada. Reuse the fabric.

## Architecture

- **Hub:** ClusterProfile CRDs installed; a `FleetGenAIService` (carries the
  GenAIService spec/ref + `placement.clusterSelector`); the placement controller
  (Go, multicluster-runtime) resolves placement, server-side-applies the
  GenAIService into each matching member, tracks applied manifests per
  `(object, member)`, GCs on unplace/delete via a finalizer, and aggregates member
  status into `status.clusters[]` + a rolled-up condition (honor a `tolerance`).
- **Members (unmodified):** stock kro + the sister repo's RGDs + a `ClusterPlatform`
  instance per "cloud"; kro expands the placed GenAIService locally, resolving each
  member's StorageClass per cloud.
- **Placement is platform-owned;** the distributed GenAIService stays
  cloud/cluster-agnostic.

## Build phases

0. **Validate the foundation FIRST.** 2 kind clusters, install ClusterProfile CRDs,
   register one as a ClusterProfile on the other, wire multicluster-runtime with a
   ClusterProfile provider, and reconcile a TRIVIAL object (ConfigMap) hub→member.
   Confirm current API/provider versions. **Present a plan before building the rest.**
1. The `FleetGenAIService` type + the placement controller (place → track → GC →
   aggregate).
2. `scripts/setup-fleet.sh` / `teardown-fleet.sh` (hub + N members; ClusterProfiles;
   stock kro + sister RGDs on members).
3. Hermetic CI proving the full loop; runbook; Marp deck; `docs/KEP-GAP.md`.

## Conventions

- **kind + pinned versions** for everything (kro, multicluster-runtime,
  cluster-inventory-api CRDs). Reproducible, hermetic CI. No cloud, no GPU.
- Reuse the sister repo's mock images; do not build new ML.
- Go for the controller; **Apache-2.0 license headers** on source files.
- Every doc/README stays HONEST about PoC-vs-native (see `docs/KEP-GAP.md`).
- No secrets, tokens, kubeconfigs, or credentials committed — ever.

## Success criteria (CI must prove)

- [ ] One `FleetGenAIService` on the hub → workload Ready on every matching member.
- [ ] Mutating the hub object → all members converge.
- [ ] Adding a matching ClusterProfile → workload lands automatically.
- [ ] Removing/unmatching a member → workload removed there, **no orphans**.
- [ ] Deleting the hub object → all placed objects on all members GC'd.
- [ ] `status.clusters[]` reflects per-member readiness + a correct rolled-up condition.

## Working autonomously — how to operate

You may proceed WITHOUT asking for:
- Scaffolding (go.mod, Makefile, hack/ scripts, kind configs, GH Actions CI).
- Writing/refactoring the controller, tests, scripts, docs, and the deck.
- Choosing library-level details, as long as they honor the scope decision.
- Committing to a feature branch and opening **draft PRs**.

ASK the user first when:
- A choice contradicts the scope decision (e.g. anything requiring a kro fork).
- Phase 0 reveals the multicluster-runtime + ClusterProfile foundation does NOT work
  as assumed — surface it and propose alternatives before building on it.
- An architecturally significant fork appears (e.g. push vs pull, a different
  inventory source) — recommend one, but confirm.

### Git / PR workflow
- Never commit directly to `main`. Work on a feature branch; open a **draft PR**.
- Keep commits focused with clear messages. Keep CI green at each step.
- Update `docs/KEP-GAP.md` as the honest ledger whenever the PoC simplifies vs the KEP.
- Prefer small, reviewable PRs over one giant drop.

### Document consistency contract
Three documents divide the truth; do not let claims migrate between them:
- **`docs/proposals/KEP-kro-multicluster.md`** owns *promises*: goals, API
  shape, design details. What we intend to be true.
- **`docs/design/fleet-scoped-kro.md`** owns the *problem and open questions*
  for SIG-Multicluster. It cross-references the KEP; it must not restate or
  fork the KEP's API.
- **`docs/KEP-GAP.md`** owns the *proposed-vs-built delta*. It is the ONLY
  place allowed to describe what actually runs. If the KEP and the code
  disagree, KEP-GAP says so — never silently reconcile either side.

Rules that follow:
- Any PR touching the KEP's Goals / API sketch / Design details **must update
  `docs/KEP-GAP.md` in the same PR**, or include the literal phrase
  `KEP-GAP: no change needed` in the PR body with a one-line reason. CI
  enforces this (`.github/workflows/kep-gap-check.yaml`).
- When the KEP's placement or API language changes, `grep -rn KEP api/
  internal/ cmd/` — code comments (`api/v1alpha1/types.go`, the controller)
  restate KEP claims and must be updated, or the mismatch recorded in KEP-GAP.
- Presentation and repo-facing docs (`README.md`, `docs/deck.md`,
  `docs/RUNBOOK.md`, `docs/demo-script.md`, phase0 docs,
  `docs/proposals/kro-fleet-mvp-plan.md`, `headlamp-plugin/README.md`) are NOT
  part of the contract: sweep them once at the end of an editing pass, not per
  edit. The sweep is real — most drift found in review lived in exactly these
  files.

### Commit attribution — NON-NEGOTIABLE
- Claude is only ever **"Assisted-by"**, never a co-author. Use the trailer
  `Assisted-by: Claude <noreply@anthropic.com>`. NEVER use `Co-Authored-By:` for
  Claude — GitHub treats that as real co-authorship and adds Claude to the
  repo's contributors list, which violates the contribution guidelines Daniel
  works under. Claude must also never be the commit *author* or *committer*.
- **Verify the identity before the first commit and push of a session.** This
  repo is owned by `danbruno101`; commits must be authored as
  `Daniel Bruno <daniel.c.bruno@gmail.com>` and pushed as `danbruno101`. Since
  the 2026-08-31 account consolidation the machine's global git identity is the
  personal address and the old work account (`dbruno-bt`) is retired — but the
  check stays cheap and the failure stays expensive, so run it anyway:
  ```bash
  git config user.email          # must be daniel.c.bruno@gmail.com
  gh auth status                 # active account must be danbruno101
  ```
  This has gone wrong before: dbruno-bt and Claude both appeared in this repo's
  contributors list and the entire history had to be rewritten to remove them.

### Definition of done for a change
`go build` + `go vet` clean; unit tests pass; the kind e2e (or the relevant subset)
passes locally/CI; docs updated; KEP-GAP kept honest.

## Suggested layout
```
cmd/           # controller entrypoint
api/           # FleetGenAIService types
internal/      # controller, placement, applied-manifest tracking, status agg
scripts/       # setup-fleet.sh, teardown-fleet.sh
examples/      # FleetGenAIService samples
config/        # CRDs, RBAC, kind configs
docs/          # RUNBOOK.md, KEP-GAP.md, deck, design/fleet-scoped-kro.md,
               # proposals/KEP-kro-multicluster.md
headlamp-plugin/     # the fleet dashboard (Headlamp plugin)
.github/workflows/   # hermetic kind CI
```
