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

// Streams one pod's logs from a member cluster, following the pattern of the
// official Kubeflow Headlamp plugin (Pod.useGet with {cluster} + getLogs with
// follow), minus the xterm dependency — a <pre> is enough for the demo.

import { K8s } from '@kinvolk/headlamp-plugin/lib';
import { Dialog, DialogContent, DialogTitle } from '@mui/material';
import React from 'react';

export interface PodLogsTarget {
  podName: string;
  namespace: string;
  cluster: string;
}

export function PodLogsDialog(props: PodLogsTarget & { onClose: () => void }) {
  const { podName, namespace, cluster, onClose } = props;
  const [pod] = K8s.ResourceClasses.Pod.useGet(podName, namespace, { cluster });
  const [lines, setLines] = React.useState<string[]>([]);
  const endRef = React.useRef<HTMLDivElement | null>(null);

  React.useEffect(() => {
    if (!pod) {
      return;
    }
    const containerName = (pod as any).spec?.containers?.[0]?.name;
    if (!containerName) {
      return;
    }
    setLines([]);
    const cancel = (pod as any).getLogs(
      containerName,
      ({ logs }: { logs: string[] }) => setLines([...logs]),
      { tailLines: 200, follow: true, showTimestamps: false }
    );
    return () => {
      // getLogs returns a cancel function, possibly wrapped in a promise.
      Promise.resolve(cancel).then(c => typeof c === 'function' && c());
    };
  }, [pod]);

  React.useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: 'auto' });
  }, [lines]);

  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="lg">
      <DialogTitle>
        {podName} — {cluster}
      </DialogTitle>
      <DialogContent>
        <pre
          style={{
            background: '#111',
            color: '#eee',
            padding: '1em',
            maxHeight: '60vh',
            overflow: 'auto',
            fontSize: '0.8rem',
          }}
        >
          {lines.length > 0 ? lines.join('') : 'Waiting for logs…'}
          <div ref={endRef} />
        </pre>
      </DialogContent>
    </Dialog>
  );
}
