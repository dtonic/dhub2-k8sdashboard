package httpapi_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

func TestGuardUsesAuthorizedClusterOrSingleDeniedPartition(t *testing.T) {
	guard := queryprotect.New(queryprotect.Config{
		UserRate: 0.000001, DashboardRate: 0.000001,
		UserBurst: 1, DashboardBurst: 1,
		UserConcurrent: 1, DashboardConcurrent: 1,
		MaxIdentities: 16, IdleTTL: time.Hour,
		Now: func() time.Time { return testcluster.Now },
	}, queryprotect.NewMetrics())
	f := newFixture(t, func(d *httpapi.Deps) {
		d.Guard = guard
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		d.Resolver = scope.Static{S: scope.Scope{Subject: "viewer", Clusters: []scope.Cluster{
			{ID: testcluster.ClusterID, Name: "a", All: true},
			{ID: "cluster-b", Name: "b", All: true},
		}}}
	})
	request := func(cluster string) int {
		recorder := httptest.NewRecorder()
		path := "/api/v1/clusters/" + cluster + "/overview?range=1h"
		f.srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder.Code
	}
	if status := request(testcluster.ClusterID); status == http.StatusTooManyRequests {
		t.Fatalf("cluster A first request was rate limited: %d", status)
	}
	if status := request("cluster-b"); status == http.StatusTooManyRequests {
		t.Fatalf("cluster B did not have an isolated authorized bucket: %d", status)
	}
	if status := request(testcluster.ClusterID); status != http.StatusTooManyRequests {
		t.Fatalf("cluster A rate limit was bypassed: %d", status)
	}

	deniedAllowed := 0
	for i := 0; i < 10_000; i++ {
		if status := request(fmt.Sprintf("unknown-%05d", i)); status != http.StatusTooManyRequests {
			deniedAllowed++
		}
	}
	if deniedAllowed != 1 {
		t.Fatalf("random denied cluster IDs reset the rate bucket: allowed=%d want 1", deniedAllowed)
	}
	users, dashboards := guard.IdentityCounts()
	if users != 3 || dashboards != 3 {
		t.Fatalf("guard cardinality grew with raw path IDs: users=%d dashboards=%d", users, dashboards)
	}
}
