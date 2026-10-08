# Cluster properties used by the demo

Placement in the demo is a function of cluster properties: each member
advertises what it is and what it has, the hub mirrors that into its
`ClusterProfile`, and the demo scheduler reads the hub. kro-fleet itself
reads properties only to **enforce** `spec.placement.requirements.matchProperties`
(fail closed); it never selects clusters with them.

## Where properties live

| Layer | Object | Who writes it |
|---|---|---|
| Member cluster | `ClusterProperty` (About API, `about.k8s.io/v1beta1`, cluster-scoped, `spec.value`) — KEP-2149 | The platform (setup/provision scripts) for static facts; the demo **inventory agent** for computed ones |
| Hub | `ClusterProfile.status.properties[]` (`{name, value, lastObservedTime}`) — KEP-4322 | The demo inventory agent, playing the cluster-manager role: it mirrors every member `ClusterProperty` **as-is** (name and value), as KEP-4322 says a cluster manager SHOULD |

A real cluster manager (OCM, KubeFleet, GKE Fleet) that populates
`status.properties` replaces the agent without any change to kro-fleet or the
scheduler.

## Names

KEP-2149 reserves the `.k8s.io` and `.kubernetes.io` suffixes for Kubernetes
projects and asks everyone else to namespace their properties. The only
standard names are `cluster.clusterset.k8s.io` and `clusterset.k8s.io`.
Everything else below is a **demo vocabulary under `example.com`** — it is
deliberately not a kro vocabulary (one of the design principles is to never
invent a kro-specific property vocabulary), and nothing in kro-fleet depends
on these names: the scheduler's capacity property is a flag, and
`matchProperties` takes any name. Property names are object names, so they
cannot contain `/`; the node-label spellings are given for reference only.

| Property | Values | Source | Used by |
|---|---|---|---|
| `cluster.clusterset.k8s.io` | the member's name | setup / provision | standard identity (KEP-2149) |
| `cloud.example.com` | `eks` `gke` `aks` `kind` | setup / provision | display; the fleet RGD takes the cloud from the platform ConfigMap, not from here |
| `region.example.com` | e.g. `us-east-1` (cf. node label `topology.kubernetes.io/region`) | setup / provision | display, compliance framing |
| `accelerator.example.com` | `gpu` | setup / provision | the demo instance's hard requirement (`matchProperties`) |
| `gpus-total.example.com` | integer | **computed** by the inventory agent: node allocatable of the accelerator resource (`example.com/gpu` simulated, `nvidia.com/gpu` real) over schedulable nodes | display |
| `gpus-free.example.com` | integer | **computed**: total minus requests of non-terminal pods | the scheduler's capacity (one replica = one unit); the scheduler credits an instance's own current assignment back, so re-evaluation is stable |
| `cost-tier.example.com` | `1` (cheapest) … | setup / provision | `cheapest-first` / `spot-first` policies |
| `capacity-type.example.com` | `spot` `on-demand` | setup / provision | `spot-first` policy |
| `compliance.example.com` | e.g. `fedramp-high` | setup / provision, on the accredited member only | the compliance beat's hard requirement; never a soft preference |
| `draining.example.com` | `true` | the operator, when draining a cluster | the scheduler stops placing there and moves what it can; kro-fleet unplaces |

Health is not a property: `ControlPlaneHealthy` is a `ClusterProfile`
condition (KEP-4322), asserted by the scripts in place of a cluster-manager
agent, and enforced by both the scheduler (exclude) and kro-fleet (treat as
unreachable).

## Trust

A mislabelled cluster is a compliance breach, not a scheduling mistake
(`docs/design/fleet-scoped-kro.md`, "Who is trusted to assert cluster
properties?"). The demo asserts properties from the provisioning scripts and
offers no provenance; that question is open with SIG-Multicluster and is
ledgered in `docs/KEP-GAP.md`.
