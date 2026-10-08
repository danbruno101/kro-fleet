#!/usr/bin/env bash
# =============================================================================
# provision.sh — create (or reuse) the KubeCon demo fleet:
#   one member cluster per cloud (EKS, GKE, AKS) + the hub, then install
#   stock kro on each member. Prints a cost estimate and asks before creating
#   anything billable.
#
# Where the hub runs — a kind cluster on this machine — and why:
#   * no cross-cloud identity federation on the critical path: the hub reaches
#     each cloud through that cloud's own exec plugin (aws / gke-gcloud-auth-
#     plugin / kubelogin) using the logins already on this laptop
#     (ClusterProfile.status.accessProviders, KEP-4322 / KEP-5339);
#   * it is the one component the presenter fully controls on stage;
#   * it makes the neutrality point: the hub is not one of the clouds;
#   * reset between rehearsals is instant and costs nothing.
#   A cloud-hosted hub (e.g. GKE with workload-identity federation to the
#   other two) is documented as a stretch in docs/demo-cloud.md, not built.
#
# Usage: scripts/provision.sh [--simulated|--real-gpu] [--dry-run] [--yes]
#   --simulated  (default) CPU nodes; each cluster advertises FAKE_GPUS units of
#                example.com/gpu and the model server is the mock stand-in.
#   --real-gpu   adds one GPU node pool per cluster (quota required) and the
#                platform backs "gpu" with nvidia.com/gpu + vLLM.
#   --dry-run    print every command, create nothing (also skips login checks).
#   --yes        skip the confirmation prompt (CI, rehearsals).
#
# Idempotent: existing clusters are reused. Logs: logs/provision-<run>/.
# Env knobs: see scripts/lib/demo.sh (names, regions, sizes).
# =============================================================================
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"
demo::parse_flags "$@"
demo::init_logs
demo::preflight
demo::cost_table
echo
confirm "Create/reuse the hub + ${CLOUDS} clusters (${MODE})?" || die "aborted; nothing created"

# --- hub (kind, local) -----------------------------------------------------------
cyan "━━━ hub: kind cluster ${HUB} on this machine"
if kind get clusters 2>/dev/null | grep -qx "$HUB"; then
  dim "    kind cluster ${HUB} exists, reusing"
else
  run kind create cluster --name "$HUB" --image "$KIND_NODE_IMAGE" --config "${REPO_ROOT}/config/kind/cluster.yaml" --wait 240s
fi
run_sh "kubectl --context '$HUB_CTX' apply -f '$INVENTORY_CRD_CLUSTERPROFILE'"
run_sh "kubectl --context '$HUB_CTX' apply -f '$INVENTORY_CRD_PLACEMENTDECISION'"
run_sh "kubectl --context '$HUB_CTX' apply -f '${REPO_ROOT}/config/crd/'"
run_sh "kubectl --context '$HUB_CTX' create namespace '$FLEET_NS' --dry-run=client -o yaml | kubectl --context '$HUB_CTX' apply -f -"
run_sh "kubectl --context '$HUB_CTX' create namespace '$WORKLOAD_NS' --dry-run=client -o yaml | kubectl --context '$HUB_CTX' apply -f -"

