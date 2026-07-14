# kro-fleet Headlamp plugin

Visualizes the fleet demo: **one `FleetGenAIService` on the hub → running across
the member clusters**, in one Headlamp instance.

- **KRO Fleet** sidebar view: the hub object with per-member
  `status.clusters[]`, the ClusterProfile inventory, and every placed pod on
  every member — with streaming logs.
- **Map source**: the cross-cluster object graph in Headlamp's resource-map
  view — hub `FleetGenAIService` → placed `GenAIService` per member → the
  kro-expanded children (Deployment/Service/PVC) on that member.

Built with `@kinvolk/headlamp-plugin` **0.14.0** (pinned). API patterns follow
the official Cluster API plugin (`registerMapSource`) and Kubeflow plugin
(per-cluster `useGet` + `getLogs`) shipped with that package. Desk-validated
findings and the remaining live-validation checklist: see
[`../docs/headlamp-phase0.md`](../docs/headlamp-phase0.md).

## Run it against the kind fleet

```bash
# 1. Fleet up (from the repo root): 1 hub + 3 members (gke/aks/eks personas)
scripts/setup-fleet.sh 3
go run ./cmd/fleet-controller --hub-context kind-kro-fleet-hub &
kubectl --context kind-kro-fleet-hub apply -f examples/fleetgenaiservice-sample.yaml

# 2. One kubeconfig with hub + member contexts (gitignored — never commit it)
scripts/headlamp-kubeconfig.sh

# 3. Build the plugin and lay it out for Headlamp
cd headlamp-plugin
npm install
npm run build
mkdir -p dist-plugins/kro-fleet
cp dist/main.js package.json dist-plugins/kro-fleet/

# 4. Point Headlamp at the fleet + the plugin
headlamp --kubeconfig "$PWD/../kro-fleet.kubeconfig" --plugins-dir "$PWD/dist-plugins"
```

For plugin development, `npm start` watches and deploys into the local
Headlamp desktop app's plugins directory.

## Conventions

The plugin discovers the fleet from Headlamp's configured clusters: the
context whose name ends in `-hub` is the hub, everything else is a member
(`src/fleet.ts`). Namespaces default to the `setup-fleet.sh` defaults
(`fleet-system`, `fleet-demo`).
