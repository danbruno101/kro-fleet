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
#
# Usage: scripts/e2e.sh          (creates the fleet via setup-fleet.sh, runs the
#                                 controller as a host process, asserts, cleans up)
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
M1="${PREFIX}-member-1"; M1_CTX="kind-${M1}"
M2="${PREFIX}-member-2"; M2_CTX="kind-${M2}"
CTRL_LOG="$(mktemp -t fleet-controller-XXXX.log)"
CTRL_PID=""

hub()    { kubectl --context "$HUB_CTX" "$@"; }
member() { local m=$1; shift; kubectl --context "kind-${m}" "$@"; }

fail() {
  echo "!!! FAIL: $*" >&2
  echo "--- fleet controller log (tail) ---" >&2; tail -50 "$CTRL_LOG" >&2 || true
  echo "--- hub FleetGenAIServices ---" >&2; hub get fgs -A -o yaml 2>/dev/null | tail -80 >&2 || true
  exit 1
}

# wait_for <timeout-seconds> <description> <command...>   (command must succeed)
wait_for() {
  local timeout=$1 desc=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
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
    [ "$(date +%s)" -ge "$deadline" ] && fail "timed out waiting for: $desc"
    sleep 5
  done
  echo "    ok: $desc"
}

ready_count() { hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.summary.ready}' 2>/dev/null; }
is_ready()    { [ "$(ready_count)" = "$1" ]; }

cleanup() {
  [ -n "$CTRL_PID" ] && kill "$CTRL_PID" 2>/dev/null || true
  if [ "${KEEP_FLEET:-}" != "true" ] && [ -n "${CI:-}" ]; then
    "$REPO_ROOT/scripts/teardown-fleet.sh" || true
  fi
}
trap cleanup EXIT

echo "### e2e: setting up the fleet (1 hub + 2 members)"
"$REPO_ROOT/scripts/setup-fleet.sh" 2

echo "### e2e: starting the fleet controller (host process)"
CTRL_BIN="$(mktemp -t fleet-controller-XXXX)"
( cd "$REPO_ROOT" && go build -o "$CTRL_BIN" ./cmd/fleet-controller )
"$CTRL_BIN" --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" >"$CTRL_LOG" 2>&1 &
CTRL_PID=$!

echo "### criterion 3 (part 1): member-2 is NOT registered when the workload is placed"
hub delete clusterprofile "$M2" -n "$FLEET_NS" --ignore-not-found >/dev/null

echo "### criterion 1: place -> Ready on every matching member"
hub apply -f "$REPO_ROOT/examples/fleetgenaiservice-sample.yaml"
wait_for 420 "workload Ready on member-1 (real kro expansion)" is_ready 1
member "$M1" get deploy demo-llm -n "$WORKLOAD_NS" >/dev/null || fail "kro did not expand a Deployment on member-1"

echo "### criterion 6: status.clusters[] + rollup are correct (1 member)"
[ "$(hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.clusters[?(@.name=="'"$M1"'")].ready}')" = "true" ] || fail "status.clusters[] does not report $M1 ready"
[ "$(hub get fgs demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = "True" ] || fail "rolled-up Ready condition is not True (tolerance minReadyClusters=1)"

echo "### criterion 3 (part 2): registering member-2's ClusterProfile lands the workload automatically"
cat <<EOF | hub apply -f - >/dev/null
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${M2}
  namespace: ${FLEET_NS}
  labels:
    tier: prod
    fleet.kro.run/cloud: aks
spec:
  displayName: ${M2}
  clusterManager:
    name: kro-fleet
EOF
hub patch clusterprofile "$M2" -n "$FLEET_NS" --subresource=status --type=merge \
  -p "{\"status\":{\"conditions\":[{\"type\":\"ControlPlaneHealthy\",\"status\":\"True\",\"reason\":\"AssertedAtRegistration\",\"message\":\"e2e\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}" >/dev/null
wait_for 300 "workload landed + Ready on freshly added member-2" is_ready 2

echo "### criterion 6: per-cloud expansion really differs (the portability claim)"
[ "$(member "$M1" get pvc demo-llm-cache -n "$WORKLOAD_NS" -o jsonpath='{.spec.storageClassName}')" = "premium-rwo" ] || fail "member-1 (gke sim) did not resolve premium-rwo"
[ "$(member "$M2" get pvc demo-llm-cache -n "$WORKLOAD_NS" -o jsonpath='{.spec.storageClassName}')" = "managed-csi" ] || fail "member-2 (aks sim) did not resolve managed-csi"

echo "### criterion 2: mutate the hub object -> all members converge"
hub patch fgs demo-llm -n "$WORKLOAD_NS" --type=merge \
  -p '{"spec":{"template":{"spec":{"name":"demo-llm","model":"Qwen/Qwen2.5-0.5B-Instruct","mode":"mock","replicas":2,"cacheSize":"1Gi","monitoring":true}}}}' >/dev/null
check_replicas() { [ "$(member "$1" get deploy demo-llm -n "$WORKLOAD_NS" -o jsonpath='{.spec.replicas}' 2>/dev/null)" = "2" ]; }
wait_for 180 "member-1 converged to replicas=2" check_replicas "$M1"
wait_for 180 "member-2 converged to replicas=2" check_replicas "$M2"

echo "### criterion 4: unmatch member-2 -> workload removed there, no orphans"
hub label clusterprofile "$M2" -n "$FLEET_NS" tier=dev --overwrite >/dev/null
wait_gone 180 "GenAIService removed from member-2" member "$M2" get genaiservice demo-llm -n "$WORKLOAD_NS"
wait_gone 180 "expanded graph GC'd on member-2 (Deployment gone)" member "$M2" get deploy demo-llm -n "$WORKLOAD_NS"
wait_gone 180 "expanded graph GC'd on member-2 (PVC gone)" member "$M2" get pvc demo-llm-cache -n "$WORKLOAD_NS"
wait_for 120 "hub rollup back to 1/1 ready" is_ready 1
hub label clusterprofile "$M2" -n "$FLEET_NS" tier=prod --overwrite >/dev/null
wait_for 300 "re-matched member-2 landed again" is_ready 2

echo "### criterion 5: delete the hub object -> fleet-wide GC"
hub delete fgs demo-llm -n "$WORKLOAD_NS" --timeout=120s >/dev/null
for m in "$M1" "$M2"; do
  wait_gone 180 "GenAIService gone on $m" member "$m" get genaiservice demo-llm -n "$WORKLOAD_NS"
  wait_gone 180 "expanded graph gone on $m" member "$m" get deploy demo-llm -n "$WORKLOAD_NS"
done

echo
echo "### e2e PASSED: all six success criteria hold"
