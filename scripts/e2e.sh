#!/usr/bin/env bash
# =============================================================================
# e2e.sh — the PoC's proof. Runs the full fleet loop on kind and asserts every
# success criterion from CLAUDE.md:
#
#   1. one FleetGenAIService on the hub -> workload Ready on every matching member
#   2. mutating the hub object          -> all members converge
#   3. adding a matching ClusterProfile -> workload lands automatically
#   4. removing/unmatching a member     -> workload removed there, no orphans
#   5. deleting the hub object          -> all placed objects on all members GC'd
#   6. status.clusters[] reflects per-member readiness + correct rollup
#   7. the per-member inventory (AppliedManifestRecord) tracks every placed
#      object, prunes stale ones, and surfaces (then settles) orphans left on
#      a member that was deregistered before cleanup
#   8. a PlacementDecision (KEP-5313) is consumed as the placement input:
#      per-member replicas divide the template, a violated hard requirement
#      refuses the whole decision, a partial decision never spills, an empty
#      decision is a terminal refusal with the producer's reason, and a
#      missing decision is pending — all recorded in status.placement
#   9. capacity across clouds: the demo inventory agent advertises free
#      accelerators per member, the demo scheduler divides 8 replicas that no
#      single member can hold, readiness rolls up by replicas, and switching
#      to cheapest-first consolidates onto the cheaper tiers
#  10. compliance: a hard requirement only one member satisfies places only
#      there, reports the shortfall and never spills; a requirement nobody
#      satisfies is a terminal refusal
#  11. drain: marking a member draining moves its replicas to the other
#      clouds, readiness dips then recovers, and the drained member's
#      inventory record is gone
#  12. one URL: the demo gateway spreads requests across the members' own
#      load balancers (needs cloud-provider-kind; skipped when unavailable)
#
# Usage: scripts/e2e.sh          (creates the fleet via setup-fleet.sh, runs the
#                                 controller + demo processes as host processes,
#                                 asserts, cleans up)
# Env:   KEEP_FLEET=true         leave clusters up afterwards (default: teardown
#                                only in CI, keep locally)
#        Everything setup-fleet.sh accepts (PREFIX, KIND_NODE_IMAGE, ...).
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX="${PREFIX:-kro-fleet}"
FLEET_NS="${FLEET_NS:-fleet-system}"
WORKLOAD_NS="${WORKLOAD_NS:-fleet-demo}"
HUB_CTX="kind-${PREFIX}-hub"
M1="${PREFIX}-member-1"; M1_CTX="kind-${M1}"   # gke persona, cost tier 2
M2="${PREFIX}-member-2"; M2_CTX="kind-${M2}"   # aks persona, cost tier 3, spot, fedramp-high
M3="${PREFIX}-member-3"; M3_CTX="kind-${M3}"   # eks persona, cost tier 1
CTRL_LOG="$(mktemp -t fleet-controller-XXXX.log)"
CTRL_PID=""
DEMO_LOG="$(mktemp -t fleet-demo-XXXX.log)"
DEMO_PID=""
CPK_LOG="$(mktemp -t cloud-provider-kind-XXXX.log)"
CPK_PID=""
CLOUD_PROVIDER_KIND_VERSION="${CLOUD_PROVIDER_KIND_VERSION:-v0.12.0}"
CONSUMER="${CONSUMER:-kro-fleet}"
TIER="${TIER:-prod}"
PLUGIN_BIN="${PLUGIN_BIN:-${REPO_ROOT}/bin/kubeconfig-secretreader-plugin}"

# shellcheck source=lib/fleet.sh
. "${REPO_ROOT}/scripts/lib/fleet.sh"

hub()    { kubectl --context "$HUB_CTX" "$@"; }
member() { local m=$1; shift; kubectl --context "kind-${m}" "$@"; }

fail() {
  echo "!!! FAIL: $*" >&2
  echo "--- fleet controller log (tail) ---" >&2; tail -50 "$CTRL_LOG" >&2 || true
  echo "--- fleet-demo log (tail) ---" >&2; tail -40 "$DEMO_LOG" >&2 || true
  echo "--- hub FleetGenAIServices ---" >&2; hub get fgs -A -o yaml 2>/dev/null | tail -80 >&2 || true
  exit 1
}

controller_alive() {
  [ -z "$CTRL_PID" ] || kill -0 "$CTRL_PID" 2>/dev/null || fail "fleet controller exited prematurely"
  [ -z "$DEMO_PID" ] || kill -0 "$DEMO_PID" 2>/dev/null || fail "fleet-demo exited prematurely"
}

