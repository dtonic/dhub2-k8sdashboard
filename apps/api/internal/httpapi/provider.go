package httpapi

import (
	"context"
	"net/http"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/timerange"
)

type providerKey struct{}
type localProviderRegistry struct{ provider clusterstate.Provider }

func (r localProviderRegistry) ForScreen(context.Context, clusterstate.ScreenRequest) (clusterstate.Provider, error) {
	return r.provider, nil
}
func (r localProviderRegistry) Ready() bool { return r.provider != nil && r.provider.HasSynced() }
func providerFrom(ctx context.Context, fallback clusterstate.Provider) clusterstate.Provider {
	if p, ok := ctx.Value(providerKey{}).(clusterstate.Provider); ok {
		return p
	}
	return fallback
}
func (s *Server) withProvider(screen string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("clusterId")
		c, ok := scope.From(r.Context()).Cluster(id)
		if !ok || !c.Accessible() {
			writeError(w, r, http.StatusForbidden, "forbidden", "cluster access denied")
			return
		}
		requested := r.PathValue("namespace")
		if requested == "" {
			if screen == "overview" {
				requested = r.URL.Query().Get("namespace")
			} else {
				requested = r.URL.Query().Get("ns")
			}
		}
		f, e := namespaceFilter(c, requested)
		if e != nil {
			writeError(w, r, http.StatusForbidden, "forbidden", "namespace access denied")
			return
		}
		win, e := timerange.Parse(r.URL.Query().Get("range"), r.URL.Query().Get("from"), r.URL.Query().Get("to"), s.deps.Now())
		if e != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_range", "invalid time range")
			return
		}
		req := clusterstate.ScreenRequest{ClusterID: id, Screen: screen, RequestedNamespace: requested, Namespaces: f, From: win.From, EventLimit: eventLimit, UnhealthyLimit: unhealthyLimit, EntityUID: r.URL.Query().Get("uid"), Kind: r.PathValue("kind"), Name: r.PathValue("name")}
		p, e := s.deps.ProviderRegistry.ForScreen(r.Context(), req)
		if e != nil {
			writeError(w, r, http.StatusServiceUnavailable, "cluster_unavailable", "cluster state unavailable")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), providerKey{}, p)))
	}
}
