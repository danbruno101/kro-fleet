#!/usr/bin/env bash
# =============================================================================
# setup-fleet.sh — stand up the kro-fleet PoC: 1 hub + N member kind clusters.
#
#   hub:     ClusterProfile + PlacementDecision CRDs (cluster-inventory-api),
#            the fleet.kro.run CRDs, one ClusterProfile per member whose
#            status.accessProviders points an exec credential plugin at that
#            member's kubeconfig Secret (KEP-4322 / KEP-5339).
#   members: stock kro (pinned Helm chart) + the sister repo's RGDs
#            (GenAIService, ClusterPlatform) + one ClusterPlatform instance per
#            simulated cloud + the About API ClusterProperty CRD. Members are
#            UNMODIFIED kro — the only fleet-aware code anywhere is the hub
#            controller (cmd/fleet-controller).
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
# Sister project (reused verbatim, pinned to a commit). kro >= 0.9.3 rejects the
# older RGD's `status.conditions` string, so this pin must move with KRO_VERSION.
SISTER_REF="${SISTER_REF:-cf7ae45cae92dec530969ba4af29c20fe72e9907}"
SISTER_RAW="https://raw.githubusercontent.com/danbruno101/kro-genaiops-demo/${SISTER_REF}"
# cluster-inventory-api / about-api pins live in scripts/lib/fleet.sh.

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
  cloud="${CLOUDS[$(( (i-1) % ${#CLOUDS[@]} ))]}"
  class="${CLASSES[$(( (i-1) % ${#CLASSES[@]} ))]}"

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

  log "[$member] applying sister-repo RGDs (pinned ${SISTER_REF:0:12})"
  kubectl --context "$ctx" apply -f "${SISTER_RAW}/rgd/genaiops-rgd.yaml"
  kubectl --context "$ctx" apply -f "${SISTER_RAW}/rgd/platform-rgd.yaml"
  kubectl --context "$ctx" wait rgd/genaiservice.kro.run rgd/clusterplatform.kro.run \
    --for=jsonpath='{.status.state}'=Active --timeout=120s

  log "[$member] applying ClusterPlatform instance (cloud=${cloud}, storageClass=${class})"
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
  storageClass: ${class}
  manageStorageClass: true          # kind sims mint the cloud's named class
  provisioner: rancher.io/local-path
  makeDefault: false
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
