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

import {
  registerMapSource,
  registerRoute,
  registerSidebarEntry,
} from '@kinvolk/headlamp-plugin/lib';
import FleetView from './components/FleetView';
import { fleetMapSource } from './fleetMap';

registerSidebarEntry({
  parent: '',
  name: 'kro-fleet',
  label: 'KRO Fleet',
  url: '/kro-fleet',
  icon: 'mdi:transit-connection-variant',
});

registerRoute({
  path: '/kro-fleet',
  sidebar: 'kro-fleet',
  name: 'kro-fleet',
  exact: true,
  component: () => <FleetView />,
});

registerMapSource(fleetMapSource);
