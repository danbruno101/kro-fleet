#!/usr/bin/env bash
# =============================================================================
# scripts/lib/fleet.sh — shell helpers shared by the fleet scripts.
#
# Source it: . "$(dirname "${BASH_SOURCE[0]}")/lib/fleet.sh"
#
# Everything here talks to the hub through `kubectl --context "$HUB_CTX"` and
# never writes credentials anywhere but the hub's fleet namespace.
# =============================================================================

# Pinned upstream artifacts (single source of truth for the scripts).
INVENTORY_VERSION="${INVENTORY_VERSION:-v0.1.3}"          # sigs.k8s.io/cluster-inventory-api
INVENTORY_RAW="https://raw.githubusercontent.com/kubernetes-sigs/cluster-inventory-api/${INVENTORY_VERSION}"
INVENTORY_CRD_CLUSTERPROFILE="${INVENTORY_RAW}/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml"
INVENTORY_CRD_PLACEMENTDECISION="${INVENTORY_RAW}/config/crd/bases/multicluster.x-k8s.io_placementdecisions.yaml"
# sigs.k8s.io/about-api has no tags; pinned to a commit (ClusterProperty,
# about.k8s.io v1alpha1+v1beta1). The CRD is vendored, rendered through
# upstream's kustomize overlay (config/about-api/ — see the header there).
ABOUT_API_REF="${ABOUT_API_REF:-93309ed4a6031f62674fb22069e6bec7e6452c59}"
ABOUT_API_CRD="${REPO_ROOT}/config/about-api/about.k8s.io_clusterproperties.yaml"

# ClusterProfile access provider name advertised by kind members; must match
# the fleet controller's built-in default (cmd/fleet-controller).
KIND_ACCESS_PROVIDER="kubeconfig-secretreader"

fleet::log() { echo ">>> $*"; }

# fleet::hub <kubectl args...> — kubectl against the hub context.
fleet::hub() { kubectl --context "$HUB_CTX" "$@"; }

# fleet::install_hub_crds — ClusterProfile + PlacementDecision (cluster-inventory-api)
# and the FleetGenAIService CRDs on the hub.
fleet::install_hub_crds() {
  fleet::log "installing ClusterProfile + PlacementDecision CRDs (cluster-inventory-api ${INVENTORY_VERSION}) and fleet.kro.run CRDs on hub"
  fleet::hub apply -f "$INVENTORY_CRD_CLUSTERPROFILE"
  fleet::hub apply -f "$INVENTORY_CRD_PLACEMENTDECISION"
  fleet::hub apply -f "${REPO_ROOT}/config/crd/"
}

# fleet::install_member_crds <context> — the About API ClusterProperty CRD on a member.
fleet::install_member_crds() {
  local ctx=$1
  kubectl --context "$ctx" apply -f "$ABOUT_API_CRD"
}

# fleet::register_member <profile-name> <cloud> <kubeconfig-file> [tier]
#
# Registers one member on the hub the KEP-4322 way:
#   * a ClusterProfile (labels tier=<tier>, fleet.kro.run/cloud=<cloud>);
#   * status.accessProviders[] naming the kubeconfig-secretreader provider,
#     with the member's API endpoint + CA taken from the kubeconfig and a
#     client.authentication.k8s.io/exec extension pointing the plugin at the
#     Secret below (KEP-5339 exec plugin mechanism);
#   * a Secret holding the member kubeconfig (data key Config), read by the
#     exec plugin at credential time — never by the controller directly;
#   * ControlPlaneHealthy=True, self-asserted: a bare kind fleet has no
#     cluster-manager agent (ledgered in docs/KEP-GAP.md).
#
# `kind get kubeconfig` yields the host-reachable endpoint: the controller
# runs as a host process in this PoC. No credentials ever touch git.
fleet::register_member() {
  local name=$1 cloud=$2 kubeconfig=$3 tier=${4:-${TIER:-prod}}
  local secret="${name}-kubeconfig"
  local server ca
  server="$(kubectl --kubeconfig "$kubeconfig" config view --raw -o jsonpath='{.clusters[0].cluster.server}')"
  ca="$(kubectl --kubeconfig "$kubeconfig" config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  [ -n "$server" ] && [ -n "$ca" ] || { echo "!!! $kubeconfig: no server/CA found" >&2; return 1; }

  fleet::log "[$name] registering ClusterProfile on hub (tier=${tier}, cloud=${cloud}, accessProvider=${KIND_ACCESS_PROVIDER})"
  fleet::hub create secret generic "$secret" -n "$FLEET_NS" \
    --from-file=Config="$kubeconfig" --dry-run=client -o yaml | fleet::hub apply -f - >/dev/null

  cat <<EOF | fleet::hub apply -f - >/dev/null
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${name}
  namespace: ${FLEET_NS}
  labels:
    tier: ${tier}
    fleet.kro.run/cloud: ${cloud}
spec:
  displayName: ${name}
  clusterManager:
    name: ${CONSUMER}
EOF

  fleet::hub patch clusterprofile "$name" -n "$FLEET_NS" --subresource=status --type=merge -p "$(cat <<EOF
{"status":{
  "conditions":[{"type":"ControlPlaneHealthy","status":"True","reason":"AssertedAtRegistration",
                 "message":"kind member registered by scripts/lib/fleet.sh","lastTransitionTime":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}],
  "accessProviders":[{"name":"${KIND_ACCESS_PROVIDER}","cluster":{
     "server":"${server}","certificate-authority-data":"${ca}",
     "extensions":[{"name":"client.authentication.k8s.io/exec",
                    "extension":{"name":"${secret}","key":"Config","namespace":"${FLEET_NS}"}}]}}]
}}
EOF
)" >/dev/null
}

# fleet::deregister_member <profile-name> — remove the ClusterProfile (and its Secret).
fleet::deregister_member() {
  local name=$1
  fleet::hub delete clusterprofile "$name" -n "$FLEET_NS" --ignore-not-found >/dev/null
  fleet::hub delete secret "${name}-kubeconfig" -n "$FLEET_NS" --ignore-not-found >/dev/null
}

# fleet::build_plugin <output-path> — build the upstream kubeconfig-secretreader
# exec plugin (sigs.k8s.io/cluster-inventory-api) the controller's built-in
# access provider shells out to.
fleet::build_plugin() {
  local out=$1
  ( cd "$REPO_ROOT" && go build -o "$out" sigs.k8s.io/cluster-inventory-api/plugins/kubeconfig-secretreader/cmd/plugin )
}
