#!/usr/bin/env bash
# =============================================================================
# setup-fleet.sh — stand up the kro-fleet PoC: 1 hub + N member kind clusters.
#
#   hub:     ClusterProfile CRD (cluster-inventory-api), FleetGenAIService CRD,
#            one ClusterProfile + kubeconfig Secret per member.
#   members: stock kro (pinned Helm chart) + the sister repo's RGDs
#            (GenAIService, ClusterPlatform) + one ClusterPlatform instance per
#            simulated cloud. Members are UNMODIFIED kro — the only fleet-aware
#            code anywhere is the hub controller (cmd/fleet-controller).
#
# Usage:  scripts/setup-fleet.sh [num-members]        (default: 2)
# Then:   go run ./cmd/fleet-controller --hub-context kind-${PREFIX}-hub
#         kubectl --context kind-${PREFIX}-hub apply -f examples/fleetgenaiservice-sample.yaml
#
# Everything is pinned for reproducibility. All knobs may be overridden via env.
# =============================================================================
set -euo pipefail

MEMBERS="${1:-2}"
PREFIX="${PREFIX:-kro-fleet}"
FLEET_NS="${FLEET_NS:-fleet-system}"          # hub: ClusterProfiles + Secrets
WORKLOAD_NS="${WORKLOAD_NS:-fleet-demo}"      # everywhere: placed workloads
CONSUMER="${CONSUMER:-kro-fleet}"             # cluster-inventory consumer name
TIER="${TIER:-prod}"                          # placement label on every member

# --- pinned versions ---------------------------------------------------------
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5}"
KRO_CHART="${KRO_CHART:-oci://registry.k8s.io/kro/charts/kro}"
KRO_VERSION="${KRO_VERSION:-0.9.2}"   # OCI tag (chart reports itself as v0.9.2)
INVENTORY_VERSION="${INVENTORY_VERSION:-v0.1.3}"
INVENTORY_CRD_URL="https://raw.githubusercontent.com/kubernetes-sigs/cluster-inventory-api/${INVENTORY_VERSION}/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml"
# Sister project (reused verbatim, pinned to a commit):
SISTER_REF="${SISTER_REF:-9a2fa5c19e04e0c96bf0d37ed1c69761c1d164e0}"
SISTER_RAW="https://raw.githubusercontent.com/danbruno101/kro-genaiops-demo/${SISTER_REF}"

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

# Simulated clouds cycled across members: name + the StorageClass each cloud
# would ship. On kind the class is minted with the local-path provisioner
# (manageStorageClass: true), mirroring the sister repo's cloud simulations.
CLOUDS=(gke aks eks)
CLASSES=(premium-rwo managed-csi gp3)

log() { echo ">>> $*"; }

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
log "installing ClusterProfile CRD (cluster-inventory-api ${INVENTORY_VERSION}) + FleetGenAIService CRD on hub"
kubectl --context "$HUB_CTX" apply -f "$INVENTORY_CRD_URL"
kubectl --context "$HUB_CTX" apply -f "${REPO_ROOT}/config/crd/fleet.kro.run_fleetgenaiservices.yaml"
kubectl --context "$HUB_CTX" create namespace "$FLEET_NS" --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -
kubectl --context "$HUB_CTX" create namespace "$WORKLOAD_NS" --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -

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

  log "[$member] registering ClusterProfile on hub (tier=${TIER}, cloud=${cloud})"
  cat <<EOF | kubectl --context "$HUB_CTX" apply -f -
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${member}
  namespace: ${FLEET_NS}
  labels:
    tier: ${TIER}
    fleet.kro.run/cloud: ${cloud}
spec:
  displayName: ${member}
  clusterManager:
    name: ${CONSUMER}
EOF
  # No cluster-manager agent exists on a bare kind fleet, so health is asserted
  # at registration time (ledgered in docs/KEP-GAP.md). The provider's readiness
  # gate consumes this condition unchanged.
  kubectl --context "$HUB_CTX" patch clusterprofile "$member" -n "$FLEET_NS" \
    --subresource=status --type=merge -p "{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"True\",\"reason\":\"AssertedAtRegistration\",\"message\":\"kind member registered by setup-fleet.sh\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}"

  # Kubeconfig Secret for the provider's Secret strategy. `kind get kubeconfig`
  # returns the host-reachable endpoint: the fleet controller runs as a host
  # process in this PoC (see docs/KEP-GAP.md). No credentials ever touch git.
  kind get kubeconfig --name "$member" | kubectl --context "$HUB_CTX" \
    create secret generic "${member}-kubeconfig" -n "$FLEET_NS" \
    --from-file=Config=/dev/stdin --dry-run=client -o yaml | kubectl --context "$HUB_CTX" apply -f -
  kubectl --context "$HUB_CTX" label secret "${member}-kubeconfig" -n "$FLEET_NS" --overwrite \
    "x-k8s.io/cluster-inventory-consumer=${CONSUMER}" "x-k8s.io/cluster-profile=${member}"
done

log "fleet ready: hub=${HUB} members=${MEMBERS}"
log "next: go run ./cmd/fleet-controller --hub-context ${HUB_CTX} --fleet-namespace ${FLEET_NS}"
log "then: kubectl --context ${HUB_CTX} apply -f examples/fleetgenaiservice-sample.yaml"
