/*
Copyright 2026 The kro-fleet Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The cross-cluster object graph, fed into Headlamp's resource-map view via
// registerMapSource (same mechanism as the official Cluster API plugin):
//
//   FleetGenAIService (hub)
//     └─ placed GenAIService (per member)          edge: "placed on <member>"
//          └─ kro-expanded children on that member  edge: ownerReferences
//
// Node ids are namespaced by cluster (uids are only unique per cluster).

import { Icon } from '@iconify/react';
import { K8s } from '@kinvolk/headlamp-plugin/lib';
import { useClustersConf } from '@kinvolk/headlamp-plugin/lib/k8s';
import { useMemo } from 'react';
import { FleetGenAIService, GenAIService, splitFleet, WORKLOAD_NAMESPACE } from './fleet';

function nodeId(cluster: string, uid: string) {
  return `kro-fleet:${cluster}:${uid}`;
}

export const fleetMapSource = {
  id: 'kro-fleet',
  label: 'KRO Fleet',
  icon: (
    <Icon icon="mdi:transit-connection-variant" width="100%" height="100%" color="#326ce5" />
  ),
  useData() {
    const clustersConf = useClustersConf() || {};
    const { hub, members } = splitFleet(Object.keys(clustersConf));

    const [fleetServices] = FleetGenAIService.useList(hub ? { cluster: hub } : {});
    const [placed] = GenAIService.useList({
      clusters: members,
      namespace: WORKLOAD_NAMESPACE,
    });
    const [deployments] = K8s.ResourceClasses.Deployment.useList({
      clusters: members,
      namespace: WORKLOAD_NAMESPACE,
    });
    const [services] = K8s.ResourceClasses.Service.useList({
      clusters: members,
      namespace: WORKLOAD_NAMESPACE,
    });
    const [pvcs] = K8s.ResourceClasses.PersistentVolumeClaim.useList({
      clusters: members,
      namespace: WORKLOAD_NAMESPACE,
    });

    return useMemo(() => {
      if (!hub) {
        return { nodes: [], edges: [] };
      }

      const nodes: any[] = [];
      const edges: any[] = [];

      for (const fgs of fleetServices || []) {
        nodes.push({ id: nodeId(hub, fgs.metadata.uid), kubeObject: fgs });
      }

      // Hub object -> its placed copy on each member (same namespace/name,
      // by the controller's contract).
      for (const copy of placed || []) {
        const cluster = (copy as any).cluster;
        nodes.push({ id: nodeId(cluster, copy.metadata.uid), kubeObject: copy });
        const owner = (fleetServices || []).find(
          fgs =>
            fgs.metadata.name === copy.metadata.name &&
            fgs.metadata.namespace === copy.metadata.namespace
        );
        if (owner) {
          edges.push({
            id: `${nodeId(hub, owner.metadata.uid)}-${nodeId(cluster, copy.metadata.uid)}`,
            source: nodeId(hub, owner.metadata.uid),
            target: nodeId(cluster, copy.metadata.uid),
            label: `placed on ${cluster}`,
          });
        }
      }

      // Placed copy -> the children stock kro expanded on that member,
      // linked by same-cluster ownerReferences (the CAPI plugin pattern).
      const children = [...(deployments || []), ...(services || []), ...(pvcs || [])];
      for (const child of children) {
        const cluster = (child as any).cluster;
        const ownerRef = (child.metadata.ownerReferences || []).find(ref =>
          (placed || []).some(
            p => (p as any).cluster === cluster && p.metadata.uid === ref.uid
          )
        );
        if (!ownerRef) {
          continue;
        }
        nodes.push({ id: nodeId(cluster, child.metadata.uid), kubeObject: child });
        edges.push({
          id: `${nodeId(cluster, ownerRef.uid)}-${nodeId(cluster, child.metadata.uid)}`,
          source: nodeId(cluster, ownerRef.uid),
          target: nodeId(cluster, child.metadata.uid),
          label: `owned by ${ownerRef.kind}`,
        });
      }

      return { nodes, edges };
    }, [hub, fleetServices, placed, deployments, services, pvcs]);
  },
};
