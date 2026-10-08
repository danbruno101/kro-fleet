#!/usr/bin/env bash
# =============================================================================
# teardown.sh — delete every cloud resource the demo created (the three
# clusters and their load balancers, AKS resource group) and the kind hub.
# Asks for confirmation first.
#
# Usage: scripts/teardown.sh [--dry-run] [--yes]
# =============================================================================
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"
demo::parse_flags "$@"
demo::init_logs

bold "this deletes:"
echo "    eks  cluster ${EKS_CLUSTER} (${EKS_REGION})"
echo "    gke  cluster ${GKE_CLUSTER} (${GKE_ZONE})"
echo "    aks  cluster ${AKS_CLUSTER} + resource group ${AKS_RG} (${AKS_LOCATION})"
echo "    hub  kind cluster ${HUB} on this machine"
confirm "Delete all of the above?" || die "aborted; nothing deleted"

# Instances first, so each cloud's load balancers and volumes are released
# by their own controllers before the clusters go.
if kubectl --context "$HUB_CTX" get ns "$WORKLOAD_NS" >/dev/null 2>&1; then
  run_sh "kubectl --context '$HUB_CTX' delete fgs --all -n '$WORKLOAD_NS' --timeout=300s || true"
fi
demo::stop_background

for c in $CLOUDS; do
  case "$c" in
    eks) cyan "━━━ eks"; run eksctl delete cluster --name "$EKS_CLUSTER" --region "$EKS_REGION" --wait ;;
    gke) cyan "━━━ gke"; run gcloud container clusters delete "$GKE_CLUSTER" --zone "$GKE_ZONE" --quiet ;;
    aks) cyan "━━━ aks"; run az group delete --name "$AKS_RG" --yes --no-wait ;;
  esac
  ctx="$(demo::ctx_of "$c")"
  run_sh "kubectl config delete-context '$ctx' 2>/dev/null || true"
done
cyan "━━━ hub"
run kind delete cluster --name "$HUB"
run_sh "rm -rf '${STATE_DIR}'"
cyan "━━━ done. Check each cloud console once for stray load balancers or disks before you stop watching the bill."
