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

// Package properties names the cluster properties the demo uses. The only
// standard names (KEP-2149) are cluster.clusterset.k8s.io and
// clusterset.k8s.io; everything else is a demo vocabulary under example.com,
// deliberately NOT a kro vocabulary — see docs/properties.md.
package properties

const (
	// ClusterID is the well-known KEP-2149 cluster identity property.
	ClusterID = "cluster.clusterset.k8s.io"

	// Cloud is the cloud provider persona (eks | gke | aks | kind).
	Cloud = "cloud.example.com"
	// Region is the provider region (e.g. us-east-1).
	Region = "region.example.com"
	// Accelerator says what accelerator class the cluster offers ("gpu").
	Accelerator = "accelerator.example.com"
	// GPUsTotal / GPUsFree are computed by the inventory agent from node
	// allocatable and pod requests of the accelerator resource.
	GPUsTotal = "gpus-total.example.com"
	GPUsFree  = "gpus-free.example.com"
	// CostTier orders clusters by price: lower is cheaper ("1", "2", "3").
	CostTier = "cost-tier.example.com"
	// CapacityType is "spot" or "on-demand".
	CapacityType = "capacity-type.example.com"
	// Compliance is the regime the cluster is accredited for (e.g.
	// "fedramp-high"); a hard requirement in the compliance beat.
	Compliance = "compliance.example.com"
	// Draining is "true" while the platform is draining the cluster; the
	// scheduler stops placing there and moves what it can.
	Draining = "draining.example.com"
)
