kro-fleet
A thin proof-of-concept for fleet-scoped KRO objects: author one object on a hub
cluster; it is placed across many member clusters (and clouds), with per-member
status aggregated back on the hub.
This is the reference implementation that backs the design proposal
docs/proposals/KEP-kro-multicluster.md
— “Native Multi-Cluster Mode for KRO.”
Status: discussion-stage PoC. Built on kro v1alpha1 and SIG-Multicluster
primitives (ClusterProfile / KEP-4322, multicluster-runtime). Not for
production. Runs entirely on kind — no cloud account, no GPU.
The idea
KRO lets a platform team define a custom
API (a ResourceGraphDefinition) that expands a short developer instance into a
reconciled graph of Kubernetes resources — with no custom Go controller. The
sister project kro-genaiops-demo
proves that graph is portable: the same GenAIService runs unchanged on GKE, AKS,
and EKS.
Its honest limit: ownership is cluster-local. ownerReferences never cross a
cluster boundary, so “the same workload on N clusters” is N independent objects,
applied N times, with N control loops and no aggregated view.
kro-fleet closes that gap: one placement-enabled object on a hub cluster →
placed onto the matching member clusters → status aggregated back on the hub.
Change it once, apply it once, it disperses.
Scope (important, and settled)
	•	This PoC does NOT fork or modify kro. It runs stock kro on each member
cluster (which does the local graph expansion, exactly as in the demo) and adds
only a hub-side fleet placement controller.
	•	It is built on existing SIG-Multicluster standards — not a new propagation
engine:
	•	ClusterProfile / Cluster Inventory API
(KEP-4322) for the fleet registry + member credentials.
	•	multicluster-runtime
for reconciling across a dynamic fleet.
	•	The “native mode inside kro” (expand-on-hub, one control loop) is future work,
tracked honestly in docs/KEP-GAP.md.
Architecture (thin PoC)