# --- members ---------------------------------------------------------------------
provision_eks() {
  cyan "━━━ eks: ${EKS_CLUSTER} (${EKS_REGION}, Auto Mode)"
  if [ "$DRY_RUN" = "true" ] || ! aws eks describe-cluster --name "$EKS_CLUSTER" --region "$EKS_REGION" >/dev/null 2>&1; then
    # Auto Mode: built-in compute + the EBS CSI driver, no addons to manage.
    run eksctl create cluster --name "$EKS_CLUSTER" --region "$EKS_REGION" --enable-auto-mode
  else
    dim "    cluster exists, reusing"
  fi
  run aws eks update-kubeconfig --region "$EKS_REGION" --name "$EKS_CLUSTER" --alias "$EKS_CTX"
  if [ "$MODE" = "real-gpu" ]; then
    # A Karpenter NodePool for GPU instances (Auto Mode's built-in pools have none).
    run_sh "kubectl --context '$EKS_CTX' apply -f - <<'EOF'
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu
spec:
  template:
    spec:
      nodeClassRef: {group: eks.amazonaws.com, kind: NodeClass, name: default}
      requirements:
        - {key: eks.amazonaws.com/instance-gpu-manufacturer, operator: In, values: [nvidia]}
        - {key: eks.amazonaws.com/instance-category, operator: In, values: [g]}
        - {key: eks.amazonaws.com/instance-gpu-count, operator: In, values: [\"1\"]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
  limits:
    nvidia.com/gpu: ${GPU_NODES}
EOF"
  fi
}
provision_gke() {
  cyan "━━━ gke: ${GKE_CLUSTER} (${GKE_ZONE})"
  if [ "$DRY_RUN" = "true" ] || ! gcloud container clusters describe "$GKE_CLUSTER" --zone "$GKE_ZONE" >/dev/null 2>&1; then
    run gcloud container clusters create "$GKE_CLUSTER" --zone "$GKE_ZONE" --num-nodes 2 --machine-type e2-standard-4 --release-channel regular --quiet
  else
    dim "    cluster exists, reusing"
  fi
  run gcloud container clusters get-credentials "$GKE_CLUSTER" --zone "$GKE_ZONE"
  run_sh "kubectl config rename-context \"gke_\$(gcloud config get-value project 2>/dev/null)_${GKE_ZONE}_${GKE_CLUSTER}\" '$GKE_CTX' 2>/dev/null || true"
  if [ "$MODE" = "real-gpu" ]; then
    run gcloud container node-pools create gpu --cluster "$GKE_CLUSTER" --zone "$GKE_ZONE" \
      --machine-type g2-standard-4 --accelerator "type=nvidia-l4,count=1,gpu-driver-version=latest" --num-nodes "$GPU_NODES" --quiet
  fi
}
provision_aks() {
  cyan "━━━ aks: ${AKS_CLUSTER} (${AKS_LOCATION}, resource group ${AKS_RG})"
  run az group create --name "$AKS_RG" --location "$AKS_LOCATION" --output none
  if [ "$DRY_RUN" = "true" ] || ! az aks show --resource-group "$AKS_RG" --name "$AKS_CLUSTER" >/dev/null 2>&1; then
    # Entra ID integration + Azure RBAC, so kubelogin mints the hub's tokens.
    run az aks create --resource-group "$AKS_RG" --name "$AKS_CLUSTER" --node-count 2 --node-vm-size Standard_D4s_v3 \
      --enable-aad --enable-azure-rbac --generate-ssh-keys --output none
  else
    dim "    cluster exists, reusing"
  fi
  run az aks get-credentials --resource-group "$AKS_RG" --name "$AKS_CLUSTER" --context "$AKS_CTX" --overwrite-existing
  if [ "$MODE" = "real-gpu" ]; then
    run az aks nodepool add --resource-group "$AKS_RG" --cluster-name "$AKS_CLUSTER" --name gpu \
      --node-vm-size Standard_NC4as_T4_v3 --node-count "$GPU_NODES" --output none
  fi
}
for c in $CLOUDS; do "provision_$c"; done

# --- kro + the About API on every member; simulated accelerators ----------------------
for c in $CLOUDS; do
  ctx="$(demo::ctx_of "$c")"
  cyan "━━━ ${c}: stock kro ${KRO_VERSION} + About API CRD"
  run helm upgrade --install kro "$KRO_CHART" --version "$KRO_VERSION" --kube-context "$ctx" -n kro --create-namespace --wait --timeout 5m
  run kubectl --context "$ctx" apply -f "$ABOUT_API_CRD"
  run_sh "kubectl --context '$ctx' create namespace '$WORKLOAD_NS' --dry-run=client -o yaml | kubectl --context '$ctx' apply -f -"
  if [ "$MODE" = "simulated" ] && [ "$FAKE_GPUS" != "0" ]; then
    say "${c}: advertising ${FAKE_GPUS} x ${FAKE_GPU_RESOURCE} across the nodes (simulated accelerators)"
    if [ "$DRY_RUN" = "true" ]; then
      dim "    (dry-run) would PATCH each node's status.capacity/allocatable"
    else
      nodes=( $(kubectl --context "$ctx" get nodes -o name) )
      per=$(( (FAKE_GPUS + ${#nodes[@]} - 1) / ${#nodes[@]} )); left=$FAKE_GPUS
      key="${FAKE_GPU_RESOURCE//\//~1}"
      for n in "${nodes[@]}"; do
        this=$(( left < per ? left : per )); left=$(( left - this ))
        kubectl --context "$ctx" patch "$n" --subresource=status --type=json \
          -p "[{\"op\":\"add\",\"path\":\"/status/capacity/${key}\",\"value\":\"${this}\"},{\"op\":\"add\",\"path\":\"/status/allocatable/${key}\",\"value\":\"${this}\"}]" >/dev/null
      done
    fi
  fi
done

echo
cyan "━━━ provisioned (${MODE}). Next: scripts/demo.sh --${MODE}"
dim "    teardown: scripts/teardown.sh   reset between rehearsals: scripts/reset.sh"
