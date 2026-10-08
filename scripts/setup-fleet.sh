#!/usr/bin/env bash
# =============================================================================
# setup-fleet.sh — stand up the kro-fleet PoC: 1 hub + N member kind clusters.
#
#   hub:     ClusterProfile + PlacementDecision CRDs (cluster-inventory-api),
#            the fleet.kro.run CRDs, one ClusterProfile per member whose
#            status.accessProviders points an exec credential plugin at that
#            member's kubeconfig Secret (KEP-4322 / KEP-5339).
#   members: stock kro (pinned Helm chart) + the fleet RGDs (config/rgd/:
#            GenAIService + ClusterPlatform, derived from the sister repo's)
#            + one ClusterPlatform instance per simulated cloud + the About
#            API ClusterProperty CRD with that cloud persona's properties +
#            a fake accelerator resource (example.com/gpu) on the node so the
#            capacity story runs with zero GPUs. Members are UNMODIFIED kro —
#            the only fleet-aware code anywhere runs on the hub.
#
# Usage:  scripts/setup-fleet.sh [num-members]        (default: 2)
# Then:   go run ./cmd/fleet-controller --hub-context kind-${PREFIX}-hub \
#           --kubeconfig-secretreader-plugin bin/kubeconfig-secretreader-plugin
#         kubectl --context kind-${PREFIX}-hub apply -f examples/fleetgenaiservice-sample.yaml
#
# Everything is pinned for reproducibility. All knobs may be overridden via env.
# =============================================================================
set -euo pipefail

MEMBERS="${1:-2}"
PREFIX="${PREFIX:-kro-fleet}"
FLEET_NS="${FLEET_NS:-fleet-system}"          # hub: ClusterProfiles + Secrets
WORKLOAD_NS="${WORKLOAD_NS:-fleet-demo}"      # everywhere: placed workloads
CONSUMER="${CONSUMER:-kro-fleet}"             # ClusterProfile spec.clusterManager.name
TIER="${TIER:-prod}"                          # placement label on every member

# --- pinned versions ---------------------------------------------------------
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}"
KRO_CHART="${KRO_CHART:-oci://registry.k8s.io/kro/charts/kro}"
KRO_VERSION="${KRO_VERSION:-0.9.4}"   # OCI tag (chart reports itself as v0.9.4)
# The fleet RGDs in config/rgd/ are derived from the sister project at commit
# cf7ae45 (kro >= 0.9.3 rejects the older RGD's `status.conditions` string).
# cluster-inventory-api / about-api pins live in scripts/lib/fleet.sh.

# Simulated accelerators: each member advertises this many units of this
# extended resource on its node (the documented node-status PATCH mechanism),
# and the workload requests one per replica in gpu mode. 0 disables.
FAKE_GPUS="${FAKE_GPUS:-4}"
FAKE_GPU_RESOURCE="${FAKE_GPU_RESOURCE:-example.com/gpu}"

# PRELOAD_IMAGES=true pulls the member-side images on the host and `kind load`s
# them into each member. Needed on hosts where the nodes cannot reach registries
# directly (e.g. behind a localhost egress proxy the node containers can't see);
# also cuts pull time in CI.
PRELOAD_IMAGES="${PRELOAD_IMAGES:-false}"
KRO_IMAGE="${KRO_IMAGE:-registry.k8s.io/kro/kro:v${KRO_VERSION#v}}"
MOCK_IMAGE="${MOCK_IMAGE:-ghcr.io/danbruno101/mock-vllm:demo}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CONFIG="${REPO_ROOT}/config/kind/cluster.yaml"
HUB="${PREFIX}-hub"
HUB_CTX="kind-${HUB}"
PLUGIN_BIN="${PLUGIN_BIN:-${REPO_ROOT}/bin/kubeconfig-secretreader-plugin}"

# shellcheck source=lib/fleet.sh
. "${REPO_ROOT}/scripts/lib/fleet.sh"

# Simulated clouds cycled across members: name + the StorageClass each cloud
# would ship. On kind the class is minted with the local-path provisioner
# (manageStorageClass: true), mirroring the sister repo's cloud simulations.
CLOUDS=(gke aks eks)
CLASSES=(premium-rwo managed-csi gp3)
# Cloud persona properties (About API ClusterProperty on each member; the demo
# vocabulary is documented in docs/properties.md). The aks persona is the one
# accredited member for the compliance beat; eks is the cheapest tier.
REGIONS=(us-central1 eastus us-east-1)
COST_TIERS=(2 3 1)
CAPACITY_TYPES=(on-demand spot on-demand)
COMPLIANCE=("" fedramp-high "")

log() { fleet::log "$@"; }

have_cluster() { kind get clusters 2>/dev/null | grep -qx "$1"; }

create_cluster() {
  local name=$1
  if have_cluster "$name"; then
    log "kind cluster $name already exists, skipping create"
  else
    log "creating kind cluster $name"
    kind create cluster --name "$name" --image "$KIND_NODE_IMAGE" \
      --config "$KIND_CONFIG" --wait 240s
  fi
}