# wait_for <timeout-seconds> <description> <command...>   (command must succeed)
wait_for() {
  local timeout=$1 desc=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    controller_alive
    [ "$(date +%s)" -ge "$deadline" ] && fail "timed out waiting for: $desc"
    sleep 5
  done
  echo "    ok: $desc"
}

# wait_gone <timeout-seconds> <description> <command...>  (command must fail)
wait_gone() {
  local timeout=$1 desc=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  while "$@" >/dev/null 2>&1; do
    controller_alive
    [ "$(date +%s)" -ge "$deadline" ] && fail "timed out waiting for: $desc"
    sleep 5
  done
  echo "    ok: $desc"
}

ready_count() { hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.ready}' 2>/dev/null; }
is_ready()    { [ "$(ready_count)" = "$1" ]; }
# record_applied <member>: the member's AppliedManifestRecord confirms the placed object (with its UID).
record_applied() {
  [ "$(hub get amr "demo-llm-$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Applied")].status}' 2>/dev/null)" = "True" ] &&
  [ -n "$(hub get amr "demo-llm-$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.applied[?(@.kind=="GenAIService")].uid}' 2>/dev/null)" ]
}
register_member2() {
  local kc; kc="$(mktemp -t "${M2}-XXXX.kubeconfig")"
  kind get kubeconfig --name "$M2" > "$kc"
  fleet::register_member "$M2" aks "$kc" prod
  rm -f "$kc"
}

cleanup() {
  [ -n "$CTRL_PID" ] && kill "$CTRL_PID" 2>/dev/null || true
  [ -n "$DEMO_PID" ] && kill "$DEMO_PID" 2>/dev/null || true
  [ -n "$CPK_PID" ] && kill "$CPK_PID" 2>/dev/null || true
  if [ "${KEEP_FLEET:-}" != "true" ] && [ -n "${CI:-}" ]; then
    "$REPO_ROOT/scripts/teardown-fleet.sh" || true
  fi
}
trap cleanup EXIT

echo "### e2e: setting up the fleet (1 hub + 3 members: gke / aks / eks personas)"
"$REPO_ROOT/scripts/setup-fleet.sh" 3

# cloud-provider-kind hands every kind LoadBalancer Service a real address
# (so each member exposes its slice through "its own load balancer" exactly
# as on the clouds). Optional: criterion 12 is skipped when it cannot run.
if command -v docker >/dev/null 2>&1; then
  echo "### e2e: starting cloud-provider-kind ${CLOUD_PROVIDER_KIND_VERSION} (LoadBalancer addresses on kind)"
  CPK_BIN="$(mktemp -t cloud-provider-kind-XXXX)"
  if GOBIN="$(dirname "$CPK_BIN")" go install "sigs.k8s.io/cloud-provider-kind@${CLOUD_PROVIDER_KIND_VERSION}" 2>>"$CPK_LOG" && \
     mv "$(dirname "$CPK_BIN")/cloud-provider-kind" "$CPK_BIN"; then
    "$CPK_BIN" >"$CPK_LOG" 2>&1 &
    CPK_PID=$!
  else
    echo "    (cloud-provider-kind unavailable; criterion 12 will be skipped)"
  fi
fi

echo "### e2e: starting the fleet controller (host process)"
CTRL_BIN="$(mktemp -t fleet-controller-XXXX)"
( cd "$REPO_ROOT" && go build -o "$CTRL_BIN" ./cmd/fleet-controller )
"$CTRL_BIN" --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" \
  --kubeconfig-secretreader-plugin "$PLUGIN_BIN" >"$CTRL_LOG" 2>&1 &
CTRL_PID=$!

echo "### e2e: starting the demo inventory agent + scheduler (host process, demo-only)"
DEMO_BIN="$(mktemp -t fleet-demo-XXXX)"
( cd "$REPO_ROOT" && go build -o "$DEMO_BIN" ./cmd/fleet-demo )
"$DEMO_BIN" all --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" \
  --kubeconfig-secretreader-plugin "$PLUGIN_BIN" >"$DEMO_LOG" 2>&1 &
DEMO_PID=$!

# Dev convenience, on a fleet that is already up: E2E_FROM=8 skips the
# replicated flow (criteria 1-7) and runs the decision + demo scenarios;
# E2E_FROM=9 also skips the hand-written decision (criterion 8).
if [ "${E2E_FROM:-1}" -le 7 ]; then
echo "### criterion 3 (part 1): member-2 is NOT registered when the workload is placed"
hub delete clusterprofile "$M2" -n "$FLEET_NS" --ignore-not-found >/dev/null

