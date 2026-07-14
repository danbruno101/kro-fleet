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

// Resource classes and fleet-topology helpers shared by the views.
//
// Headlamp is pointed at ONE kubeconfig containing the hub and every member
// context (scripts/headlamp-kubeconfig.sh emits it). The hub is recognized by
// name; everything else is a member. The plugin never talks to the hub
// controller — it reads the same objects the controller writes.

import { makeCustomResourceClass } from '@kinvolk/headlamp-plugin/lib/k8s/crd';

// Must match scripts/setup-fleet.sh (WORKLOAD_NS / PREFIX defaults).
export const WORKLOAD_NAMESPACE = 'fleet-demo';
export const FLEET_NAMESPACE = 'fleet-system';
const HUB_SUFFIX = '-hub';

export const FleetGenAIService = makeCustomResourceClass({
  apiInfo: [{ group: 'fleet.kro.run', version: 'v1alpha1' }],
  kind: 'FleetGenAIService',
  pluralName: 'fleetgenaiservices',
  singularName: 'fleetgenaiservice',
  isNamespaced: true,
});

export const GenAIService = makeCustomResourceClass({
  apiInfo: [{ group: 'kro.run', version: 'v1alpha1' }],
  kind: 'GenAIService',
  pluralName: 'genaiservices',
  singularName: 'genaiservice',
  isNamespaced: true,
});

export const ClusterProfile = makeCustomResourceClass({
  apiInfo: [{ group: 'multicluster.x-k8s.io', version: 'v1alpha1' }],
  kind: 'ClusterProfile',
  pluralName: 'clusterprofiles',
  singularName: 'clusterprofile',
  isNamespaced: true,
});

export interface FleetTopology {
  hub: string | null;
  members: string[];
}

/** Split Headlamp's configured cluster names into hub + members. */
export function splitFleet(clusterNames: string[]): FleetTopology {
  const hub = clusterNames.find(name => name.endsWith(HUB_SUFFIX)) ?? null;
  return { hub, members: clusterNames.filter(name => name !== hub) };
}

/**
 * Map a ClusterProfile name (e.g. kro-fleet-member-1) to the Headlamp cluster
 * that serves it. kind kubeconfig contexts are prefixed (kind-<cluster>), so
 * match exact first, then by suffix.
 */
export function headlampClusterForProfile(
  profileName: string,
  clusterNames: string[]
): string | null {
  return (
    clusterNames.find(name => name === profileName) ??
    clusterNames.find(name => name.endsWith(profileName)) ??
    null
  );
}
