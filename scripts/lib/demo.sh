#!/usr/bin/env bash
# =============================================================================
# scripts/lib/demo.sh — shared plumbing for the cloud demo scripts
# (provision.sh, demo.sh, reset.sh, teardown.sh).
#
#   * flags:     --simulated | --real-gpu, --dry-run, --step N, --yes, --no-pause
#   * preflight: CLIs present, logged in, kube contexts reachable
#   * steps:     narration -> show the command -> run it -> wait for Enter;
#                every step is recorded under logs/<run>/ for a backup video
#   * cost:      a per-hour estimate and an explicit confirmation before
#                anything billable is created
#
# Nothing here ever prints or stores a credential; kubeconfigs stay where the
# cloud CLIs put them, and the hub only learns each cluster's endpoint, CA and
# which exec plugin to call (ClusterProfile.status.accessProviders).
# =============================================================================

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
# shellcheck source=fleet.sh
. "${REPO_ROOT}/scripts/lib/fleet.sh"

# --- fleet-wide names (override via env) --------------------------------------
PREFIX="${PREFIX:-kro-fleet}"
HUB="${HUB:-${PREFIX}-hub}"
HUB_CTX="${HUB_CTX:-kind-${HUB}}"
FLEET_NS="${FLEET_NS:-fleet-system}"
WORKLOAD_NS="${WORKLOAD_NS:-fleet-demo}"
CONSUMER="${CONSUMER:-kro-fleet}"
TIER="${TIER:-prod}"
CLOUDS="${CLOUDS:-eks gke aks}"
# Cluster names and the kube-contexts the cloud CLIs are told to create.
EKS_CLUSTER="${EKS_CLUSTER:-${PREFIX}-eks}";  EKS_REGION="${EKS_REGION:-us-east-1}";    EKS_CTX="${EKS_CTX:-eks}"
GKE_CLUSTER="${GKE_CLUSTER:-${PREFIX}-gke}";  GKE_ZONE="${GKE_ZONE:-us-central1-a}";   GKE_CTX="${GKE_CTX:-gke}"
AKS_CLUSTER="${AKS_CLUSTER:-${PREFIX}-aks}";  AKS_LOCATION="${AKS_LOCATION:-eastus}";  AKS_CTX="${AKS_CTX:-aks}"
AKS_RG="${AKS_RG:-${PREFIX}-demo}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}"
KRO_CHART="${KRO_CHART:-oci://registry.k8s.io/kro/charts/kro}"
KRO_VERSION="${KRO_VERSION:-0.9.4}"
FAKE_GPUS="${FAKE_GPUS:-4}"            # simulated accelerators per cluster
FAKE_GPU_RESOURCE="${FAKE_GPU_RESOURCE:-example.com/gpu}"
GPU_NODES="${GPU_NODES:-4}"            # real-gpu: nodes x 1 GPU per cluster
STATE_DIR="${STATE_DIR:-${REPO_ROOT}/.fleet}"          # gitignored; no secrets
PLUGIN_BIN="${PLUGIN_BIN:-${REPO_ROOT}/bin/kubeconfig-secretreader-plugin}"
ACCESS_PROVIDERS_FILE="${ACCESS_PROVIDERS_FILE:-${STATE_DIR}/access-providers.json}"

# --- flags ----------------------------------------------------------------------
MODE="${MODE:-simulated}"      # simulated | real-gpu
DRY_RUN="${DRY_RUN:-false}"
START_STEP="${START_STEP:-1}"
ASSUME_YES="${ASSUME_YES:-false}"
NO_PAUSE="${NO_PAUSE:-false}"
[ -n "${CI:-}" ] && NO_PAUSE=true

demo::parse_flags() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --simulated) MODE=simulated ;;
      --real-gpu)  MODE=real-gpu ;;
      --dry-run)   DRY_RUN=true ;;
      --step)      START_STEP="$2"; shift ;;
      --step=*)    START_STEP="${1#--step=}" ;;
      --yes|-y)    ASSUME_YES=true ;;
      --no-pause)  NO_PAUSE=true ;;
      -h|--help)   grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
      *) echo "unknown flag: $1 (try --help)" >&2; exit 2 ;;
    esac
    shift
  done
}

# --- output + logs ----------------------------------------------------------------
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)}"
LOG_DIR="${LOG_DIR:-${REPO_ROOT}/logs/$(basename "$0" .sh)-${RUN_ID}}"
demo::init_logs() {
  mkdir -p "$LOG_DIR"
  # Everything on stdout/stderr also lands in the run log (the backup video's script).
  exec > >(tee -a "${LOG_DIR}/run.log") 2>&1
  echo "# $(basename "$0") run ${RUN_ID}  mode=${MODE} dry-run=${DRY_RUN}  logs: ${LOG_DIR}"
}
bold()  { printf "\033[1m%s\033[0m\n" "$*"; }
cyan()  { printf "\033[1;36m%s\033[0m\n" "$*"; }
dim()   { printf "\033[2m%s\033[0m\n" "$*"; }
warn()  { printf "\033[1;33m!!! %s\033[0m\n" "$*" >&2; }
die()   { printf "\033[1;31m!!! %s\033[0m\n" "$*" >&2; exit 1; }