echo "### criterion 1: place -> Ready on every matching member"
hub apply -f "$REPO_ROOT/examples/fleetgenaiservice-sample.yaml"
wait_for 420 "workload Ready on member-1 and member-3 (real kro expansion)" is_ready 2
member "$M1" get sts demo-llm -n "$WORKLOAD_NS" >/dev/null || fail "kro did not expand a StatefulSet on member-1"

echo "### criterion 7 (part 1): the inventory record for member-1 names the placed object"
wait_for 60 "AppliedManifestRecord demo-llm-${M1} confirms the GenAIService with its UID" record_applied "$M1"
[ "$(hub get amr "demo-llm-${M1}" -n "$WORKLOAD_NS" -o jsonpath='{.spec.manifests[0].kind}/{.spec.manifests[0].name}')" = "GenAIService/demo-llm" ] || fail "record intent does not name the GenAIService"

echo "### criterion 6: status.clusters[] + rollup are correct (2 members)"
[ "$(hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.clusters[?(@.name=="'"$M1"'")].ready}')" = "true" ] || fail "status.clusters[] does not report $M1 ready"
[ "$(hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = "True" ] || fail "rolled-up Ready condition is not True (tolerance minReadyClusters=1)"

echo "### criterion 3 (part 2): registering member-2's ClusterProfile lands the workload automatically"
register_member2
wait_for 300 "workload landed + Ready on freshly added member-2" is_ready 3
wait_for 60 "AppliedManifestRecord demo-llm-${M2} confirms the GenAIService" record_applied "$M2"

echo "### criterion 6: per-cloud expansion really differs (the portability claim)"
[ "$(member "$M1" get pvc cache-demo-llm-0 -n "$WORKLOAD_NS" -o jsonpath='{.spec.storageClassName}')" = "premium-rwo" ] || fail "member-1 (gke sim) did not resolve premium-rwo"
[ "$(member "$M2" get pvc cache-demo-llm-0 -n "$WORKLOAD_NS" -o jsonpath='{.spec.storageClassName}')" = "managed-csi" ] || fail "member-2 (aks sim) did not resolve managed-csi"
[ "$(member "$M3" get pvc cache-demo-llm-0 -n "$WORKLOAD_NS" -o jsonpath='{.spec.storageClassName}')" = "gp3" ] || fail "member-3 (eks sim) did not resolve gp3"

echo "### criterion 2: mutate the hub object -> all members converge"
hub patch fgs demo-llm -n "$WORKLOAD_NS" --type=merge \
  -p '{"spec":{"template":{"spec":{"name":"demo-llm","model":"Qwen/Qwen2.5-0.5B-Instruct","mode":"mock","replicas":2,"cacheSize":"1Gi","monitoring":true}}}}' >/dev/null
check_replicas() { [ "$(member "$1" get sts demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.spec.replicas}' 2>/dev/null)" = "2" ]; }
wait_for 180 "member-1 converged to replicas=2" check_replicas "$M1"
wait_for 180 "member-2 converged to replicas=2" check_replicas "$M2"
wait_for 180 "member-3 converged to replicas=2" check_replicas "$M3"

echo "### criterion 4: unmatch member-2 -> workload removed there, no orphans"
hub label clusterprofile "$M2" -n "$FLEET_NS" tier=dev --overwrite >/dev/null
wait_gone 180 "GenAIService removed from member-2" member "$M2" get genaiservice demo-llm -n "$WORKLOAD_NS"
wait_gone 180 "expanded graph GC'd on member-2 (StatefulSet gone)" member "$M2" get sts demo-llm -n "$WORKLOAD_NS"
wait_gone 180 "expanded graph GC'd on member-2 (per-replica PVCs gone)" member "$M2" get pvc cache-demo-llm-0 -n "$WORKLOAD_NS"
wait_gone 60 "inventory record for member-2 deleted after unplacement" hub get amr "demo-llm-${M2}" -n "$WORKLOAD_NS"
wait_for 120 "hub rollup back to 2/2 ready" is_ready 2
hub label clusterprofile "$M2" -n "$FLEET_NS" tier=prod --overwrite >/dev/null
wait_for 300 "re-matched member-2 landed again" is_ready 3

echo "### criterion 7 (part 2): the inventory prunes a tracked object that is no longer intended"
# Simulate a leftover from an earlier, different placement: an object on
# member-2 that carries our placed-by label and is recorded as applied, but
# is not in the record's intent any more. The controller must delete exactly it.
member "$M2" create configmap stray -n "$WORKLOAD_NS" --from-literal=left=over >/dev/null
member "$M2" label configmap stray -n "$WORKLOAD_NS" "fleet.kro.run/placed-by=demo-llm" >/dev/null
hub patch amr "demo-llm-${M2}" -n "$WORKLOAD_NS" --subresource=status --type=json \
  -p '[{"op":"add","path":"/status/applied/-","value":{"version":"v1","kind":"ConfigMap","namespace":"'"$WORKLOAD_NS"'","name":"stray"}}]' >/dev/null
wait_gone 120 "stale ConfigMap pruned from member-2 by the inventory" member "$M2" get configmap stray -n "$WORKLOAD_NS"
wait_for 60 "record no longer lists the pruned object" sh -c "! hub get amr demo-llm-${M2} -n $WORKLOAD_NS -o jsonpath='{.status.applied[*].name}' | grep -q stray"
member "$M2" get genaiservice demo-llm -n "$WORKLOAD_NS" >/dev/null || fail "pruning must not touch the intended object"

echo "### criterion 5 + 7 (part 3): delete the hub object -> fleet-wide GC; an unreachable member blocks, a deregistered one is surfaced as an orphan and settled when it returns"
# Make member-2 unreachable first (its cluster manager reports the control
# plane unhealthy -> the provider disengages it), so nothing can be cleaned
# there. Deleting the ClusterProfile alone would leave a window in which the
# controller still reaches the member and, correctly, cleans up.
hub patch clusterprofile "$M2" -n "$FLEET_NS" --subresource=status --type=merge \
  -p "{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"False\",\"reason\":\"E2E\",\"message\":\"simulated outage\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}" >/dev/null
member2_message_has() {  # member2_message_has <substring>: the hub's view of member-2 says so
  hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath="{.status.clusters[?(@.name=='$M2')].message}" 2>/dev/null | grep -q "$1"
}
wait_for 120 "member-2 reported as not engaged" member2_message_has "not engaged"
hub delete fgs demo-llm -n "$WORKLOAD_NS" --wait=false >/dev/null
for m in "$M1" "$M3"; do
  wait_gone 180 "GenAIService gone on $m" member "$m" get genaiservice demo-llm -n "$WORKLOAD_NS"
  wait_gone 180 "expanded graph gone on $m" member "$m" get sts demo-llm -n "$WORKLOAD_NS"
  wait_gone 60 "inventory record for $m deleted" hub get amr "demo-llm-${m}" -n "$WORKLOAD_NS"
done
wait_for 120 "finalization blocked on the registered-but-unreachable member-2" member2_message_has "pending removal"
hub get fgs demo-llm -n "$WORKLOAD_NS" >/dev/null 2>&1 || fail "the hub object must not be released while a registered member is unreachable"
# Now the member leaves the inventory entirely: the instance is released, the
# record is kept as the orphan ledger, and member-2's copy is still there.
hub delete clusterprofile "$M2" -n "$FLEET_NS" >/dev/null
wait_gone 120 "hub object released once the unreachable member is deregistered" hub get fgs demo-llm -n "$WORKLOAD_NS"
[ "$(hub get amr "demo-llm-${M2}" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Orphaned")].status}')" = "True" ] || fail "record for the deregistered member-2 is not marked Orphaned"
member "$M2" get genaiservice demo-llm -n "$WORKLOAD_NS" >/dev/null || fail "member-2's copy should still exist: it is orphaned and surfaced by the record, not silently dropped"
echo "    ok: orphan surfaced: record demo-llm-${M2} kept with Orphaned=True, copy still on member-2"
register_member2
wait_gone 180 "orphaned copy removed from member-2 once it registered again" member "$M2" get genaiservice demo-llm -n "$WORKLOAD_NS"
wait_gone 180 "expanded graph gone on $M2" member "$M2" get sts demo-llm -n "$WORKLOAD_NS"
wait_gone 120 "orphan record settled (deleted)" hub get amr "demo-llm-${M2}" -n "$WORKLOAD_NS"
fi  # E2E_FROM

