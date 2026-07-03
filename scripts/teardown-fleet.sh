#!/usr/bin/env bash
# Tear down every kind cluster created by setup-fleet.sh (hub + members).
set -euo pipefail

PREFIX="${PREFIX:-kro-fleet}"

for cluster in $(kind get clusters 2>/dev/null | grep -E "^${PREFIX}-(hub|member-[0-9]+)$" || true); do
  echo ">>> deleting kind cluster ${cluster}"
  kind delete cluster --name "$cluster"
done
echo ">>> done"
