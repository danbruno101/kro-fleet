#!/usr/bin/env bash
# =============================================================================
# headlamp-kubeconfig.sh — emit ONE kubeconfig containing the hub and every
# member context of the kind fleet, for pointing Headlamp at the whole fleet:
#
#   scripts/headlamp-kubeconfig.sh                # writes ./kro-fleet.kubeconfig
#   headlamp --kubeconfig "$PWD/kro-fleet.kubeconfig" \
#            --plugins-dir "$PWD/headlamp-plugin/dist-plugins"
#
# The output file contains live credentials: it is gitignored (*.kubeconfig)
# and must never be committed. Run scripts/setup-fleet.sh first.
# =============================================================================
set -euo pipefail

PREFIX="${PREFIX:-kro-fleet}"
OUT="${1:-${PREFIX}.kubeconfig}"

clusters=$(kind get clusters 2>/dev/null | grep "^${PREFIX}-" || true)
if [ -z "$clusters" ]; then
  echo "no kind clusters with prefix '${PREFIX}-' — run scripts/setup-fleet.sh first" >&2
  exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

for c in $clusters; do
  kind get kubeconfig --name "$c" > "${tmpdir}/${c}"
done

KUBECONFIG=$(printf '%s\n' "$tmpdir"/* | paste -sd:) \
  kubectl config view --flatten > "$OUT"
chmod 600 "$OUT"

echo ">>> wrote ${OUT} with contexts:"
KUBECONFIG="$OUT" kubectl config get-contexts -o name | sed 's/^/    /'
