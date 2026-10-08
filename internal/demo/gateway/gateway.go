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

// Package gateway is the DEMO-ONLY single entry point: one URL on the hub
// that spreads requests across the members' own cloud load balancers,
// weighted by each member's ready replicas, so an audience can hit one
// address and see answers come back from every cloud. It reads nothing but
// the FleetGenAIService status the fleet controller already aggregates
// (status.clusters[].endpoint / readyReplicas). Weighted DNS would do the
// same job without this process; MCS-API is explicitly out of scope.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	fleetv1alpha1 "github.com/danbruno101/kro-fleet/api/v1alpha1"
)

// ClusterHeader names the member that answered a proxied request.
const ClusterHeader = "X-Fleet-Cluster"

// Gateway proxies to the members of one FleetGenAIService.
type Gateway struct {
	Manager   mcmanager.Manager
	Listen    string
	Namespace string
	Instance  string

	mu        sync.Mutex
	upstreams []*upstream
}

type upstream struct {
	Cluster  string   `json:"cluster"`
	Endpoint string   `json:"endpoint"`
	Weight   int      `json:"readyReplicas"`
	url      *url.URL // not serialized
	current  int
}

// SetupWithManager watches the instance on the hub and serves HTTP as a
// manager runnable.
func (g *Gateway) SetupWithManager(mgr mcmanager.Manager) error {
	g.Manager = mgr
	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("demo-gateway").
		For(&fleetv1alpha1.FleetGenAIService{},
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Complete(g); err != nil {
		return err
	}
	return mgr.GetLocalManager().Add(manager.RunnableFunc(g.serve))
}

// Reconcile refreshes the upstream set from the instance's status.
func (g *Gateway) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	if req.Namespace != g.Namespace || req.Name != g.Instance {
		return ctrl.Result{}, nil
	}
	fgs := &fleetv1alpha1.FleetGenAIService{}
	if err := g.Manager.GetLocalManager().GetClient().Get(ctx, req.NamespacedName, fgs); err != nil {
		if client.IgnoreNotFound(err) == nil {
			g.set(nil)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	var ups []*upstream
	for _, c := range fgs.Status.Clusters {
		if c.Endpoint == "" || c.ReadyReplicas <= 0 {
			continue
		}
		u, err := url.Parse("http://" + net.JoinHostPort(c.Endpoint, "80"))
		if err != nil {
			continue
		}
		ups = append(ups, &upstream{Cluster: c.Name, Endpoint: c.Endpoint, Weight: int(c.ReadyReplicas), url: u})
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].Cluster < ups[j].Cluster })
	g.set(ups)
	ctrllog.FromContext(ctx).Info("gateway upstreams refreshed", "count", len(ups))
	return ctrl.Result{}, nil
}

func (g *Gateway) set(ups []*upstream) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.upstreams = ups
}

// pick is smooth weighted round-robin (nginx's algorithm): members with
// more ready replicas get proportionally more requests, evenly interleaved.
func (g *Gateway) pick() *upstream {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.upstreams) == 0 {
		return nil
	}
	total := 0
	var best *upstream
	for _, u := range g.upstreams {
		u.current += u.Weight
		total += u.Weight
		if best == nil || u.current > best.current {
			best = u
		}
	}
	best.current -= total
	return best
}

func (g *Gateway) serve(ctx context.Context) error {
	log := ctrllog.Log.WithName("gateway")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/fleet", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"instance": g.Namespace + "/" + g.Instance, "upstreams": g.upstreams})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		up := g.pick()
		if up == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no member with a ready endpoint", "instance": g.Namespace + "/" + g.Instance})
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(up.url)
		proxy.Transport = &http.Transport{ResponseHeaderTimeout: 10 * time.Second}
		proxy.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set(ClusterHeader, up.Cluster)
			return nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set(ClusterHeader, up.Cluster)
			http.Error(w, fmt.Sprintf("upstream %s (%s) failed: %v", up.Cluster, up.Endpoint, err), http.StatusBadGateway)
		}
		proxy.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: g.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("gateway listening", "addr", g.Listen, "instance", g.Namespace+"/"+g.Instance)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
