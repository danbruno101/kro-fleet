#!/usr/bin/env bash
# =============================================================================
# reset.sh — return the fleet to the demo's start state between rehearsals,
# without re-provisioning: removes the instances (and so every placed object
# on every cloud), decisions and records, un-drains, restores health, stops
# the hub-side processes. Clusters, kro, RGDs and properties stay.
#
# Usage: scripts/reset.sh [--dry-run] [--yes] [--deregister]
#   --deregister   also remove the ClusterProfiles so the demo restarts at step 1
#                  (default keeps them, so you can resume with --step 4)
# =============================================================================
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"
DEREGISTER=false
args=()
for a in "$@"; do case "$a" in --deregister) DEREGISTER=true ;; *) args+=("$a") ;; esac; done
demo::parse_flags "${args[@]}"
demo::init_logs

cyan "━━━ reset: instances on the hub (cleans every cloud through the inventory)"
run_sh "kubectl --context '$HUB_CTX' delete fgs --all -n '$WORKLOAD_NS' --timeout=300s"
run_sh "kubectl --context '$HUB_CTX' delete placementdecisions --all -n '$WORKLOAD_NS' --ignore-not-found"
if [ "$DRY_RUN" != "true" ]; then
  leftovers="$(kubectl --context "$HUB_CTX" get appliedmanifestrecords -n "$WORKLOAD_NS" -o name 2>/dev/null || true)"
  [ -z "$leftovers" ] || warn "orphan records remain (a member was unreachable at cleanup): $leftovers — they settle when the member returns"
fi

cyan "━━━ reset: un-drain, restore health"
for c in $CLOUDS; do
  ctx="$(demo::ctx_of "$c")"; name="$(demo::name_of "$c")"
  has_context "$ctx" || { dim "    ${c}: no context ${ctx}, skipping"; continue; }
  run_sh "kubectl --context '$ctx' delete clusterproperty draining.example.com --ignore-not-found"
  if kubectl --context "$HUB_CTX" get clusterprofile "$name" -n "$FLEET_NS" >/dev/null 2>&1 || [ "$DRY_RUN" = "true" ]; then
    run_sh "kubectl --context '$HUB_CTX' patch clusterprofile '$name' -n '$FLEET_NS' --subresource=status --type=merge -p '{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"True\",\"reason\":\"Reset\",\"message\":\"scripts/reset.sh\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}'"
  fi
  # Leftover slices on a cloud that was unreachable at cleanup time.
  run_sh "kubectl --context '$ctx' delete genaiservice --all -n '$WORKLOAD_NS' --ignore-not-found --timeout=180s"
done
if [ "$DEREGISTER" = "true" ]; then
  cyan "━━━ reset: deregistering the ClusterProfiles (demo restarts at step 1)"
  run_sh "kubectl --context '$HUB_CTX' delete clusterprofiles --all -n '$FLEET_NS' --ignore-not-found"
fi

cyan "━━━ reset: hub-side processes"
demo::stop_background
echo "    stopped (logs kept under ${REPO_ROOT}/logs/)"
cyan "━━━ ready for the next take: scripts/demo.sh --${MODE}$([ "$DEREGISTER" = "true" ] || echo ' --step 4')"