# run <cmd...>: show, then execute (or only show under --dry-run). Output is
# also appended to the current step's log.
STEP_LOG=""
run() {
  printf "  \033[2m\$\033[0m %s\n" "$*"
  if [ "$DRY_RUN" = "true" ]; then return 0; fi
  if [ -n "$STEP_LOG" ]; then "$@" 2>&1 | tee -a "$STEP_LOG"; else "$@"; fi
}
# run_sh "<shell snippet>": like run, for pipelines/heredocs.
run_sh() {
  printf "  \033[2m\$\033[0m %s\n" "$1"
  if [ "$DRY_RUN" = "true" ]; then return 0; fi
  if [ -n "$STEP_LOG" ]; then bash -o pipefail -c "$1" 2>&1 | tee -a "$STEP_LOG"; else bash -o pipefail -c "$1"; fi
}

# --- steps -------------------------------------------------------------------------
STEP_NO=0
# step <title> <narration...>: begins a numbered step; returns 1 (skip) when
# resuming past it with --step N. Use:  if step "..." "..."; then ...; fi
step() {
  STEP_NO=$((STEP_NO + 1))
  local title=$1; shift
  if [ "$STEP_NO" -lt "$START_STEP" ]; then
    dim "[step ${STEP_NO}] ${title} — skipped (resuming at ${START_STEP})"
    return 1
  fi
  echo
  cyan "━━━ step ${STEP_NO}: ${title}"
  [ $# -gt 0 ] && bold "    $*"
  STEP_LOG="${LOG_DIR}/step-$(printf '%02d' "$STEP_NO").log"
  { echo "# step ${STEP_NO}: ${title}"; [ $# -gt 0 ] && echo "# $*"; } > "$STEP_LOG"
  return 0
}
# say <text>: one-line narration (also logged).
say() { bold "    $*"; }
# pause: wait for Enter unless --no-pause / CI / --dry-run.
pause() {
  if [ "$NO_PAUSE" = "true" ] || [ "$DRY_RUN" = "true" ]; then return 0; fi
  printf "\n    \033[2m[Enter to continue]\033[0m "; read -r _ </dev/tty
}
# confirm <question>: explicit yes, unless --yes or --dry-run.
confirm() {
  if [ "$ASSUME_YES" = "true" ]; then return 0; fi
  if [ "$DRY_RUN" = "true" ]; then dim "(dry-run: would ask) $1"; return 0; fi
  printf "    %s [y/N] " "$1"; read -r reply </dev/tty
  [ "$reply" = "y" ] || [ "$reply" = "yes" ]
}

# --- preflight -----------------------------------------------------------------------
# need_cli <cmd> [hint]: required unless --dry-run (then a warning).
need_cli() {
  if command -v "$1" >/dev/null 2>&1; then return 0; fi
  if [ "$DRY_RUN" = "true" ]; then warn "missing CLI: $1 ${2:+($2)} — continuing because of --dry-run"; return 0; fi
  die "missing CLI: $1 ${2:+($2)}"
}
# check_login <cloud>: a cheap authenticated call per cloud.
check_login() {
  [ "$DRY_RUN" = "true" ] && { dim "(dry-run) skipping ${1} login check"; return 0; }
  case "$1" in
    eks) aws sts get-caller-identity >/dev/null 2>&1 || die "aws: not logged in (aws configure / aws sso login)" ;;
    gke) gcloud auth print-access-token >/dev/null 2>&1 || die "gcloud: not logged in (gcloud auth login)"
         [ -n "$(gcloud config get-value project 2>/dev/null)" ] || die "gcloud: no project set (gcloud config set project ...)" ;;
    aks) az account show >/dev/null 2>&1 || die "az: not logged in (az login)" ;;
  esac
}
demo::preflight() {
  bold "preflight: CLIs"
  need_cli kubectl; need_cli helm; need_cli kind "the hub runs on kind"; need_cli docker; need_cli go "builds the controller and the demo binary"
  need_cli python3 "pretty-printing"
  for c in $CLOUDS; do
    case "$c" in
      eks) need_cli aws; need_cli eksctl "EKS Auto Mode cluster creation" ;;
      gke) need_cli gcloud; need_cli gke-gcloud-auth-plugin "gcloud components install gke-gcloud-auth-plugin" ;;
      aks) need_cli az; need_cli kubelogin "az aks install-cli" ;;
    esac
  done
  bold "preflight: logins"
  for c in $CLOUDS; do check_login "$c"; done
  echo "    ok"
}

