package httpapi_test

import (
	"context"
	"errors"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type countingRegistry struct {
	p clusterstate.Provider
	n atomic.Int32
}

func (r *countingRegistry) ForScreen(context.Context, clusterstate.ScreenRequest) (clusterstate.Provider, error) {
	r.n.Add(1)
	return r.p, nil
}
func (r *countingRegistry) Ready() bool { return true }
func TestScreenProviderResolvedOnce(t *testing.T) {
	var reg *countingRegistry
	f := newFixture(t, withClusterWideScope, func(d *httpapi.Deps) { reg = &countingRegistry{p: d.Store}; d.ProviderRegistry = reg })
	var out any
	rec := f.get(t, "/api/v1/clusters/"+testcluster.ClusterID+"/overview?range=1h", &out)
	if rec.Code != 200 || reg.n.Load() != 1 {
		t.Fatalf("status=%d calls=%d", rec.Code, reg.n.Load())
	}
}
func TestUnauthorizedRejectedBeforeProvider(t *testing.T) {
	var reg *countingRegistry
	f := newFixture(t, func(d *httpapi.Deps) { reg = &countingRegistry{p: d.Store}; d.ProviderRegistry = reg })
	rec := f.get(t, "/api/v1/clusters/unknown/overview?range=1h", nil)
	if rec.Code != 403 || reg.n.Load() != 0 {
		t.Fatalf("status=%d calls=%d", rec.Code, reg.n.Load())
	}
}

type freshnessProvider struct {
	clusterstate.Provider
	stale    bool
	observed time.Time
}

func (p freshnessProvider) HasSynced() bool       { return !p.stale }
func (p freshnessProvider) ObservedAt() time.Time { return p.observed }

type freshnessRegistry struct {
	provider clusterstate.Provider
	state    atomic.Int32 // 0=fresh, 1=stale, 2=expired
}

func (r *freshnessRegistry) ForScreen(_ context.Context, req clusterstate.ScreenRequest) (clusterstate.Provider, error) {
	state := r.state.Load()
	if req.ClusterID == "a" && state == 2 {
		return nil, errors.New("cluster unavailable")
	}
	return freshnessProvider{Provider: r.provider, stale: req.ClusterID == "a" && state == 1, observed: time.Unix(1_700_000_000, 0).UTC()}, nil
}
func (*freshnessRegistry) Ready() bool { return true }

func TestHTTPFreshStaleExpiredClusterIsolation(t *testing.T) {
	var reg *freshnessRegistry
	f := newFixture(t, func(d *httpapi.Deps) {
		reg = &freshnessRegistry{provider: d.Store}
		d.ProviderRegistry = reg
		d.Resolver = scope.Static{S: scope.Scope{Clusters: []scope.Cluster{{ID: "a", Name: "A", All: true}, {ID: "b", Name: "B", All: true}}}}
	})
	get := func(cluster string) (contract.ClusterOverviewResponse, int) {
		var out contract.ClusterOverviewResponse
		rangeValue := []string{"1h", "1d", "7d"}[reg.state.Load()]
		rec := f.get(t, "/api/v1/clusters/"+cluster+"/overview?range="+rangeValue, &out)
		return out, rec.Code
	}
	fresh, code := get("a")
	if code != http.StatusOK || fresh.Pods.Status != contract.StatusOK || fresh.Pods.ObservedAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("fresh code=%d section=%+v", code, fresh.Pods)
	}
	reg.state.Store(1)
	stale, code := get("a")
	if code != http.StatusOK || stale.Pods.Status != contract.StatusDegraded || stale.Pods.ObservedAt != fresh.Pods.ObservedAt || !reflect.DeepEqual(stale.Pods.Data, fresh.Pods.Data) {
		t.Fatalf("stale code=%d section=%+v", code, stale.Pods)
	}
	reg.state.Store(2)
	if _, code = get("a"); code != http.StatusServiceUnavailable {
		t.Fatalf("expired status=%d", code)
	}
	if b, bCode := get("b"); bCode != http.StatusOK || b.Pods.Status != contract.StatusOK {
		t.Fatalf("B status=%d section=%+v", bCode, b.Pods)
	}
	if rec := f.get(t, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("ready status=%d", rec.Code)
	}
}