# --- hub ----------------------------------------------------------------------
create_cluster "$HUB"
fleet::install_hub_crds
kubectl --context "$HUB_CTX" create namespace "$FLEET_NS" --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -
kubectl --context "$HUB_CTX" create namespace "$WORKLOAD_NS" --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -

log "building the kubeconfig-secretreader exec plugin -> ${PLUGIN_BIN}"
fleet::build_plugin "$PLUGIN_BIN"

# --- members -------------------------------------------------------------------
for i in $(seq 1 "$MEMBERS"); do
  member="${PREFIX}-member-${i}"
  ctx="kind-${member}"
  idx=$(( (i-1) % ${#CLOUDS[@]} ))
  cloud="${CLOUDS[$idx]}"
  class="${CLASSES[$idx]}"
  region="${REGIONS[$idx]}"
  cost_tier="${COST_TIERS[$idx]}"
  capacity_type="${CAPACITY_TYPES[$idx]}"
  compliance="${COMPLIANCE[$idx]}"

  create_cluster "$member"

  if [ "$PRELOAD_IMAGES" = "true" ]; then
    log "[$member] preloading images into the node"
    for img in "$KRO_IMAGE" "$MOCK_IMAGE"; do
      docker pull -q "$img"
      # Not `kind load docker-image`: with Docker's containerd image store it
      # exports the multi-arch index and `ctr import --all-platforms` then
      # fails on never-pulled foreign blobs. Import host-platform-only instead.
      docker save "$img" | docker exec -i "${member}-control-plane" \
        ctr --namespace=k8s.io images import --digests -
    done
  fi

  log "[$member] installing stock kro ${KRO_VERSION} (helm)"
  helm upgrade --install kro "$KRO_CHART" --version "$KRO_VERSION" \
    --kube-context "$ctx" -n kro --create-namespace --wait --timeout 5m

  log "[$member] installing the About API ClusterProperty CRD (about-api ${ABOUT_API_REF:0:12})"
  fleet::install_member_crds "$ctx"

  log "[$member] advertising cluster properties (About API) for the ${cloud} persona"
  fleet::apply_properties "$ctx" "$member" "$cloud" "$region" "$cost_tier" "$capacity_type" "$compliance"

  if [ "$FAKE_GPUS" != "0" ]; then
    log "[$member] advertising ${FAKE_GPUS} x ${FAKE_GPU_RESOURCE} on the node (simulated accelerators)"
    fleet::fake_accelerators "$ctx" "$FAKE_GPU_RESOURCE" "$FAKE_GPUS"
  fi

  log "[$member] applying the fleet RGDs (config/rgd/, derived from the sister repo)"
  kubectl --context "$ctx" apply -f "${REPO_ROOT}/config/rgd/"
  kubectl --context "$ctx" wait rgd/genaiservice.kro.run rgd/clusterplatform.kro.run \
    --for=jsonpath='{.status.state}'=Active --timeout=120s

  log "[$member] applying ClusterPlatform instance (cloud=${cloud}, region=${region}, storageClass=${class})"
  kubectl --context "$ctx" create namespace "$WORKLOAD_NS" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
  # The GenAIService RGD resolves each cluster's StorageClass from the
  # genaiops-platform-config ConfigMap this instance emits — it must live in the
  # namespace workloads are placed into (kro expands children into the
  # instance's namespace, and the RGD's externalRef reads from its own).
  cat <<EOF | kubectl --context "$ctx" apply -f -
apiVersion: kro.run/v1alpha1
kind: ClusterPlatform
metadata:
  name: platform
  namespace: ${WORKLOAD_NS}
spec:
  cloud: ${cloud}
  region: ${region}
  storageClass: ${class}
  manageStorageClass: true          # kind sims mint the cloud's named class
  provisioner: rancher.io/local-path
  makeDefault: false
  acceleratorResource: ${FAKE_GPU_RESOURCE}   # what backs "gpu" here (simulated)
  # servingImage / servingCPU / servingMemory keep their defaults: the mock
  # stand-in, sized for a laptop. Real GPU pools set vLLM and real requests.
EOF
  # kro states are cased differently: RGDs report "Active", instances "ACTIVE".
  kubectl --context "$ctx" -n "$WORKLOAD_NS" wait clusterplatform/platform \
    --for=jsonpath='{.status.state}'=ACTIVE --timeout=120s

  # Register on the hub: ClusterProfile + accessProviders + kubeconfig Secret.
  kc="$(mktemp -t "${member}-XXXX.kubeconfig")"
  kind get kubeconfig --name "$member" > "$kc"
  fleet::register_member "$member" "$cloud" "$kc" "$TIER"
  rm -f "$kc"
done

log "fleet ready: hub=${HUB} members=${MEMBERS}"
log "next: go run ./cmd/fleet-controller --hub-context ${HUB_CTX} --fleet-namespace ${FLEET_NS} --kubeconfig-secretreader-plugin ${PLUGIN_BIN}"
log "then: kubectl --context ${HUB_CTX} apply -f examples/fleetgenaiservice-sample.yaml"
