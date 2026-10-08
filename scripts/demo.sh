#!/usr/bin/env bash
# =============================================================================
# demo.sh — the KubeCon EU 2027 session demo, step by step: one kro API goes
# from a single cluster to a fleet spanning EKS, GKE and AKS.
#
# Each step prints a one-line narration, shows the command, runs it and waits
# for Enter. Every step's output is recorded under logs/demo-<run>/ (for the
# backup video). Resume with --step N; rehearse hands-free with --no-pause.
#
# Usage: scripts/demo.sh [--simulated|--real-gpu] [--dry-run] [--step N] [--no-pause] [--yes]
#   (run scripts/provision.sh with the same mode first)
#
# Steps
#    1  register the three clusters as ClusterProfiles (cloud access providers)
#    2  advertise + show cluster properties: no single cluster has 8 free GPUs
#    3  apply the RGDs + each cloud's platform config (the only thing that differs)
#    4  apply the developer's instance: 8 replicas, no cloud named
#    5  show the PlacementDecision and the per-cloud split
#    6  one rolled-up readiness across clouds
#    7  one URL: requests answered from all three clouds
#    8  compliance: placed only on the accredited cloud, shortfall reported, no spill
#    9  drain: a cloud drains, its replicas move, readiness dips then recovers
#   10  outage: a cloud goes unhealthy, the rest absorb what they can, it comes back
#   11  delete the instance: every cloud cleaned up with one delete
#
# The developer's instance (examples/fleetgenaiservice-divided.yaml) is byte-
# identical in --simulated and --real-gpu; only each cloud's platform config
# (config/clouds/<cloud>/platform.yaml) says what backs "gpu" there.
# =============================================================================
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"
demo::parse_flags "$@"
demo::init_logs
demo::preflight
INSTANCE=demo-llm
GOV=gov-llm
GW_PORT="${GW_PORT:-8080}"

# --- always: the hub-side processes (not a numbered step) -------------------------
cyan "━━━ hub: fleet controller + demo inventory agent/scheduler"
demo::start_background
pause