# --- cost ------------------------------------------------------------------------------
# Rough list prices (verify with each cloud's calculator before a long rehearsal).
demo::cost_table() {
  echo
  bold "estimated cost (per hour, list prices, approximate)"
  printf "    %-6s %-42s %8s\n" cloud "what" "USD/h"
  printf "    %-6s %-42s %8s\n" eks  "Auto Mode control plane + 2 x m5.large"    "0.30"
  printf "    %-6s %-42s %8s\n" gke  "Standard control plane + 2 x e2-standard-4" "0.37"
  printf "    %-6s %-42s %8s\n" aks  "free control plane + 2 x Standard_D4s_v3"   "0.38"
  if [ "$MODE" = "real-gpu" ]; then
    printf "    %-6s %-42s %8s\n" eks  "+ ${GPU_NODES} x g6.xlarge (1 x L4 each)"      "$(echo "${GPU_NODES} * 0.81" | bc 2>/dev/null || echo "~3.2")"
    printf "    %-6s %-42s %8s\n" gke  "+ ${GPU_NODES} x g2-standard-4 (1 x L4 each)"  "$(echo "${GPU_NODES} * 0.70" | bc 2>/dev/null || echo "~2.8")"
    printf "    %-6s %-42s %8s\n" aks  "+ ${GPU_NODES} x Standard_NC4as_T4_v3 (1 x T4)" "$(echo "${GPU_NODES} * 0.53" | bc 2>/dev/null || echo "~2.1")"
    printf "    %-6s %-42s %8s\n" ""   "total, real-gpu"                              "~9.2"
    dim  "    GPU quotas must be granted beforehand on all three clouds; see docs/demo-cloud.md."
  else
    printf "    %-6s %-42s %8s\n" ""   "total, simulated (CPU only)"                  "~1.05"
  fi
  printf "    %-6s %-42s %8s\n" hub  "kind on this machine"                          "0.00"
  dim  "    load balancers add ~0.02-0.03/h each while an instance is placed."
}

# --- cloud access providers (KEP-4322 / KEP-5339) -------------------------------------
# The hub never holds a cloud credential: each ClusterProfile names the exec
# plugin that mints one from the operator's own logins on this machine.
demo::write_access_providers() {
  mkdir -p "$STATE_DIR"
  cat > "$ACCESS_PROVIDERS_FILE" <<'EOF'
{
  "providers": [
    {
      "name": "eks",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1beta1",
        "command": "aws",
        "args": ["eks", "get-token", "--output", "json"],
        "provideClusterInfo": true
      },
      "profileSourcedCLIArgsPolicy": "Append"
    },
    {
      "name": "gke",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1beta1",
        "command": "gke-gcloud-auth-plugin",
        "provideClusterInfo": true
      }
    },
    {
      "name": "aks",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1beta1",
        "command": "kubelogin",
        "args": ["get-token", "--login", "azurecli", "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630"],
        "provideClusterInfo": true
      }
    }
  ]
}
EOF
}

# demo::register_cloud_member <cloud> <profile-name> <kube-context> [additional-args-json]
#
# ClusterProfile on the hub for a cloud cluster: labels, the cloud's access
# provider with the endpoint + CA taken from the kubeconfig the cloud CLI
# wrote, cluster-specific exec args as a KEP-5339 extension, and the
# self-asserted ControlPlaneHealthy (no cluster-manager agent in the demo).
demo::register_cloud_member() {
  local cloud=$1 name=$2 ctx=$3 extra_args=${4:-[]}
  local cluster server ca
  cluster="$(kubectl config view --raw -o jsonpath="{.contexts[?(@.name=='${ctx}')].context.cluster}")"
  server="$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='${cluster}')].cluster.server}")"
  ca="$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='${cluster}')].cluster.certificate-authority-data}")"
  [ -n "$server" ] && [ -n "$ca" ] || die "context ${ctx}: no server/CA in kubeconfig (run the cloud CLI's get-credentials first)"
  cat <<EOF | kubectl --context "$HUB_CTX" apply -f - >/dev/null
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: ${name}
  namespace: ${FLEET_NS}
  labels:
    tier: ${TIER}
    fleet.kro.run/cloud: ${cloud}
spec:
  displayName: ${name}
  clusterManager:
    name: ${CONSUMER}
EOF
  kubectl --context "$HUB_CTX" patch clusterprofile "$name" -n "$FLEET_NS" --subresource=status --type=merge -p "$(cat <<EOF
{"status":{
  "conditions":[{"type":"ControlPlaneHealthy","status":"True","reason":"AssertedAtRegistration",
                 "message":"registered by scripts/demo.sh","lastTransitionTime":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}],
  "accessProviders":[{"name":"${cloud}","cluster":{
     "server":"${server}","certificate-authority-data":"${ca}",
     "extensions":[{"name":"clusterprofiles.multicluster.x-k8s.io/exec/additional-args","extension":${extra_args}}]}}]
}}
EOF
)" >/dev/null
}

