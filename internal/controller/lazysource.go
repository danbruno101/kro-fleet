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

package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"

	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"
)

// kindNotServedPoll is how often a member is re-checked for a kind it does
// not serve yet.
const kindNotServedPoll = 10 * time.Second

// LazyMemberKind is a multicluster-runtime source for a kind a member may
// not serve YET. multicluster-runtime's Kind source fails a member's
// engagement outright when the kind has no REST mapping there, and both
// member-side kinds this controller watches are kro-generated: the
// ResourceGraphDefinition CRD comes with kro, and the GenAIService CRD only
// exists once a distributed definition is Active — i.e. AFTER the member
// engaged. This source lets the member engage and starts the real watch as
// soon as the kind appears (the same reason KREP-013 registers spoke
// informers without waiting for sync). Until then the member is reachable
// and reconciles run on the periodic resync.
func LazyMemberKind(obj client.Object, handler mchandler.TypedEventHandlerFunc[client.Object, mcreconcile.Request]) mcsource.TypedSource[client.Object, mcreconcile.Request] {
	return &lazyKind{obj: obj, inner: mcsource.Kind(obj, handler)}
}

type lazyKind struct {
	obj   client.Object
	inner mcsource.SyncingSource[client.Object]
}

// ForCluster wraps the inner per-cluster source.
func (l *lazyKind) ForCluster(name multicluster.ClusterName, cl cluster.Cluster) (crsource.TypedSource[mcreconcile.Request], bool, error) {
	src, engage, err := l.inner.ForCluster(name, cl)
	if err != nil {
		return nil, false, err
	}
	return &lazyClusterKind{name: name, cl: cl, obj: l.obj.DeepCopyObject().(client.Object), inner: src}, engage, nil
}

type lazyClusterKind struct {
	name  multicluster.ClusterName
	cl    cluster.Cluster
	obj   client.Object
	inner crsource.TypedSource[mcreconcile.Request]
}

// Start starts the real watch now when the member serves the kind, else
// polls for it in the background and starts the watch when it appears.
// It never fails the engagement for a missing kind.
func (s *lazyClusterKind) Start(ctx context.Context, q workqueue.TypedRateLimitingInterface[mcreconcile.Request]) error {
	log := ctrllog.FromContext(ctx).WithValues("cluster", s.name, "kind", s.obj.GetObjectKind().GroupVersionKind().Kind)
	served, err := s.served(ctx)
	if err != nil {
		return err
	}
	if served {
		return s.inner.Start(ctx, q)
	}
	log.Info("member does not serve the kind yet; engaging without it and watching for it to appear")
	go func() {
		_ = wait.PollUntilContextCancel(ctx, kindNotServedPoll, false, func(ctx context.Context) (bool, error) {
			served, err := s.served(ctx)
			if err != nil {
				log.Error(err, "failed to check whether the member serves the kind")
				return false, nil
			}
			if !served {
				return false, nil
			}
			if err := s.inner.Start(ctx, q); err != nil {
				log.Error(err, "failed to start the watch once the kind appeared")
				return false, nil
			}
			log.Info("member now serves the kind; watch started")
			return true, nil
		})
	}()
	return nil
}

// served reports whether the member's API currently maps the kind. The
// cluster's lazy REST mapper re-discovers on a miss, so a CRD created after
// engagement is seen on a later poll.
func (s *lazyClusterKind) served(ctx context.Context) (bool, error) {
	_, err := s.cl.GetCache().GetInformer(ctx, s.obj, cache.BlockUntilSynced(false))
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}