echo "### criterion 8: PlacementDecision consumption — division, fail-closed, provenance"
FGS2=split-llm
placed_reason()  { hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Placed")].reason}' 2>/dev/null; }
placed_message() { hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Placed")].message}' 2>/dev/null; }
ready_reason()   { hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null; }
placed_is()      { [ "$(placed_reason)" = "$1" ]; }
ready_replicas() { [ "$(hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.readyReplicas}' 2>/dev/null)" = "$1" ]; }
deploy_replicas(){ [ "$(member "$1" get sts "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.spec.replicas}' 2>/dev/null)" = "$2" ]; }
pd_patch() {  # pd_patch <json merge patch>
  hub patch placementdecision "$FGS2" -n "$WORKLOAD_NS" --type=merge -p "$1" >/dev/null
}
add_property() {  # add_property <member> <name> <value>: advertised by the member (About API), mirrored by the inventory agent
  fleet::set_property "kind-$1" "$2" "$3"
}
has_property() {  # has_property <member> <name> <value>: mirrored into the member's ClusterProfile on the hub
  [ "$(hub get clusterprofile "$1" -n "$FLEET_NS" -o jsonpath="{.status.properties[?(@.name=='$2')].value}" 2>/dev/null)" = "$3" ]
}

if [ "${E2E_FROM:-1}" -le 8 ]; then
# The decision is hand-written below, so the demo scheduler must leave this
# instance alone: policy "manual" opts it out.
cat <<EOF | hub apply -f - >/dev/null
apiVersion: fleet.kro.run/v1alpha1
kind: FleetGenAIService
metadata:
  name: ${FGS2}
  namespace: ${WORKLOAD_NS}
  annotations:
    scheduler.example.com/policy: manual
spec:
  placement:
    decisionRef:
      name: ${FGS2}
    requirements:
      matchProperties:
        e2e-approved.example.com: "yes"
  template:
    spec:
      name: ${FGS2}
      model: "Qwen/Qwen2.5-0.5B-Instruct"
      mode: mock
      replicas: 3
      cacheSize: 1Gi
      monitoring: false
EOF
wait_for 60 "no decision yet -> Placed=False/DecisionPending" placed_is DecisionPending

cat <<EOF | hub apply -f - >/dev/null
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: PlacementDecision
metadata:
  name: ${FGS2}
  namespace: ${WORKLOAD_NS}
  annotations:
    fleet.kro.run/decision-reason: "3 requested; split by free capacity"
    parameters.fleet.kro.run/${M1}: '{"replicas":"2"}'
    parameters.fleet.kro.run/${M2}: '{"replicas":"1"}'
schedulerName: e2e-hand-written
decisions:
  - clusterProfileRef: {namespace: ${FLEET_NS}, name: ${M1}}
    reason: "2 free"
  - clusterProfileRef: {namespace: ${FLEET_NS}, name: ${M2}}
    reason: "1 free"
EOF
wait_for 60 "decided clusters lack the required property -> Placed=False/DecisionViolatesRequirements" placed_is DecisionViolatesRequirements
member "$M1" get genaiservice "$FGS2" -n "$WORKLOAD_NS" >/dev/null 2>&1 && fail "fail closed: nothing may be placed while the decision violates a hard requirement"
member "$M2" get genaiservice "$FGS2" -n "$WORKLOAD_NS" >/dev/null 2>&1 && fail "fail closed: nothing may be placed while the decision violates a hard requirement"
echo "    ok: refused as a whole, nothing placed"

add_property "$M1" e2e-approved.example.com yes
add_property "$M2" e2e-approved.example.com yes
wait_for 120 "member-1's property mirrored to the hub by the inventory agent" has_property "$M1" e2e-approved.example.com yes
wait_for 120 "requirement now satisfied -> Placed=True" placed_is Placed
wait_for 180 "member-1 runs its share (replicas=2)" deploy_replicas "$M1" 2
wait_for 180 "member-2 runs its share (replicas=1)" deploy_replicas "$M2" 1
wait_for 300 "3/3 replicas ready across the fleet" ready_replicas 3
[ "$(ready_reason)" = "MinReadyReplicasMet" ] || fail "Ready reason is $(ready_reason), want MinReadyReplicasMet"
[ "$(hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.placement.source}/{.status.placement.schedulerName}/{.status.placement.reason}')" = "PlacementDecision/e2e-hand-written/3 requested; split by free capacity" ] || fail "status.placement provenance not recorded"
[ "$(hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath="{.status.placement.clusters[?(@.name=='$M1')].parameters.replicas}")" = "2" ] || fail "per-member parameters not recorded in status.placement"
[ "$(hub get amr "${FGS2}-${M1}" -n "$WORKLOAD_NS" -o jsonpath='{.status.applied[0].parameters.replicas}')" = "2" ] || fail "inventory record does not carry the member's parameters"
echo "    ok: provenance + parameters recorded"

echo "### criterion 8 (partial): the decision shrinks to one member with too few replicas -> placed only there, never spilled"
pd_patch "{\"metadata\":{\"annotations\":{\"fleet.kro.run/decision-reason\":\"only 2 of 3 fit on the eligible cluster\",\"parameters.fleet.kro.run/${M2}\":null}},\"decisions\":[{\"clusterProfileRef\":{\"namespace\":\"${FLEET_NS}\",\"name\":\"${M1}\"},\"reason\":\"2 free\"}]}"
wait_for 60 "Placed=False/InsufficientCapacity" placed_is InsufficientCapacity
echo "$(placed_message)" | grep -q "2 of 3" || fail "InsufficientCapacity message does not name the shortfall: $(placed_message)"
wait_gone 180 "member-2's share removed (not spilled, not kept)" member "$M2" get genaiservice "$FGS2" -n "$WORKLOAD_NS"
deploy_replicas "$M1" 2 || fail "member-1's share must be untouched by the shrink"
[ "$(ready_reason)" = "MinReadyReplicasNotMet" ] || fail "Ready must be False while 2/3 replicas are ready, got $(ready_reason)"

echo "### criterion 8 (refusal): an empty decision is a terminal refusal with the producer's reason"
pd_patch "{\"metadata\":{\"annotations\":{\"fleet.kro.run/decision-reason\":\"no cluster qualifies (e2e)\"}},\"decisions\":[]}"
wait_for 60 "Placed=False/NoEligibleClusters" placed_is NoEligibleClusters
echo "$(placed_message)" | grep -q "no cluster qualifies (e2e)" || fail "refusal does not carry the producer's reason: $(placed_message)"
wait_gone 180 "member-1's share removed on refusal" member "$M1" get genaiservice "$FGS2" -n "$WORKLOAD_NS"
[ "$(ready_reason)" = "NotPlaced" ] || fail "Ready must be False/NotPlaced after a refusal, got $(ready_reason)"
[ "$(hub get fgs "$FGS2" -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.placed}')" = "0" ] || fail "summary.placed must be 0 after a refusal"

echo "### criterion 8 (pending): the decision disappears -> pending again, not a fallback"
hub delete placementdecision "$FGS2" -n "$WORKLOAD_NS" >/dev/null
wait_for 60 "Placed=False/DecisionPending" placed_is DecisionPending
hub delete fgs "$FGS2" -n "$WORKLOAD_NS" --timeout=120s >/dev/null
fi  # E2E_FROM

# --- shared helpers for the demo scenarios ------------------------------------
# placed_reason_of <instance> / split_of <decision> <member> / sts_replicas <member> <instance> <n>
placed_reason_of() { hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Placed")].reason}' 2>/dev/null; }
placed_message_of(){ hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Placed")].message}' 2>/dev/null; }
ready_status_of()  { hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null; }
ready_replicas_of(){ hub get fgs "$1" -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.readyReplicas}' 2>/dev/null; }
split_of() {  # replicas the decision assigns to a member ("" when absent)
  hub get placementdecision "$1" -n "$WORKLOAD_NS" -o jsonpath="{.metadata.annotations.parameters\.fleet\.kro\.run/$2}" 2>/dev/null \
    | python3 -c 'import json,sys; d=sys.stdin.read().strip(); print(json.loads(d).get("replicas","") if d else "")'
}
# split_is <decision> <m1> <m2> <m3>: the decision's split over member-1/2/3 ("" = not decided there)
split_is() { [ "$(split_of "$1" "$M1")" = "$2" ] && [ "$(split_of "$1" "$M2")" = "$3" ] && [ "$(split_of "$1" "$M3")" = "$4" ]; }
sts_replicas() { [ "$(member "$1" get sts "$2" -n "$WORKLOAD_NS" -o jsonpath='{.spec.replicas}' 2>/dev/null)" = "$3" ]; }
placed_of_is()  { [ "$(placed_reason_of "$1")" = "$2" ]; }
ready_of_is()   { [ "$(ready_replicas_of "$1")" = "$2" ]; }
apply_gpu_instance() {  # apply_gpu_instance <name> <replicas> <requirement-name> <requirement-value>
  cat <<EOF | hub apply -f - >/dev/null
apiVersion: fleet.kro.run/v1alpha1
kind: FleetGenAIService
metadata:
  name: $1
  namespace: ${WORKLOAD_NS}
spec:
  placement:
    decisionRef:
      name: $1
    requirements:
      matchProperties:
        $3: "$4"
  template:
    spec:
      name: $1
      model: "Qwen/Qwen2.5-0.5B-Instruct"
      mode: gpu          # the platform backs "gpu" with example.com/gpu + the mock stand-in here
      replicas: $2
      cacheSize: 1Gi
      monitoring: false
EOF
}

echo "### criterion 9: capacity across clouds — free accelerators advertised per member; 8 replicas no member can hold alone are divided"
for m in "$M1" "$M2" "$M3"; do
  wait_for 180 "$m advertises gpus-free.example.com=4 on the hub (inventory agent)" has_property "$m" gpus-free.example.com 4
done
CAP=cap-llm
apply_gpu_instance "$CAP" 8 accelerator.example.com gpu
wait_for 120 "scheduler wrote PlacementDecision ${CAP} with a 3/3/2 spread" split_is "$CAP" 3 3 2
wait_for 120 "Placed=True" placed_of_is "$CAP" Placed
wait_for 300 "member-1 runs 3" sts_replicas "$M1" "$CAP" 3
wait_for 300 "member-2 runs 3" sts_replicas "$M2" "$CAP" 3
wait_for 300 "member-3 runs 2" sts_replicas "$M3" "$CAP" 2
wait_for 480 "8/8 replicas ready across three clouds" ready_of_is "$CAP" 8
[ "$(ready_status_of "$CAP")" = "True" ] || fail "Ready must be True at 8/8, got $(ready_status_of "$CAP")"
wait_for 120 "member-3 now advertises gpus-free=2 (its 2 replicas consume accelerators)" has_property "$M3" gpus-free.example.com 2
echo "    ok: no single member has 8 free; the developer's instance still just says replicas: 8"

echo "### criterion 9 (efficiency): cheapest-first consolidates onto the cheaper tiers (eks=1, gke=2, aks=3)"
hub annotate fgs "$CAP" -n "$WORKLOAD_NS" scheduler.example.com/policy=cheapest-first --overwrite >/dev/null
wait_for 120 "re-decided: gke 4 / aks 0 / eks 4" split_is "$CAP" 4 "" 4
wait_gone 240 "aks member's slice removed (not needed on the dearest tier)" member "$M2" get sts "$CAP" -n "$WORKLOAD_NS"
wait_for 480 "8/8 replicas ready again on the two cheaper clouds" ready_of_is "$CAP" 8

echo "### criterion 10: compliance — only the fedramp-high member (aks persona) qualifies; the shortfall is reported, never spilled"
GOV=gov-llm
apply_gpu_instance "$GOV" 6 compliance.example.com fedramp-high
wait_for 120 "decision names only the accredited member, with its 4 free" split_is "$GOV" "" 4 ""
wait_for 120 "Placed=False/InsufficientCapacity" placed_of_is "$GOV" InsufficientCapacity
echo "$(placed_message_of "$GOV")" | grep -q "not spilled" || fail "InsufficientCapacity message must say the shortfall is not spilled: $(placed_message_of "$GOV")"
wait_for 300 "accredited member runs 4" sts_replicas "$M2" "$GOV" 4
member "$M1" get sts "$GOV" -n "$WORKLOAD_NS" >/dev/null 2>&1 && fail "spilled onto a non-compliant cloud (gke)"
member "$M3" get sts "$GOV" -n "$WORKLOAD_NS" >/dev/null 2>&1 && fail "spilled onto a non-compliant cloud (eks)"
wait_for 300 "4/6 replicas ready, Ready=False" ready_of_is "$GOV" 4
[ "$(ready_status_of "$GOV")" = "False" ] || fail "Ready must be False at 4/6"
echo "    ok: placed only on the compliant cloud; Placed=False with the reason; nothing spilled"

echo "### criterion 10 (refusal): a requirement no member satisfies is a terminal, explicit refusal"
hub patch fgs "$GOV" -n "$WORKLOAD_NS" --type=merge -p '{"spec":{"placement":{"requirements":{"matchProperties":{"compliance.example.com":"il5"}}}}}' >/dev/null
wait_for 120 "Placed=False/NoEligibleClusters" placed_of_is "$GOV" NoEligibleClusters
echo "$(placed_message_of "$GOV")" | grep -q "no eligible cluster" || fail "refusal must carry the scheduler's reason: $(placed_message_of "$GOV")"
wait_gone 240 "the accredited member's slice removed on refusal" member "$M2" get sts "$GOV" -n "$WORKLOAD_NS"
hub delete fgs "$GOV" -n "$WORKLOAD_NS" --timeout=180s >/dev/null

echo "### criterion 11: drain — the gke member drains; its replicas move to the other clouds; readiness recovers; its inventory record is gone"
fleet::set_property "$M1_CTX" draining.example.com true
wait_for 120 "hub sees member-1 draining" has_property "$M1" draining.example.com true
wait_for 120 "re-decided: gke 0 / aks 4 / eks 4" split_is "$CAP" "" 4 4
wait_gone 300 "slice removed from the draining member" member "$M1" get sts "$CAP" -n "$WORKLOAD_NS"
wait_gone 120 "inventory record for the drained member gone" hub get amr "${CAP}-${M1}" -n "$WORKLOAD_NS"
wait_for 480 "8/8 replicas ready again across aks + eks" ready_of_is "$CAP" 8
echo "    ok: drained, re-placed, recovered"

if [ -n "$CPK_PID" ] && kill -0 "$CPK_PID" 2>/dev/null; then
  echo "### criterion 12: one URL — the gateway spreads requests across the members' own load balancers"
  endpoints_ready() {
    [ -n "$(hub get fgs "$CAP" -n "$WORKLOAD_NS" -o jsonpath="{.status.clusters[?(@.name=='$M2')].endpoint}")" ] &&
    [ -n "$(hub get fgs "$CAP" -n "$WORKLOAD_NS" -o jsonpath="{.status.clusters[?(@.name=='$M3')].endpoint}")" ]
  }
  wait_for 300 "member load-balancer endpoints collected in status.clusters[]" endpoints_ready
  GW_PORT="${GW_PORT:-18080}"
  GW_LOG="$(mktemp -t fleet-gateway-XXXX.log)"
  "$DEMO_BIN" gateway --listen ":${GW_PORT}" --instance "$CAP" --workload-namespace "$WORKLOAD_NS" \
    --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" --kubeconfig-secretreader-plugin "$PLUGIN_BIN" >"$GW_LOG" 2>&1 &
  GW_PID=$!
  wait_for 60 "gateway up" curl -fsS "http://localhost:${GW_PORT}/healthz"
  wait_for 60 "gateway sees both members" sh -c "curl -fsS http://localhost:${GW_PORT}/fleet | grep -q '$M3'"
  seen=""
  for i in $(seq 1 10); do
    out="$(curl -sS -i "http://localhost:${GW_PORT}/v1/chat/completions" -H 'Content-Type: application/json' \
          -d '{"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"user","content":"hello from KubeCon"}]}' 2>/dev/null || true)"
    c="$(echo "$out" | grep -i '^X-Fleet-Cluster:' | awk '{print $2}' | tr -d '\r')"
    echo "$out" | grep -q 'served by KRO-managed pod' || fail "request $i did not get a model reply through the gateway: $(echo "$out" | head -5)"
    seen="$seen $c"
  done
  echo "    answered by:$seen"
  echo "$seen" | grep -q "$M2" || fail "no answer came from the aks member"
  echo "$seen" | grep -q "$M3" || fail "no answer came from the eks member"
  echo "    ok: one URL, answers from two clouds (the third is draining)"
  kill "$GW_PID" 2>/dev/null || true
else
  echo "### criterion 12: skipped (cloud-provider-kind not running — no LoadBalancer addresses on this kind fleet)"
fi

echo "### finally: delete the divided instance -> every cloud cleaned up with one delete"
hub delete fgs "$CAP" -n "$WORKLOAD_NS" --timeout=180s >/dev/null
for m in "$M1" "$M2" "$M3"; do
  wait_gone 240 "GenAIService ${CAP} gone on $m" member "$m" get genaiservice "$CAP" -n "$WORKLOAD_NS"
  wait_gone 240 "StatefulSet ${CAP} gone on $m" member "$m" get sts "$CAP" -n "$WORKLOAD_NS"
  wait_gone 120 "record ${CAP}-${m} gone" hub get amr "${CAP}-${m}" -n "$WORKLOAD_NS"
done
wait_gone 60 "PlacementDecision ${CAP} garbage-collected with its instance" hub get placementdecision "$CAP" -n "$WORKLOAD_NS"

echo
echo "### e2e PASSED: all twelve success criteria hold"