# demo::ctx_of <cloud> / demo::name_of <cloud>
demo::ctx_of()  { case "$1" in eks) echo "$EKS_CTX";; gke) echo "$GKE_CTX";; aks) echo "$AKS_CTX";; esac; }
demo::name_of() { case "$1" in eks) echo "$EKS_CLUSTER";; gke) echo "$GKE_CLUSTER";; aks) echo "$AKS_CLUSTER";; esac; }
has_context() { kubectl config get-contexts -o name 2>/dev/null | grep -qx "$1"; }

# demo::render <file>: fill the __PLACEHOLDERS__ of config/clouds/*/platform.yaml for MODE.
demo::render() {
  local f=$1 accel image cpu mem
  if [ "$MODE" = "real-gpu" ]; then
    accel="nvidia.com/gpu"; image="vllm/vllm-openai:latest"; cpu="2"; mem="8Gi"
  else
    accel="$FAKE_GPU_RESOURCE"; image="ghcr.io/danbruno101/mock-vllm:demo"; cpu="100m"; mem="128Mi"
  fi
  sed -e "s#__ACCELERATOR__#${accel}#g" -e "s#__SERVING_IMAGE__#${image}#g" \
      -e "s#__SERVING_CPU__#${cpu}#g" -e "s#__SERVING_MEMORY__#${mem}#g" \
      -e "s#__IDENTITY_ANNOTATION__#${IDENTITY_ANNOTATION:-}#g" -e "s#__IDENTITY__#${IDENTITY:-}#g" "$f"
}

# --- background processes (controller, demo agent+scheduler, gateway) ------------------------
demo::start_background() {
  mkdir -p "$STATE_DIR"
  local ctrl="${STATE_DIR}/fleet-controller" demo="${STATE_DIR}/fleet-demo"
  run_sh "cd '${REPO_ROOT}' && go build -o '${ctrl}' ./cmd/fleet-controller && go build -o '${demo}' ./cmd/fleet-demo"
  run_sh "cd '${REPO_ROOT}' && go build -o '${PLUGIN_BIN}' sigs.k8s.io/cluster-inventory-api/plugins/kubeconfig-secretreader/cmd/plugin"
  demo::write_access_providers
  if [ "$DRY_RUN" = "true" ]; then return 0; fi
  demo::stop_background
  "$ctrl" --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" --access-providers-file "$ACCESS_PROVIDERS_FILE" \
    > "${LOG_DIR}/fleet-controller.log" 2>&1 & echo $! > "${STATE_DIR}/fleet-controller.pid"
  "$demo" all --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" --access-providers-file "$ACCESS_PROVIDERS_FILE" \
    > "${LOG_DIR}/fleet-demo.log" 2>&1 & echo $! > "${STATE_DIR}/fleet-demo.pid"
  dim "    controller + demo agent/scheduler running (logs in ${LOG_DIR})"
}
demo::start_gateway() {
  local instance=$1 port=${GW_PORT:-8080}
  [ "$DRY_RUN" = "true" ] && return 0
  [ -f "${STATE_DIR}/fleet-gateway.pid" ] && kill "$(cat "${STATE_DIR}/fleet-gateway.pid")" 2>/dev/null || true
  "${STATE_DIR}/fleet-demo" gateway --listen ":${port}" --instance "$instance" --workload-namespace "$WORKLOAD_NS" \
    --hub-context "$HUB_CTX" --fleet-namespace "$FLEET_NS" --access-providers-file "$ACCESS_PROVIDERS_FILE" \
    > "${LOG_DIR}/fleet-gateway.log" 2>&1 & echo $! > "${STATE_DIR}/fleet-gateway.pid"
}
demo::stop_background() {
  local p
  for p in fleet-gateway fleet-demo fleet-controller; do
    [ -f "${STATE_DIR}/${p}.pid" ] && { kill "$(cat "${STATE_DIR}/${p}.pid")" 2>/dev/null || true; rm -f "${STATE_DIR}/${p}.pid"; }
  done
  return 0
}

# --- waiting -----------------------------------------------------------------------------
# wait_until <seconds> <description> <cmd...>
wait_until() {
  local timeout=$1 desc=$2; shift 2
  [ "$DRY_RUN" = "true" ] && { dim "(dry-run) would wait for: ${desc}"; return 0; }
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$deadline" ] && die "timed out waiting for: ${desc}"
    sleep 3
  done
  echo "    ✓ ${desc}"
}
hub() { kubectl --context "$HUB_CTX" "$@"; }
