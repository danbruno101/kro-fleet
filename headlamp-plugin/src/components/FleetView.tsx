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

// The fleet view: the hub's FleetGenAIServices with their per-member
// status.clusters[], the ClusterProfile inventory, and the placed pods on
// every member (with log streaming) — one screen for "author once on the
// hub → running across the fleet".

import { K8s } from '@kinvolk/headlamp-plugin/lib';
import {
  SectionBox,
  SimpleTable,
  StatusLabel,
} from '@kinvolk/headlamp-plugin/lib/components/common';
import { useClustersConf } from '@kinvolk/headlamp-plugin/lib/k8s';
import { Button, Typography } from '@mui/material';
import React from 'react';
import {
  ClusterProfile,
  FLEET_NAMESPACE,
  FleetGenAIService,
  splitFleet,
  WORKLOAD_NAMESPACE,
} from '../fleet';
import { PodLogsDialog, PodLogsTarget } from './PodLogs';

function ReadyLabel({ ready, text }: { ready: boolean; text?: string }) {
  return <StatusLabel status={ready ? 'success' : 'error'}>{text ?? (ready ? 'Ready' : 'NotReady')}</StatusLabel>;
}

export default function FleetView() {
  const clustersConf = useClustersConf() || {};
  const { hub, members } = splitFleet(Object.keys(clustersConf));

  const [fleetServices] = FleetGenAIService.useList(hub ? { cluster: hub } : {});
  const [profiles] = ClusterProfile.useList(
    hub ? { cluster: hub, namespace: FLEET_NAMESPACE } : {}
  );
  const podsResult = K8s.ResourceClasses.Pod.useList({
    clusters: members,
    namespace: WORKLOAD_NAMESPACE,
  });
  // Read per-cluster results, NOT the aggregated items: live validation
  // showed the aggregate stays empty while any member hangs (e.g. a paused
  // kind node), which would blank the whole table. Per-cluster results let
  // reachable members render and unreachable ones surface as warnings.
  const clusterResults = ((podsResult as any).clusterResults ?? {}) as Record<
    string,
    { items: any[] | null; errors: Array<Error & { cluster?: string }> | null }
  >;
  const pods = members.flatMap(m => clusterResults[m]?.items ?? []);
  const memberErrors = members.flatMap(m =>
    (clusterResults[m]?.errors ?? []).map(err => ({ cluster: m, message: err.message }))
  );
  const [logsTarget, setLogsTarget] = React.useState<PodLogsTarget | null>(null);

  if (!hub) {
    return (
      <SectionBox title="KRO Fleet">
        <Typography>
          No hub cluster found. Point Headlamp at a kubeconfig that includes the hub context
          (a cluster whose name ends in <code>-hub</code>) and the member contexts — see
          scripts/headlamp-kubeconfig.sh.
        </Typography>
      </SectionBox>
    );
  }

  return (
    <>
      <SectionBox title={`FleetGenAIServices — hub: ${hub}`}>
        <SimpleTable
          columns={[
            { label: 'Name', getter: (fgs: any) => fgs.metadata.name },
            { label: 'Namespace', getter: (fgs: any) => fgs.metadata.namespace },
            {
              label: 'Placed / Ready',
              getter: (fgs: any) =>
                `${fgs.jsonData?.status?.summary?.placed ?? 0} / ${
                  fgs.jsonData?.status?.summary?.ready ?? 0
                }`,
            },
            {
              label: 'Rolled-up',
              getter: (fgs: any) => {
                const ready = (fgs.jsonData?.status?.conditions || []).find(
                  (c: any) => c.type === 'Ready'
                );
                return <ReadyLabel ready={ready?.status === 'True'} text={ready?.reason} />;
              },
            },
            {
              label: 'Per member',
              getter: (fgs: any) => (
                <>
                  {(fgs.jsonData?.status?.clusters || []).map((c: any) => (
                    <span key={c.name} style={{ marginRight: '0.5em' }} title={c.message || ''}>
                      <ReadyLabel ready={!!c.ready} text={c.name} />
                    </span>
                  ))}
                </>
              ),
            },
          ]}
          data={fleetServices || []}
          emptyMessage="No FleetGenAIService on the hub yet — apply examples/fleetgenaiservice-sample.yaml."
        />
      </SectionBox>

      <SectionBox title="Fleet inventory — ClusterProfiles">
        <SimpleTable
          columns={[
            { label: 'Member', getter: (p: any) => p.metadata.name },
            {
              label: 'Cloud',
              getter: (p: any) => p.metadata.labels?.['fleet.kro.run/cloud'] ?? '—',
            },
            { label: 'Tier', getter: (p: any) => p.metadata.labels?.tier ?? '—' },
            {
              label: 'Healthy',
              getter: (p: any) => {
                const healthy = (p.jsonData?.status?.conditions || []).find(
                  (c: any) => c.type === 'ControlPlaneHealthy'
                );
                return <ReadyLabel ready={healthy?.status === 'True'} text={healthy?.reason} />;
              },
            },
          ]}
          data={profiles || []}
          emptyMessage="No ClusterProfiles registered on the hub."
        />
      </SectionBox>

      <SectionBox title={`Placed workloads — pods in ${WORKLOAD_NAMESPACE} on every member`}>
        {memberErrors.map(err => (
          <Typography key={err.cluster} color="error" sx={{ mb: 1 }}>
            ⚠ {err.cluster} unreachable: {err.message}
          </Typography>
        ))}
        <SimpleTable
          columns={[
            { label: 'Cluster', getter: (pod: any) => pod.cluster },
            { label: 'Pod', getter: (pod: any) => pod.metadata.name },
            { label: 'Phase', getter: (pod: any) => pod.jsonData?.status?.phase ?? '?' },
            {
              label: 'Logs',
              getter: (pod: any) => (
                <Button
                  size="small"
                  variant="outlined"
                  onClick={() =>
                    setLogsTarget({
                      podName: pod.metadata.name,
                      namespace: pod.metadata.namespace,
                      cluster: pod.cluster,
                    })
                  }
                >
                  View
                </Button>
              ),
            },
          ]}
          data={pods || []}
          emptyMessage="No pods placed on the members yet."
        />
      </SectionBox>

      {logsTarget && <PodLogsDialog {...logsTarget} onClose={() => setLogsTarget(null)} />}
    </>
  );
}