# --- helpers ------------------------------------------------------------------------
split_table() {  # the decision's per-cloud split, from the parameters annotations
  run_sh "kubectl --context '$HUB_CTX' get placementdecision '$1' -n '$WORKLOAD_NS' -o json | python3 -c '
import json,sys
pd=json.load(sys.stdin); ann=pd[\"metadata\"].get(\"annotations\",{})
print(\"  %-22s %-9s %s\" % (\"cluster\",\"replicas\",\"reason\"))
for d in pd.get(\"decisions\",[]):
    n=d[\"clusterProfileRef\"][\"name\"]
    p=json.loads(ann.get(\"parameters.fleet.kro.run/\"+n,\"{}\"))
    print(\"  %-22s %-9s %s\" % (n, p.get(\"replicas\",\"-\"), d.get(\"reason\",\"\")))
print(\"  decision reason:\", ann.get(\"fleet.kro.run/decision-reason\",\"\"))'"
}
fleet_table() {  # the hub's rolled-up view of an instance
  run_sh "kubectl --context '$HUB_CTX' get fgs '$1' -n '$WORKLOAD_NS' -o json | python3 -c '
import json,sys
f=json.load(sys.stdin); st=f.get(\"status\",{}); s=st.get(\"summary\",{})
print(\"  %-22s %-8s %-9s %-7s %s\" % (\"cluster\",\"assigned\",\"ready\",\"ready?\",\"endpoint / message\"))
for c in st.get(\"clusters\",[]):
    print(\"  %-22s %-8s %-9s %-7s %s\" % (c[\"name\"], c.get(\"assignedReplicas\",0), c.get(\"readyReplicas\",0), c.get(\"ready\"), c.get(\"endpoint\") or c.get(\"message\",\"\")))
print(\"  summary: %s/%s replicas ready on %s/%s clusters\" % (s.get(\"readyReplicas\",0), s.get(\"requestedReplicas\",0), s.get(\"ready\",0), s.get(\"placed\",0)))
for k in st.get(\"conditions\",[]):
    print(\"  %-7s %-5s %-30s %s\" % (k[\"type\"], k[\"status\"], k[\"reason\"], k.get(\"message\",\"\")))'"
}
properties_table() {  # gpus-free per cluster, from the hub
  run_sh "kubectl --context '$HUB_CTX' get clusterprofiles -n '$FLEET_NS' -o json | python3 -c '
import json,sys
print(\"  %-22s %-6s %-12s %-5s %-5s %-10s %-13s %s\" % (\"cluster\",\"cloud\",\"region\",\"gpus\",\"free\",\"cost-tier\",\"capacity\",\"compliance\"))
for p in json.load(sys.stdin)[\"items\"]:
    pr={x[\"name\"]:x[\"value\"] for x in p.get(\"status\",{}).get(\"properties\",[])}
    print(\"  %-22s %-6s %-12s %-5s %-5s %-10s %-13s %s\" % (p[\"metadata\"][\"name\"], pr.get(\"cloud.example.com\",\"\"), pr.get(\"region.example.com\",\"\"), pr.get(\"gpus-total.example.com\",\"?\"), pr.get(\"gpus-free.example.com\",\"?\"), pr.get(\"cost-tier.example.com\",\"\"), pr.get(\"capacity-type.example.com\",\"\"), pr.get(\"compliance.example.com\",\"-\")))'"
}
each_cloud_sts() {  # what each cloud runs for an instance
  for c in $CLOUDS; do
    run_sh "kubectl --context '$(demo::ctx_of "$c")' get sts,svc -n '$WORKLOAD_NS' -l app='$1' 2>/dev/null | sed 's/^/  ['"$c"'] /' || true"
  done
}
has_prop() { [ "$(hub get clusterprofile "$1" -n "$FLEET_NS" -o jsonpath="{.status.properties[?(@.name=='$2')].value}" 2>/dev/null)" = "$3" ]; }
placed_is() { [ "$(hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Placed")].reason}' 2>/dev/null)" = "$2" ]; }
ready_is()  { [ "$(hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "$2" ]; }
ready_replicas_is() { [ "$(hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.readyReplicas}' 2>/dev/null)" = "$2" ]; }
sts_gone() { ! kubectl --context "$1" get sts "$2" -n "$WORKLOAD_NS" >/dev/null 2>&1; }

# =====================================================================================
if step "register the fleet" "Three clusters, three clouds, one inventory: each registers as a ClusterProfile whose credentials come from that cloud's own identity plugin."; then
  for c in $CLOUDS; do
    name="$(demo::name_of "$c")"; ctx="$(demo::ctx_of "$c")"
    extra='[]'; [ "$c" = eks ] && extra="[\"--cluster-name\",\"${EKS_CLUSTER}\",\"--region\",\"${EKS_REGION}\"]"
    say "${c}: ClusterProfile ${name} -> accessProvider '${c}' (exec plugin on this machine), ControlPlaneHealthy asserted"
    if [ "$DRY_RUN" = "true" ]; then
      dim "    (dry-run) would create ClusterProfile ${name} with status.accessProviders[].name=${c}"
    else
      demo::register_cloud_member "$c" "$name" "$ctx" "$extra"
    fi
  done
  run kubectl --context "$HUB_CTX" get clusterprofiles -n "$FLEET_NS" -o wide
  say "The hub holds no cloud credential: each profile only says which plugin to call (aws / gke-gcloud-auth-plugin / kubelogin)."
  run_sh "kubectl --context '$HUB_CTX' get clusterprofile '$(demo::name_of eks)' -n '$FLEET_NS' -o jsonpath='{.status.accessProviders}' | python3 -m json.tool"
  wait_until 180 "controller engaged all three members" bash -c "grep -c 'Cluster engaged' '${LOG_DIR}/fleet-controller.log' 2>/dev/null | grep -qE '^[3-9]'"
  pause
fi

if step "advertise cluster properties" "Each cluster advertises what it is and has (About API); the hub mirrors it. No single cluster has eight free GPUs."; then
  for c in $CLOUDS; do
    ctx="$(demo::ctx_of "$c")"; name="$(demo::name_of "$c")"
    run kubectl --context "$ctx" apply -f "${REPO_ROOT}/config/clouds/${c}/properties.yaml"
    run_sh "kubectl --context '$ctx' apply -f - <<EOF
apiVersion: about.k8s.io/v1beta1
kind: ClusterProperty
metadata: {name: cluster.clusterset.k8s.io}
spec: {value: ${name}}
EOF"
  done
  run kubectl --context "$(demo::ctx_of eks)" get clusterproperties
  for c in $CLOUDS; do wait_until 180 "${c}: free GPUs computed and mirrored to the hub" bash -c "kubectl --context '$HUB_CTX' get clusterprofile '$(demo::name_of "$c")' -n '$FLEET_NS' -o jsonpath=\"{.status.properties[?(@.name=='gpus-free.example.com')].value}\" | grep -qE '^[0-9]+$'"; done
  properties_table
  say "Free GPUs: a few per cloud, never eight in one place. The scheduler reads exactly this table; kro-fleet never does."
  pause
fi

if step "apply the platform layer" "The same RGDs on every cloud; the ONLY per-cloud file is the platform config: storage class, load balancer flavor, identity, what backs 'gpu'."; then
  run_sh "diff --color=always -y -W 140 '${REPO_ROOT}/config/clouds/gke/platform.yaml' '${REPO_ROOT}/config/clouds/eks/platform.yaml' | grep -vE '^#' | head -40 || true"
  for c in $CLOUDS; do
    ctx="$(demo::ctx_of "$c")"
    say "${c}: RGDs (GenAIService, ClusterPlatform) + this cloud's platform config (${MODE})"
    run kubectl --context "$ctx" apply -f "${REPO_ROOT}/config/rgd/"
    run_sh "kubectl --context '$ctx' wait rgd/genaiservice.kro.run rgd/clusterplatform.kro.run --for=jsonpath='{.status.state}'=Active --timeout=180s"
    mkdir -p "$STATE_DIR"; demo::render "${REPO_ROOT}/config/clouds/${c}/platform.yaml" > "${STATE_DIR}/platform-${c}.yaml"
    run kubectl --context "$ctx" apply -f "${STATE_DIR}/platform-${c}.yaml"
    run_sh "kubectl --context '$ctx' -n '$WORKLOAD_NS' wait clusterplatform/platform --for=jsonpath='{.status.state}'=ACTIVE --timeout=180s"
  done
  pause
fi

if step "the developer's instance" "One instance, authored once on the hub: a model server with eight replicas. No cluster, no cloud, no storage class, no load balancer, no identity named."; then
  run_sh "grep -vE '^\\s*#' '${REPO_ROOT}/examples/fleetgenaiservice-divided.yaml'"
  run kubectl --context "$HUB_CTX" apply -f "${REPO_ROOT}/examples/fleetgenaiservice-divided.yaml"
  pause
fi

if step "the placement decision" "A scheduler (demo-only, swappable) divides the eight replicas by the GPUs each cloud advertises and writes a standard PlacementDecision; kro-fleet consumes it and places exactly that."; then
  wait_until 120 "PlacementDecision ${INSTANCE} written" hub get placementdecision "$INSTANCE" -n "$WORKLOAD_NS"
  run kubectl --context "$HUB_CTX" get placementdecision "$INSTANCE" -n "$WORKLOAD_NS" -o yaml
  split_table "$INSTANCE"
  say "The split lives on the decision (parameters per cluster); the developer's instance still just says replicas: 8."
  pause
fi

if step "one readiness across clouds" "Each cloud expands its slice with stock kro; the hub rolls replicas up into one Ready condition with per-cluster detail."; then
  wait_until 120 "Placed=True" placed_is "$INSTANCE" Placed
  each_cloud_sts "$INSTANCE"
  wait_until 600 "8/8 replicas ready across the fleet" ready_replicas_is "$INSTANCE" 8
  run kubectl --context "$HUB_CTX" get fgs "$INSTANCE" -n "$WORKLOAD_NS"
  fleet_table "$INSTANCE"
  say "Storage classes resolved per cloud (premium-rwo / managed-csi / gp3), one load balancer per cloud, one Ready on the hub."
  for c in $CLOUDS; do run_sh "kubectl --context '$(demo::ctx_of "$c")' get pvc -n '$WORKLOAD_NS' -l app='$INSTANCE' -o custom-columns='CLOUD:.metadata.namespace,PVC:.metadata.name,CLASS:.spec.storageClassName' --no-headers | sed 's/^fleet-demo/  ['"$c"']/'"; done
  pause
fi

if step "one URL" "A single entry point on the hub spreads requests across the three clouds' own load balancers, weighted by ready replicas."; then
  wait_until 300 "every cloud reports its load-balancer endpoint" bash -c "[ \"\$(kubectl --context '$HUB_CTX' get fgs '$INSTANCE' -n '$WORKLOAD_NS' -o jsonpath='{.status.clusters[*].endpoint}' | wc -w)\" -ge 3 ]"
  demo::start_gateway "$INSTANCE"
  wait_until 60 "gateway up on :${GW_PORT}" curl -fsS "http://localhost:${GW_PORT}/healthz"
  run_sh "curl -s http://localhost:${GW_PORT}/fleet | python3 -m json.tool"
  say "Nine requests to one URL; watch which cloud answers each."
  run_sh "for i in \$(seq 1 9); do curl -s -i http://localhost:${GW_PORT}/v1/chat/completions -H 'Content-Type: application/json' -d '{\"model\":\"Qwen/Qwen2.5-0.5B-Instruct\",\"messages\":[{\"role\":\"user\",\"content\":\"hello from KubeCon\"}]}' | grep -iE '^X-Fleet-Cluster:|\"content\"' | tr -d '\\r' | paste -sd ' ' -; done"
  pause
fi

if step "compliance: fail closed, never spill" "A second instance may only run on a FedRAMP High cluster. Only one cloud is accredited and it has four GPUs free; the request asks for six."; then
  run_sh "kubectl --context '$HUB_CTX' apply -f - <<EOF
apiVersion: fleet.kro.run/v1alpha1
kind: FleetGenAIService
metadata:
  name: ${GOV}
  namespace: ${WORKLOAD_NS}
spec:
  placement:
    decisionRef: {name: ${GOV}}
    requirements:
      matchProperties:
        compliance.example.com: fedramp-high
  template:
    spec:
      name: ${GOV}
      model: \"Qwen/Qwen2.5-0.5B-Instruct\"
      mode: gpu
      replicas: 6
      cacheSize: 1Gi
      monitoring: false
EOF"
  wait_until 120 "decision written" hub get placementdecision "$GOV" -n "$WORKLOAD_NS"
  split_table "$GOV"
  wait_until 120 "Placed=False/InsufficientCapacity" placed_is "$GOV" InsufficientCapacity
  fleet_table "$GOV"
  each_cloud_sts "$GOV"
  say "Four of six run on the accredited cloud. The other two are reported missing — never placed on a cloud that is not accredited."
  say "Now require a regime no cluster has."
  run kubectl --context "$HUB_CTX" patch fgs "$GOV" -n "$WORKLOAD_NS" --type=merge -p '{"spec":{"placement":{"requirements":{"matchProperties":{"compliance.example.com":"il5"}}}}}'
  wait_until 120 "Placed=False/NoEligibleClusters (terminal, explicit)" placed_is "$GOV" NoEligibleClusters
  fleet_table "$GOV"
  say "Terminal and explicit: a reason, not 'not ready yet', and no fallback."
  run kubectl --context "$HUB_CTX" delete fgs "$GOV" -n "$WORKLOAD_NS" --timeout=180s
  pause
fi

if step "drain a cloud" "The platform drains one cloud (a property it advertises). The scheduler moves its replicas elsewhere; kro-fleet unplaces exactly what it tracked there."; then
  drain_cloud="${DRAIN_CLOUD:-gke}"; drain_ctx="$(demo::ctx_of "$drain_cloud")"; drain_name="$(demo::name_of "$drain_cloud")"
  run_sh "kubectl --context '$drain_ctx' apply -f - <<EOF
apiVersion: about.k8s.io/v1beta1
kind: ClusterProperty
metadata: {name: draining.example.com}
spec: {value: \"true\"}
EOF"
  wait_until 120 "hub sees ${drain_cloud} draining" has_prop "$drain_name" draining.example.com true
  wait_until 120 "decision no longer names ${drain_cloud}" bash -c "! kubectl --context '$HUB_CTX' get placementdecision '$INSTANCE' -n '$WORKLOAD_NS' -o jsonpath='{.decisions[*].clusterProfileRef.name}' | grep -q '$drain_name'"
  split_table "$INSTANCE"
  say "Readiness dips while the replicas move, then recovers."
  run_sh "for i in 1 2 3 4 5 6; do kubectl --context '$HUB_CTX' get fgs '$INSTANCE' -n '$WORKLOAD_NS' -o jsonpath='{.status.summary.readyReplicas}/{.status.summary.requestedReplicas}{\"\\n\"}'; sleep 5; done"
  wait_until 600 "8/8 ready again without ${drain_cloud}" ready_replicas_is "$INSTANCE" 8
  wait_until 300 "slice removed from ${drain_cloud}" sts_gone "$drain_ctx" "$INSTANCE"
  run kubectl --context "$HUB_CTX" get appliedmanifestrecords -n "$WORKLOAD_NS"
  say "The drained cloud's inventory record is gone: nothing of this instance remains there."
  run kubectl --context "$drain_ctx" delete clusterproperty draining.example.com
  pause
fi

if step "a cloud goes unhealthy" "A cluster manager reports a control plane unhealthy. kro-fleet stops touching it; the scheduler re-places what the others can hold; it comes back and is cleaned."; then
  sick="${SICK_CLOUD:-aks}"; sick_name="$(demo::name_of "$sick")"
  run kubectl --context "$HUB_CTX" patch clusterprofile "$sick_name" -n "$FLEET_NS" --subresource=status --type=merge -p "{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"False\",\"reason\":\"SimulatedOutage\",\"message\":\"demo\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}"
  wait_until 120 "decision no longer names ${sick}" bash -c "! kubectl --context '$HUB_CTX' get placementdecision '$INSTANCE' -n '$WORKLOAD_NS' -o jsonpath='{.decisions[*].clusterProfileRef.name}' | grep -q '$sick_name'"
  split_table "$INSTANCE"
  fleet_table "$INSTANCE"
  say "Its slice is 'pending removal' until it is reachable again — surfaced, never silently dropped."
  run kubectl --context "$HUB_CTX" patch clusterprofile "$sick_name" -n "$FLEET_NS" --subresource=status --type=merge -p "{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"True\",\"reason\":\"Recovered\",\"message\":\"demo\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}"
  wait_until 600 "8/8 ready after recovery" ready_replicas_is "$INSTANCE" 8
  fleet_table "$INSTANCE"
  pause
fi

if step "one delete" "Delete the hub instance: every cloud's slice, volumes and load balancer go, tracked by the per-member inventory."; then
  run kubectl --context "$HUB_CTX" delete fgs "$INSTANCE" -n "$WORKLOAD_NS" --timeout=300s
  for c in $CLOUDS; do
    run_sh "kubectl --context '$(demo::ctx_of "$c")' get sts,svc,pvc -n '$WORKLOAD_NS' -l app='$INSTANCE' 2>&1 | sed 's/^/  ['"$c"'] /'"
  done
  run kubectl --context "$HUB_CTX" get appliedmanifestrecords,placementdecisions -n "$WORKLOAD_NS"
  say "Nothing left on any cloud, nothing left on the hub. One object, three clouds, one delete."
fi

echo
cyan "━━━ demo complete. logs: ${LOG_DIR}   (reset: scripts/reset.sh; teardown: scripts/teardown.sh)"
